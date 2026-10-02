# Game server pool

The fixed pool of your game's dedicated servers (design/14-launch-handoff.md §4.3): `gs-1` and
`gs-2` in `compose.yaml`, behind the `gameservers` profile. Each one registers itself with
the Allocator, heartbeats every 5 s, and is reached only through the Gameplay Proxy on UDP
27000. No game server publishes a port.

## Build

The image wraps a Linux export of your game's server project. It isn't built
from source here: export it, copy it to `deploy/gameserver/build/`, then build.

```sh
# On a machine with Godot 4.7 .NET and its export templates:
godot --headless --path <your server project> --export-release Linux <out>/gameserver.x86_64
# <out> now holds gameserver.x86_64 and data_gameserver_linuxbsd_x86_64/.
scp -r <out>/* you@play.example.com:otomo/deploy/gameserver/build/

# On the host, with COMPOSE_PROFILES=edge,gameservers in deploy/.env:
docker compose build gs-1 && docker compose up -d gs-1 gs-2
```

`gs-1` and `gs-2` share the image, so one build serves both.

## Lifecycle

The Allocator owns it; the server follows (what your server must implement is in
[Run a new game server build](../../docs/guide/game-servers.md)):

| Event | Allocator state | Server |
|---|---|---|
| Start | registered, `free` | registers, heartbeats |
| Party launches | allocation `reserved` | sees the allocation in the heartbeat answer |
| First player connects | `active`, server `busy` | heartbeat with `players_connected > 0` |
| Everyone gone for 20 s | `ended`, server `free` | `POST .../ended`, then exits 0 |
| Allocation expired or ended under it | `expired` / `ended` | exits 0 if it hosted anyone |
| Killed or hung | `dead` after 15 s, allocation `server_dead` | compose restarts it, it registers again |

`restart: always` turns every exit into a fresh world that registers as `free`.

## Check

```sh
docker compose logs gs-1 | grep allocator:
docker compose exec postgres psql -U otomo allocator -c 'select server_id, state, allocation_id, players_connected, last_heartbeat from game_server'
```
