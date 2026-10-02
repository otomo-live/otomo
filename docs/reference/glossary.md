# Glossary

Every technical word this wiki uses, in plain language. Where a word has its own page, the
entry links to it.

**Access token**
: A login pass the game attaches to every request. It lasts 15 minutes. See
  [Logins and tokens](../concepts/logins-and-tokens.md).

**Admin website**
: The private website where staff publish content and watch otomo's health. See
  [Use the admin website](../guide/admin-website.md).

**Allocation**
: One reservation of a game server for one party's match.

**Allocator**
: The otomo service that knows which game servers are free, reserves them, and writes join
  tickets.

**API**
: The list of requests a server understands. See [HTTP and APIs](../concepts/http-and-apis.md).

**async / await**
: C# keywords for waiting for something slow (like a server's answer) without freezing the
  game.

**Audience**
: Whether a settings namespace is sent to players' games (**client**) or kept on the server
  (**server**).

**Auth**
: The otomo service that logs players in. Also short for *authentication*: proving who you
  are.

**Autoload**
: A Godot node created when the game starts and kept for the whole run. The SDK's is called
  `Otomo`.

**Backend**
: The parts of an online game that run on servers, out of the player's sight. Otomo is a
  backend.

**Base URL**
: The address the SDK sends every request to: otomo's gateway.

**bash**
: The shell (command language) of Linux. See [Command-line basics](../concepts/command-line-basics.md).

**Blob**
: A stored file (a settings document or a pack), named by its SHA-256 fingerprint.

**Certificate**
: A signed document proving a server owns its domain name, used for HTTPS.

**Channel**
: A stream of releases: `dev`, `staging` or `live`. Players get `live`. See
  [Content and patching](../concepts/content-and-patching.md).

**CI (continuous integration)**
: Automatic testing of every change. Otomo's repository uses GitHub Actions.

**Client**
: A program that contacts a server. Your game is a client.

**Container**
: A running copy of an image, sealed off from other containers. See
  [Servers and containers](../concepts/servers-and-containers.md).

**Dedicated server**
: A Godot export with no window, graphics or sound, which runs matches on a server.

**Device ID**
: A long random text the SDK creates on first run. It identifies the player's account.

**DNS**
: The system that turns names like `play.example.com` into IP addresses.

**Docker / Docker Compose**
: The tools that package otomo's programs as images and run them as containers, all
  described in one file, `deploy/compose.yaml`.

**Draft**
: The unsaved, editable state of a settings namespace in the admin website.

**Edge**
: Otomo's front door on the internet. It handles HTTPS and passes requests inward.

**ENet**
: The networking library Godot's multiplayer uses, over UDP.

**Endpoint / route**
: One specific request a server accepts, such as `POST /party/launch`.

**Event**
: A message from Session telling the game that something changed. Delivered by long
  polling.

**Fallback**
: The value your code uses when a setting is missing. Every `RemoteConfig.Get…` call takes
  one.

**Firewall**
: A gatekeeper that decides which ports on a computer the internet can reach.

**Gameplay Proxy**
: The one public door to otomo's game servers. It checks join tickets.

**Game server**
: Your dedicated server program, running a match.

**Gateway**
: The otomo service that checks login passes and rate limits, and routes each request to the
  right service.

**Head**
: The release a channel is currently serving.

**Headless**
: Running without a window or screen.

**Heartbeat**
: A regular "still alive" message. Game servers send one to the Allocator every 5 seconds.

**HTTP / HTTPS**
: The request-and-response language of the web. HTTPS is HTTP with TLS encryption. See
  [HTTP and APIs](../concepts/http-and-apis.md).

**Image**
: A program packed with everything it needs to run, from which containers are started.

**IP address**
: A computer's numeric address on a network, such as `203.0.113.10`.

**Join ticket**
: A single-use pass, valid for 60 seconds, that lets one player into one match. See
  [Parties and matches](../concepts/parties-and-matches.md).

**JSON**
: A plain-text data format: `{"name": "value"}`. See [HTTP and APIs](../concepts/http-and-apis.md).

**JWT**
: JSON Web Token: a signed token format used for otomo's login passes and join tickets.

**Linux**
: The operating system otomo's server runs.

**Live**
: The channel every player receives.

**Local port**
: The port on the player's own computer that their game sends from.

**localhost / 127.0.0.1**
: "This same computer."

**Long polling**
: Asking a server a question that it holds open until it has news. How Session delivers
  events.

**Manifest**
: The list of files in a release, each with its size and fingerprint.

**Minimum client version**
: The oldest game build a release allows. Older builds get `ClientTooOld`.

**Namespace**
: A named settings document in the admin website, such as `balance.player`.

**Pack (.pck)**
: A Godot file bundling resources, delivered by otomo and mounted into `res://`.

**Party**
: A group of players who play together; also the pre-match lobby.

**Patch / patching**
: The otomo service that serves content, and the process of downloading and loading it.

**Path**
: The address of a file (`C:\Games\x`, `/home/mei/x`), or the part of a URL after the domain
  name (`/party/launch`).

**Port**
: A number that picks which program on a computer receives a message.

**Postgres**
: The database otomo's services store their data in.

**Promote**
: Copy one channel's head release to the next channel (`dev` → `staging` → `live`).

**Pull request (PR)**
: A request to review and merge a branch of changes.

**Rate limit**
: A cap on how many requests a player may send per second.

**Refresh token**
: A saved, single-use pass for getting a new access token. Lasts 30 days.

**Registry**
: A store of Docker images, each with a tag.

**Release**
: A published, unchangeable snapshot of content: namespace versions, packs and a minimum
  client version.

**RemoteConfig**
: The SDK object your code reads settings from.

**Request ID**
: A unique ID for one request, used to find it in the logs. Include it in bug reports.

**REST**
: A common style of HTTP API: paths name things, and methods say what to do with them.

**Revision**
: A party's change counter, used to stop out-of-date changes.

**Roll back**
: Point a channel at an earlier release, undoing later ones.

**Schema**
: A description of what a settings document must look like.

**SDK**
: Software Development Kit: here, the Godot addon that connects your game to otomo.

**Server**
: A program that waits for clients to contact it, or the computer such programs run on.

**Service**
: One of otomo's programs, each with one job (Auth, Patch, Session…).

**Session**
: The otomo service for profiles, friends, parties and events.

**SHA-256 / fingerprint**
: A 64-character code computed from a file's exact contents, used to check a download isn't
  damaged or altered.

**Shell / terminal**
: A text window for typing commands, and the program that runs them.

**Signal**
: Godot's way for a node to announce that something happened.

**SSH / SSH key / SSH tunnel**
: Secure remote login, the key pair that proves who you are, and a way to carry a port
  through that connection. See [Command-line basics](../concepts/command-line-basics.md).

**Status code**
: The three-digit number summarising an HTTP response, such as `200` or `404`.

**sudo**
: Run a Linux command with administrator rights.

**Tag (image tag)**
: A version name for a Docker image.

**TCP / UDP**
: The two ways programs send data: TCP is a reliable connection, UDP is fast
  fire-and-forget packets. See [How games go online](../concepts/how-games-go-online.md).

**TLS**
: The encryption that protects HTTPS.

**Token**
: A signed pass proving who you are.

**Valkey**
: A fast, in-memory store Session uses for things like who's online.

**Version (namespace version)**
: A numbered, frozen copy of a settings namespace.

**VM (virtual machine)**
: A computer simulated in software on a larger physical machine.

**WSL**
: Windows Subsystem for Linux: Linux running inside Windows.
