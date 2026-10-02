# Run a new game server build

Otomo doesn't contain a game server: it runs **yours**. This guide explains what your
Godot server has to do to work with otomo, then exports it, copies it to your VM, and
starts it in place of the old build. It takes 30 to 60 minutes the first time, and about
10 once you're used to it.

Read [Parties and matches](../concepts/parties-and-matches.md) and
[Servers and containers](../concepts/servers-and-containers.md) first. This guide also uses
the terminal a lot; if that's new to you, read
[Command-line basics](../concepts/command-line-basics.md) too.

**You'll need:**

- otomo deployed on your VM ([Deploy otomo on a VM](../start/deploy-on-a-vm.md));
- Godot 4.7.2 **.NET** and your game's **server project** on your computer;
- **WSL** on Windows ([Before you begin](../start/before-you-begin.md));
- a **login on the VM** (`ssh you@play.example.com` works from WSL).

## The big picture

```
 Your PC                                     Your VM (Linux)
 ───────                                     ───────────────
 Godot exports      ──── copy (scp) ────▶    deploy/gameserver/build/
 the server project                                 │
 for Linux                                          ▼  docker compose build
                                             a new game server image
                                                    │
                                                    ▼  docker compose up -d
                                             gs-1 and gs-2 restart on it,
                                             and tell the Allocator "I'm here"
```

**What's a dedicated server export?** Exporting a Godot project as a **dedicated server**
produces a Linux program with no window, graphics or sound (Godot strips them out), which
is exactly what a machine in a data centre needs. It's started **headless**: no screen at
all, just text output. Your server project can be a separate Godot project or the same
one as your game, exported with a server preset.

## What your game server must do

Otomo runs a fixed pool of game servers (`gs-1` and `gs-2`), each in its own container.
Otomo starts your program, but your code has to talk to the **Allocator**, the part of
otomo that hands servers out to parties. Here's the whole contract.

### Read the launch settings

