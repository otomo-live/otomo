# Roadmap

Where otomo is going, and roughly in what order. Nothing here has a date; the order can
change. If you'd like to work on something, open an issue first so we can agree on the
approach (see [CONTRIBUTING.md](CONTRIBUTING.md)).

## What otomo is for

A self-hosted backend for live-service games that you run on your own servers: content
delivery, logins, social features, lobbies, and getting each party onto a dedicated game
server. Your headless game server stays the authority for gameplay; otomo runs everything
around it.

Otomo is built and tested with Godot today. Its services are plain HTTP and UDP, so any
engine can use them: we document the protocols, and anyone is free to write an SDK for
another engine, open-source or commercial.

## In place today

- **Patch**: release channels (`dev`, `staging`, `live`), immutable releases, rollback,
  settings documents and content packs, content-addressed downloads.
- **Auth**: anonymous device login with rotating refresh tokens.
- **Session**: profiles, presence, an event stream, friends and blocks, parties and lobbies,
  launch.
- **Allocator and Gameplay Proxy**: a pool of headless game servers, signed single-use join
  tickets, and one public UDP entry point.
- **Admin website**: content publishing, staff accounts with roles and two-factor sign-in,
  service health and alerts.
- **Operations**: one-command deploy on a single VM with Docker Compose, TLS with Let's
  Encrypt, Prometheus and Loki.
- **Godot SDK** (C#): patching with download-UI hooks, login, Session, parties and launch.

## Next

### Matchmaker

Matching players and parties who don't know each other into a match, between Session
(parties) and the Allocator (game servers), whose interfaces it plugs into.

### More ways to sign in

Platform and social logins linked to the same player account: **Steam** (Steamworks session
tickets) and **Google** first, others after. Auth already stores more than one identity per
account, so a player who starts with a device login can link a platform account later
without losing progress.

### Protocol reference for SDK authors

A public, versioned description of every player-facing interface: an OpenAPI spec for the
HTTP API, the event types, the error codes, the UDP join handshake byte by byte, and a
compatibility policy saying what counts as a breaking change. This is what lets anyone
write an SDK for their own engine.

## Later

### Command-line interface

An `otomo` CLI over the admin API, so scripts, CI jobs and automated tools can drive otomo
without the website: publish and roll back releases, upload packs, manage staff and
servers, read health. Machine-readable output and meaningful exit codes. It needs
**service accounts** first: non-interactive, role-scoped, revocable credentials whose
actions are audited like a person's.

### Patch: CDNs and engine-agnostic packs

- **CDN delivery.** Serve releases from object storage behind the major CDNs, with signed
  URLs for restricted channels, while Patch stays the source of truth for what each
  channel contains.
- **Any engine's packs.** Content packs are Godot `.pck` files today. Packs become opaque,
  typed files: the manifest says what each one is, and each engine's SDK decides how to
  load it.

### In-app purchase verification

A small interface for verifying store purchases (receipts from the platform stores) and
recording them against the player's account. Shops, currencies and rewards themselves are
built with extensions (below), so each game shapes its own economy.

### Extensions: server-side scripting

A way to customise otomo's own services with your code, without forking otomo: a shop and
currencies on top of purchase verification, rewards, lobby rules. It **adds to** your game
server; it doesn't replace it.

- One documented **hook contract**: otomo calls your code at defined points (before a
  purchase, after a match, when a lobby setting changes) and acts on the answer.
- Language runtimes as separate pieces behind that contract: **C#**, **Lua**,
  **JavaScript/TypeScript**, and, if it proves workable, **GDScript** on headless Godot
  workers.
- Starting with one runtime and one hook, to prove the contract before adding more.

### Modules

The same contract opens otomo to **modules**: optional pieces that add a service or connect
otomo to another system, such as a connector to Nakama or to a commercial backend, without
changing otomo's core.

## Exploring

### Large assets from Git LFS

Games often keep large assets in Git LFS. Whether otomo needs to know about LFS at all is
still open: if those assets are built into content packs before upload, Patch delivers them
like any other pack and no LFS support is needed. If you have a workflow where it matters,
tell us in an issue.

## Not planned for now

- **First-party SDKs for other engines.** We document the protocols; community SDKs are
  welcome and we'll link to them.
- **Running gameplay inside otomo.** Gameplay belongs to your headless game server.
  Extensions customise otomo's services, not your game's simulation.
