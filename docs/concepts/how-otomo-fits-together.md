# How otomo fits together

Otomo isn't one big program. It's about a dozen small ones, called **services**, and each
has one job. This page introduces every service, shows which ones your game talks to, and
follows a request from your game through otomo and back.

## Why many small services?

Splitting a backend into services means each one is simple, can be restarted or updated on
its own, and can't break the others. If the service that handles friend lists crashes,
players can still download updates and log in. The cost is that the services have to talk
to each other over the network, which is why earlier pages covered addresses and HTTP.

## The map

```
                         THE INTERNET
                              │
        ┌─────────────────────┼─────────────────────────┐
        │ HTTPS (TCP 443)     │                         │ UDP 27000
        ▼                     │                         ▼
  ┌──────────┐                │                 ┌────────────────┐
  │   Edge   │  the front door│                 │ Gameplay Proxy │  the door to matches
  └────┬─────┘                │                 └───────┬────────┘
       │                      │                         │
       ▼                      │                         ▼
  ┌──────────┐                │                 ┌────────────────┐
  │ Gateway  │  checks passes,│                 │  Game servers  │  your Godot server
  └────┬─────┘  routes        │                 │   gs-1, gs-2   │  program
       │                      │                 └───────┬────────┘
  ┌────┴──────┬─────────┬─────┴─────┐                   │ "I'm here", "I'm free"
  ▼           ▼         ▼           ▼                   ▼
┌─────┐   ┌──────┐  ┌─────────┐  ┌──────────────────────────┐
│Patch│   │ Auth │  │ Session │─▶│        Allocator         │
└──┬──┘   └──────┘  └─────────┘  └──────────────────────────┘
   │
   ▼                  STAFF ONLY (not on the internet)
┌──────────┐  ┌──────────────────────────────────────────────────┐
│  Config  │◀─│ Admin website ─ admin gateway ─ admin-auth       │
└──────────┘  │ Dashboard (metrics, logs, alerts)                │
              └──────────────────────────────────────────────────┘

  Underneath everything: Postgres (the database) and Valkey (fast shared memory)
```

## The services your game talks to

Your game only ever talks to two addresses: the **gateway**, over HTTPS, and the
**Gameplay Proxy**, over UDP. It never contacts any other service directly.

### Edge

The **edge** is the front door for everything on `https://play.example.com`. It handles
TLS encryption (the `s` in `https`, see [How games go online](how-games-go-online.md)) and
passes each request inward according to the start of its path: `/patch/`, `/auth/` and
`/api/player/` go to the gateway, `/docs/` to this wiki, and `/` to the game's website.

### Gateway

The **gateway** is the security desk behind the front door. For every request it:

1. **Checks the login pass**, when the path needs one. A request to `/api/player/…` without
   a valid access token is turned away with `401` before it reaches anything else.
2. **Limits the rate**: each player may send about 20 requests a second. A game (or an
   attacker) sending more gets `429 Too Many Requests`.
3. **Routes** the request to the right service, based on its path.

### Patch

**Patch** answers "what's the newest content?" and hands out the files. It's read-only:
it never changes anything, it just serves what your team published. It's also the only
service your game can use without logging in, because the game must be able to update
before it knows anything else. See [Content and patching](content-and-patching.md).

### Auth

**Auth** turns a device ID into login tokens, and swaps old tokens for new ones. See
[Logins and tokens](logins-and-tokens.md).

### Session

**Session** is everything social and everything "between matches":

- the player's **profile** (display name such as `Mei#4417`);
- **presence** (who's online);
- **friends** and **blocks**;
- **parties**, which double as the pre-match **lobby** (settings, ready checks, launch);
- **events**: a way for the server to tell the game "something changed" (see below).

### Gameplay Proxy

The **Gameplay Proxy** is the only way into a match. It checks each player's join ticket
and then passes their game traffic to the right game server, and the replies back. The game
servers themselves are never reachable from the internet. See
[Parties and matches](parties-and-matches.md).

## The services your game never sees

### Allocator

The **Allocator** keeps the list of game servers and knows which are free. When a party
launches, Session asks it for a server; the Allocator reserves one and writes the join
tickets. It's **internal only**: nothing on the internet can reach it.

### Game servers

Your own Godot game, exported as a **dedicated server**: a version with no window that runs
matches. Otomo keeps a small fixed set of them running (`gs-1`, `gs-2`). Each tells the
Allocator "I'm here" when it starts, confirms "still alive" every 5 seconds, and reports
when its match is over. See [Run a new game server build](../guide/game-servers.md).

### Config and the admin website

**Config** is where your team *writes* content: settings documents, content packs and
releases. Staff use it through the **admin website**. When a release is published,
Config tells Patch, and Patch starts serving it to players. See
[Use the admin website](../guide/admin-website.md).

### Admin gateway and admin-auth

The admin website has its own gateway and its own login service (**admin-auth**), with
staff accounts, roles and two-factor sign-in. They're deliberately separate from the
player side, and not reachable from the internet at all.

### Dashboard

The **Dashboard** collects numbers (how many players are online, how many game servers are
free, how fast each service answers), log messages from every service, and **alerts** when
something goes wrong ("no free game servers"). Staff see it on the admin website's
**Overview** page.

### Postgres and Valkey

Every service that remembers something stores it in **Postgres**, a database: a program that
stores information in tables and keeps it safe across restarts. Each service has its own
database and can't read the others'. **Valkey** is a much faster, memory-based store that
Session uses for things that change constantly, like who's online right now.

## Events: how the server tells the game things

HTTP is question-and-answer: the server can only *answer*, never start a conversation. So
how does your game learn that a friend came online, or that the party leader pressed Play?

The answer is a trick called **long polling**. The game asks Session "anything new since
event 41?", and if there's nothing, Session **holds the question open** for up to 25 seconds
instead of answering "no". The moment something happens, Session answers with it. The game
then immediately asks again. To the player it feels instant, and it works through any
firewall because it's plain HTTP.

## Following one request

Here's the journey of a single request, when a player creates a party:

1. The game sends `POST /api/player/session/party` with its access token, to
   `https://play.example.com`.
2. The **edge** decrypts it and, because the path starts with `/api/player/`, passes it to
   the gateway.
3. The **gateway** checks the token's signature and expiry, checks the rate limit, and
   passes it to Session.
4. **Session** checks the token again (it never trusts anyone blindly), sees the player
   isn't in a party, creates one in its **Postgres** database, and answers `201 Created`
   with the new party as JSON.
5. The answer travels back the same way, and the SDK hands your code the party.

The whole trip usually takes a few tens of milliseconds, most of it the distance to the
server.

## Next

Continue with [Content and patching](content-and-patching.md), the part of otomo that game
teams use most.
