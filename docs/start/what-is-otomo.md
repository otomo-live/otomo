# What is otomo?

This page explains, without any jargon, what otomo does for your game and which parts of it
you'll actually touch. It takes about ten minutes to read.

## The short version

When you play an online game, a lot happens before you ever see the first level:

1. The game checks whether there's an update, and downloads it.
2. It logs you in, so the game knows it's *you* and not someone else.
3. It shows your friends, and lets you form a party with them.
4. When the party leader presses **Play**, something finds a free game server, and
   everyone in the party joins it.

None of this is gameplay, but every online game needs it. **Otomo does all four steps for
you.** Your game only has to ask ("is there an update?", "log me in", "start the match"),
and otomo does the rest.

The word for this invisible part of a game is the **backend**. The part the player sees
(your Godot game) is the **client**. Otomo is a backend.

## A story: one player's evening

Here's what happens when a player named Mei opens a game built with otomo. You don't need
to remember the names in bold yet; each has its own page later.

**Mei starts the game.** Before the main menu appears, the game asks otomo's **Patch**
service: "What's the newest content?" Patch answers with a list of files. The game notices
two of them are new (a balance change your designer made this morning, and a new event
banner) and downloads them. A progress bar fills up. This is called **patching**.

**The main menu appears.** Mei has never made an account; she didn't need to. The first
time the game ran, it made up a long random ID for her computer and saved it. Now it shows
that ID to otomo's **Auth** service, which answers with a **token**: a digital pass that
says "this is Mei's account, valid for 15 minutes". The game attaches that pass to
everything it asks from now on.

**Mei joins her friend's party.** The game asks otomo's **Session** service for Mei's
friends list and party. Session also keeps the game informed ("your friend is online now",
"the party leader changed the difficulty").

**Her friend presses Play.** Session asks otomo's **Allocator**, "Which game server is
free?" The Allocator picks one, reserves it for this party, and writes each player a
**join ticket**: a pass that's valid for one minute and only for that server.

**Mei's game connects to the match.** It sends its ticket to otomo's **Gameplay Proxy**,
the one public door to all the game servers. The proxy checks the ticket and opens the
door. From now on, Mei's game talks to the game server through the proxy, exactly as it
would talk to any Godot multiplayer server.

**The match ends.** The game server tells the Allocator it's done and restarts itself with
a fresh world, ready for the next party. Mei's game goes back to the party screen.

## What you'll touch, depending on your job

| If you are… | You'll use | You can ignore |
|---|---|---|
| **A gameplay or UI programmer** | The **Godot SDK**: an addon in your project with a few functions and signals. See [Install the SDK](../guide/install-the-sdk.md) | Everything on the server |
| **A designer or live-ops person** | The **admin website**, to publish balance changes, events and content packs. See [Use the admin website](../guide/admin-website.md) | The code |
| **The person who puts game servers online** | A few commands to upload your server build. See [Run a new game server build](../guide/game-servers.md) | Most of otomo's internals |
| **Someone running otomo itself** | The server machine, Docker and the deploy scripts. See [Deploy otomo on a VM](deploy-on-a-vm.md) | — |

## What otomo is *not*

- **Not a game engine.** Your gameplay is written in Godot as usual. Otomo never runs your
  game's logic, except by starting your game server program.
- **Not a multiplayer library.** In a match, your game uses Godot's own multiplayer
  (ENet). Otomo only gets the player to the right game server and checks they're allowed
  in.
- **Not tied to one game.** Otomo contains no game content. Everything specific to your
  game (content, rules, the game server) is something you provide.
- **Not a way to update your code.** Otomo can deliver new *content* (numbers, text,
  images, levels) without a new build. It can't change your C# code: players need a new
  build for that. [Content and patching](../concepts/content-and-patching.md) explains why.

## The pieces, in one picture

```
             Your Godot game (the client)
                         │
                         │  1. asks for updates, logs in, parties
                         ▼
     ┌──────────────── otomo ────────────────────────────────┐
     │   Patch      Auth      Session      Allocator         │
     │  (updates)  (logins)  (friends,     (finds game       │
     │                        parties)      servers)         │
     │                                                       │
     │   Admin website: where your team publishes content    │
     └───────────────────────────────────────────────────────┘
                         │
                         │  2. joins the match
                         ▼
               Gameplay Proxy (the one public door)
                         │
                         ▼
          Your game server (a Godot program with no window)
```

[How otomo fits together](../concepts/how-otomo-fits-together.md) goes through every box in
this picture.

## Next

Head to [Before you begin](before-you-begin.md) to get your computer ready.
