# otomo

Self-hosted, vendor-neutral backend for Godot live-service games.

Otomo runs everything around your game that players never see: content updates without a
new build, device logins, friends, parties and lobbies, and handing each party a game
server. You host it on your own Linux VM with Docker Compose, and your Godot (C#) project
talks to it through the otomo SDK.

```
 Your Godot game ── HTTPS ──▶  edge ─▶ gateway ─▶ Patch · Auth · Session
        │                                              │
        └──── UDP ──▶ Gameplay Proxy ──▶ your game servers ◀── Allocator
                                     admin website (SSH tunnel only)
```

## What's in the box

| Part | What it does |
|---|---|
| **Patch** | Delivers settings documents and content packs per release channel (`dev`, `staging`, `live`), with rollback |
| **Auth** | Anonymous device login with rotating refresh tokens |
| **Session** | Profiles, presence, an event long-poll, friends and blocks, parties and the lobby, launch |
| **Allocator** | A pool of headless Godot game servers; reserves one per party and signs join tickets |
| **Gameplay Proxy** | The single public UDP door to the game servers; checks tickets |
| **Admin website** | Publish content, manage staff accounts (with TOTP), watch service health |
| **Edge** | TLS with Let's Encrypt for the player API and the sites |
| **Godot SDK** | `godot-plugin/addons/otomo`: patching with download UI hooks, login, Session, parties and launch |

## Getting started

The documentation assumes you've never used otomo and starts from an empty VM:

1. [What is otomo?](docs/start/what-is-otomo.md)
2. [Before you begin](docs/start/before-you-begin.md): a VM, a domain name, Godot .NET
3. [Deploy otomo on a VM](docs/start/deploy-on-a-vm.md)
4. [Install the SDK](docs/guide/install-the-sdk.md) in your Godot project

The full documentation is in [`docs/`](docs/index.md).

## Repository layout

| Path | Contents |
|---|---|
| `services/` | The Go services, the admin website (Vue), the edge and the wiki builder |
| `godot-plugin/` | The Godot C# SDK and its tests |
| `deploy/` | Docker Compose stack and scripts (`generate-secrets.sh`, `up.sh`) |
| `docs/` | User documentation (also served at `/docs/` on your deployment) |
| `design/` | Design documents and contracts, for people changing otomo itself |
| `ci/` | Test runners used by CI |

## Branches

- `main`: the latest release. Deploy from here.
- `staging`: the next release. Pull requests go here.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Security issues: see [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE)