Otomo passes these as **environment variables** (read them with
`System.Environment.GetEnvironmentVariable` in C#):

| Variable | Example | Meaning |
|---|---|---|
| `OTOMO_GS_SERVER_ID` | `gs-1` | This server's name. Use it in every Allocator call. |
| `OTOMO_GS_PORT` | `7777` | The UDP port to listen on with `ENetMultiplayerPeer.CreateServer`. |
| `OTOMO_GS_INTERNAL_ADDR` | `gs-1:7777` | The address the Gameplay Proxy forwards players to. Send it when registering. |
| `OTOMO_GS_CAPACITY` | `4` | How many players a match may have. |
| `OTOMO_ALLOCATOR_URL` | `http://allocator:8080` | Where the Allocator is. |
| `OTOMO_ALLOCATOR_KEY_FILE` | `/run/secrets/service_keys/allocator_gameserver.key` | A file holding the key to send with every Allocator call. |

When `OTOMO_ALLOCATOR_URL` is empty (in the editor, or on your PC), run **standalone**:
listen on a port and skip everything below. That's how you test the server locally.

### Talk to the Allocator

Every call is an HTTP `POST` with the header
`Authorization: Bearer <the contents of the key file>` and a JSON body:

| When | Call | Body | Answer |
|---|---|---|---|
| Once, at start-up | `POST {url}/internal/servers/register` | `{"server_id": "gs-1", "internal_addr": "gs-1:7777", "capacity": 4}` | `204` |
| Every 5 seconds | `POST {url}/internal/servers/gs-1/heartbeat` | `{"players_connected": 0}` | `200 {"allocation": null}`, or the match it's reserved for: `{"allocation": {"allocation_id": "…", "player_ids": ["…"], "expires_at": "…"}}` |
| When the match is over | `POST {url}/internal/servers/gs-1/ended` | `{"allocation_id": "…"}` | `204` |

Then follow this lifecycle:

| Event | What your server does |
|---|---|
| Start | Register, then heartbeat every 5 seconds |
| A heartbeat answers with an allocation | Remember its `allocation_id` and `player_ids`: that's the match, and those are the players allowed in |
| Players connect | Report the count in `players_connected` on every heartbeat |
| Everyone has left for about 20 seconds | Call `ended` with the allocation ID, then **exit with code 0** |
| A heartbeat answers `404 not_registered` | Otomo thinks this server died. Exit; it will be restarted |

Exiting after a match is deliberate: the container restarts it at once with a fresh world,
and it registers as free again. A server that stops heartbeating for 15 seconds is marked
dead, and its match ends.

### Check join tickets (recommended)

Each player connects with a **join ticket**: a short-lived token, signed by the Allocator,
naming the server and the player. The Gameplay Proxy already checks it, so a server that
skips this step still works. For defence in depth, check it again in
`SceneMultiplayer.AuthCallback` before calling `CompleteAuth`:

- the client sends the ticket's text with `SendAuth`
  ([Join a match](joining-a-match.md), Step 3);
- the ticket is a JWT signed with **Ed25519**; the public key is at
  `{OTOMO_ALLOCATOR_URL}/.well-known/jwks.json`;
- accept it only if `iss` is `https://allocator.otomo.internal`, `aud` is
  `otomo:gameserver`, it hasn't expired, `srv` is this server's ID, its player is in the
  allocation's `player_ids`, and you haven't already accepted the same `jti` (each ticket
  works once).

!!! note "Full details"
    `services/allocator/README.md` and `design/14-launch-handoff.md` in the repository
    give the exact fields and failure cases.

## Step 1: Install the Linux export templates (once)

Godot needs **export templates** (pre-built engine files) to export for each platform. You
install them once per Godot version.

1. Open your server project in Godot.
2. **Editor → Manage Export Templates…**
3. If it says the templates for **4.7.2.stable.mono** are missing, press **Download and
   Install**. It downloads about 1 GB, so give it a few minutes.

When it's done, the window lists the installed version. Close it.

## Step 2: Export the server

First create the export preset, once: **Project → Export… → Add… → Linux**, name it
`Linux`, and on its **Resources** tab set **Export Mode** to **Export as dedicated server**.

Then export, from the editor or from the command line. Both give the same result.

=== "From the editor"

    1. **Project → Export…**, and select the preset named **Linux**.
    2. Press **Export Project…**, choose an empty folder such as `C:\gs-export`, name the
       file `gameserver.x86_64`, untick **Export With Debug**, and press **Save**.

=== "From the command line"

    Open **Command Prompt** (Start menu → `cmd`) and run these three lines, one at a
    time:

    ```bat
    cd %USERPROFILE%\MyGame\server
    mkdir C:\gs-export
    C:\godot_4.7\Godot_v4.7.2_console.exe --headless --path . --export-release Linux C:\gs-export\gameserver.x86_64
    ```

    - Line 1 moves into your server project's folder. `%USERPROFILE%` is Windows' name for
      your home folder, such as `C:\Users\Mei`. Change the path to where your project is.
    - Line 2 makes the output folder.
    - Line 3 runs Godot without a window (`--headless`) on this project (`--path .`, where
      `.` means "this folder"), and exports the preset named `Linux` to the given file.
      Change `C:\godot_4.7\…` to wherever your Godot is.

    !!! warning "Don't type angle brackets"
        If you copy a path from documentation that contains a placeholder like `<out>`,
        replace the whole placeholder. In `cmd`, `<` means "read from a file", and you'll
        get `The system cannot find the file specified`.

The file **must** be named `gameserver.x86_64`: otomo's container starts it by that name.
When the export finishes, `C:\gs-export` contains:

| Item | What it is |
|---|---|
| `gameserver.x86_64` | The server program, with the game's resources packed inside it |
| `data_gameserver_linuxbsd_x86_64\` | The .NET runtime your C# code needs. **Always ship it together with the program.** |

## Step 3: Check the export on your PC (optional, recommended)

Before sending it to the VM, make sure the program starts on Linux. WSL *is* Linux, so you
can test it right on your PC.

1. Open **WSL**.
2. Go to the export folder. In WSL, your `C:` drive is at `/mnt/c`:

    ```sh
    cd /mnt/c/gs-export
    ```

3. Mark the program as runnable. Linux only runs files with the **execute permission**,
   and files copied from Windows don't have it:

    ```sh
    chmod +x gameserver.x86_64
    ```

4. Run it for 10 seconds, standalone, on a test port:

    ```sh
    OTOMO_GS_PORT=7791 timeout 10 ./gameserver.x86_64 --headless
    ```

    `./` means "the program in this folder". You should see Godot's start-up lines, then
    whatever your server prints when it starts listening. If it exits at once with an
    error, fix that before going further.

## Step 4: Copy it to your VM

Still in WSL, in `/mnt/c/gs-export`:

1. Pack both items into one file, so there's a single thing to copy:

    ```sh
    tar czf /tmp/gs-build.tgz gameserver.x86_64 data_gameserver_linuxbsd_x86_64
    ```

    Tip: type `data_` and press <kbd>Tab</kbd> to complete the long folder name.

2. Copy it to the VM's `/tmp/` folder:

    ```sh
    scp /tmp/gs-build.tgz you@play.example.com:/tmp/
    ```

3. Log in to the VM:

    ```sh
    ssh you@play.example.com
    ```

    **From now until Step 7, every command runs on the VM.**

4. Replace the old build with the new one:

    ```sh
    cd ~/otomo/deploy
    rm -rf gameserver/build
    mkdir -p gameserver/build
    tar xzf /tmp/gs-build.tgz -C gameserver/build
    ```

    - `cd ~/otomo/deploy` goes to otomo's deployment folder, where all the Docker
      commands are run from.
    - `rm -rf gameserver/build` deletes the previous build. Double-check you typed
      `gameserver/build`.
    - `mkdir -p` makes the folder again, empty.
    - `tar xzf … -C gameserver/build` unpacks your upload into it.

    Check the result with `ls gameserver/build`. It should list `gameserver.x86_64` and
    `data_gameserver_linuxbsd_x86_64`.

## Step 5: Build the image and restart the game servers

If this is the first build, add `gameservers` to `COMPOSE_PROFILES` in `deploy/.env`
(`COMPOSE_PROFILES=edge,gameservers`).

1. Build a new Docker **image** from the new files:

    ```sh
    docker compose build gs-1
    ```

    Both game servers use the same image, so building `gs-1` is enough.

2. Restart both game servers on the new image:

    ```sh
    docker compose up -d gs-1 gs-2
    ```

    `up` starts the listed containers, replacing any whose image changed. `-d` returns you
    to the prompt instead of showing their output.

!!! warning "This ends any match in progress"
    Restarting a game server ends its current match, and the players in it are returned
    to their lobby. Do it when nobody's playing, or warn your testers first.

3. Watch `gs-1` start:

    ```sh
    docker compose logs -f gs-1
    ```

    `-f` means "follow": keep showing new lines as they arrive. You should see your
    server start and register. Press <kbd>Ctrl</kbd>+<kbd>C</kbd> to stop watching (the
    server keeps running).

## Step 6: Check both servers are ready

Ask the Allocator's database which game servers it knows about:

```sh
docker compose exec postgres psql -U otomo allocator -c 'select server_id, state, players_connected, last_heartbeat from game_server order by 1'
```

This runs `psql` (a program that queries the Postgres database) inside the database's
container. You should see:

```text
 server_id | state | players_connected |        last_heartbeat
-----------+-------+-------------------+-------------------------------
 gs-1      | free  |                 0 | 2026-09-30 10:15:02.123+00
 gs-2      | free  |                 0 | 2026-09-30 10:15:03.456+00
```

Both `free`, and `last_heartbeat` within the last few seconds. Type `exit` to leave the
VM.

## Step 7: Join a match

Follow [Join a match](joining-a-match.md) and press **Join** in your game.

To watch the lifecycle while you play, log in to the VM again and repeat the query from
Step 6 every few seconds. You'll see `gs-1` go from `free` to `reserved`, then `busy` with
1 player. After everyone quits, it reports the match as ended, restarts, and is `free`
again.

## Keeping old builds to go back to

To be able to switch back to a previous build quickly, give each image a **tag** named
after your server project's commit (the short code from `git log`, such as `b97b748`):

```sh
OTOMO_GAMESERVER_TAG=b97b748 docker compose build gs-1
```

Then open `deploy/.env` in a text editor (`nano .env`; save with
<kbd>Ctrl</kbd>+<kbd>X</kbd>, then <kbd>Y</kbd>), set `OTOMO_GAMESERVER_TAG=b97b748`, and run
`docker compose up -d gs-1 gs-2`. To go back, set the old tag and run the same `up -d`
command. The old image is still stored, so there's nothing to rebuild.

## Keep the server's content in step with the clients

Players' games get content packs and settings from Patch. The game server doesn't: it
only has what was in its export. If you publish a release that changes gameplay data the
server also uses, export and restart the server too, or the two will disagree.

## If something goes wrong

| You see | What it means | What to do |
|---|---|---|
| `Cannot stat: No such file or directory` from `tar` | A typo in a file or folder name | Use <kbd>Tab</kbd> completion; check with `ls` |
| `Permission denied (publickey)` from `ssh` or `scp` | The VM doesn't have your SSH key | Add your public key to the VM ([Command-line basics](../concepts/command-line-basics.md)) |
| `Host key verification failed` | You ran `ssh` from Windows instead of WSL | Run it from WSL |
| `exec … gameserver.x86_64: no such file or directory` in the logs | The export isn't named `gameserver.x86_64`, or isn't in `gameserver/build/` | Rename it and repeat Step 4 |
| The log shows only Godot's version line and nothing else | Godot buffers printed output when it isn't a terminal | Enable **Application › Run › Flush stdout on Print** in the server project and re-export |
| Your server can't read the key file | The key files have the wrong owner | `sudo chown -R 65532:65532 secrets/service_keys`, then Step 5 again |
| Register answers `401` | Your server isn't sending the key, or sends it wrongly | Check the `Authorization: Bearer …` header carries the key file's contents, without a trailing newline |
| A server stays out of the Step 6 list | It crashed on start-up or never registered | `docker compose logs gs-1` and read the last lines |
| Join says `no_capacity` | Both servers are busy, or neither registered | Step 6 |

More in [Troubleshooting](../troubleshooting.md).
