# Parties and matches

This page explains how players get from the main menu into a match: what a party is, what
happens when the leader presses Play, what a join ticket is, why there's a proxy in the
middle, and how a game server's life goes from start to finish.

## Parties are lobbies

In otomo, a **party** is a group of players who want to play together, and it's also the
**lobby** before a match. There's no separate lobby system. A party has:

- **members**, one of whom is the **leader**;
- **settings** the leader chooses, such as `{"expedition": "forest", "difficulty": "hard"}`.
  Which settings exist, and which values are allowed, is decided by your team in the
  server namespace `session.rules`;
- a **ready** flag for each member;
- a **state**: `forming` (normal lobby), `launching` (looking for a server) or `in_game`
  (the match is running).

A player alone is simply a party of one.

### The revision number

Every change to a party (someone joins, a setting changes) increases its **revision**, a
counter. When the leader changes something, the game sends the revision it last saw. If
someone else changed the party in the meantime, the numbers don't match and Session answers
`409 revision_mismatch` instead of applying a change based on out-of-date information. The
game then fetches the party again and retries. This stops two quick clicks from
overwriting each other.

## From "Play" to playing

Here's the whole journey, step by step. The first half happens over HTTP, the second over
UDP.

```
 Leader's game        Session          Allocator         Proxy         Game server
      │  POST /party/launch │                │               │               │
      │───────────────────▶│  reserve a     │               │               │
      │                    │  server please │               │               │
      │                    │──────────────▶│  picks gs-1,  │               │
      │                    │               │  writes tickets│               │
      │                    │◀──────────────│               │               │
      │ event: launching,  │               │               │               │
      │ your ticket, the   │               │               │               │
      │ proxy's address    │               │               │               │
      │◀───────────────────│               │               │               │
      │  UDP: here's my ticket (OTJ1…)     │               │               │
      │───────────────────────────────────────────────────▶│  checks it    │
      │  UDP: OK (OTOK)                                    │               │
      │◀───────────────────────────────────────────────────│               │
      │  ENet: normal Godot multiplayer, through the proxy  │──────────────▶│
      │◀═══════════════════════════════════════════════════╪═══════════════│
```

1. **Everyone readies up.** Each member tells Session they're ready.
2. **The leader launches.** Session checks that the leader asked and everyone is ready,
   switches the party to `launching`, and answers `202 Accepted`: "started, the result
   will follow".
3. **Session asks the Allocator for a server.** The Allocator keeps a list of the game
   servers and knows which ones are `free`. It **reserves** one for this party.
4. **The Allocator writes a join ticket for each player.** More on tickets below.
5. **Session tells every member**, using its events (see
   [How otomo fits together](how-otomo-fits-together.md)): "the match is ready; here's the
   address and port to connect to, and here's *your* ticket". The party is now `in_game`.
6. **Each game sends its ticket to the Gameplay Proxy** in a small UDP message, and the
   proxy answers "OK" or "no, and here's why".
7. **Each game connects with Godot's multiplayer (ENet)**, through the proxy, to the game
   server. From here on it's an ordinary Godot multiplayer session.

If no server is free in step 3, every member gets a "launch failed" event, the party goes
back to `forming`, and the leader can try again.

## Join tickets

A **join ticket** is a pass for exactly one player, one match and one game server. It's
the same kind of signed token as a login token (see
[Logins and tokens](logins-and-tokens.md)), stating:

- which player it's for;
- which allocation (this particular reservation) it belongs to;
- which game server to go to;
- a unique ID, so it can be used **only once**;
- an expiry **60 seconds** after it was written.

The short life and single use mean a leaked ticket is almost worthless. If a player's
ticket expires before they connect (their computer was slow, or they lost connection),
the game simply asks Session for a fresh one.

## Why a proxy?

Game servers could simply be put on the internet, one port each. Otomo puts a **Gameplay
Proxy** in front of them instead, for three reasons:

- **One door to guard.** Only one public port is open for all matches, and nothing reaches
  a game server without a valid ticket. Random traffic from the internet never reaches
  your game server code.
- **Game servers stay private.** They can be moved, restarted or added without players
  noticing, because players only ever know the proxy's address.
- **The ticket is checked before a single game packet flows.**

### The "same port" rule

The proxy recognises a player by where their packets come from: their IP address *and*
their [local port](how-games-go-online.md). After it accepts a ticket from a given address
and port, it forwards traffic from that exact address and port, and nothing else.

So the game must send its ticket from the **same local port** that ENet will use
afterwards. The launch code does this for you: it picks a port, sends the ticket from it,
then tells ENet to use that port too. (Today that code is `OtomoLaunch.cs` in the game
project; it's moving into the SDK. See [Join a match](../guide/joining-a-match.md).) If you
ever write this code yourself, this is the detail to get right.

## A game server's life

Otomo runs a small, fixed set of game servers (on your VM: `gs-1` and `gs-2`).
The Allocator controls their lifecycle:

| Step | Game server | Allocator's view |
|---|---|---|
| Starts up | Tells the Allocator "I'm `gs-1`, reach me here, I hold 4 players", then says "still alive" every 5 seconds | `free` |
| A party launches | Learns from the Allocator's reply that it's been reserved, and for which players | `reserved` |
| First player connects | Reports "1 player connected" | `active` (busy) |
| Everyone has left for 20 seconds | Tells the Allocator "match over", then **quits** | `ended`, then back to `free` |
| Quits | Docker starts it again immediately, with a fresh world | registers again as `free` |

A few things can go wrong, and each is handled:

- **Nobody connects within 60 seconds** of the launch: the reservation **expires**, the
  server is `free` again, and the party is told.
- **A game server crashes or freezes**, so its "still alive" messages stop: after 15
  seconds the Allocator marks it `dead` and tells the party the match is over. Docker
  restarts the server.

Whenever a match ends, for any of these reasons, Session puts the party back into
`forming` and tells every member, so the players land back in the lobby together.

!!! note "Why quit at the end of every match?"
    A game server that has run a match holds leftover state: enemies, dropped items,
    scores. Quitting and being restarted is the simplest way to guarantee the next party
    starts with a clean world.

## Next

Continue with [Servers and containers](servers-and-containers.md) to see what "Docker
starts it again" actually means.
