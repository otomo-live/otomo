# Troubleshooting

Find the symptom closest to yours. Each entry says what's happening and what to do. For a
specific error code, see [Error codes](reference/errors.md).

!!! tip "Read the Output panel first"
    With **Verbose Log** on (the default), the SDK prints every step to Godot's **Output**
    panel. The last few lines before a problem almost always say what went wrong.

## Installing the SDK

### `Otomo.Instance` is null, or "the Otomo autoload is missing"

The plugin isn't enabled, or the project wasn't built after the SDK was added.

1. Click **Build** (the hammer icon). Fix any build errors first.
2. **Project → Project Settings → Plugins**: tick **Enable** next to **Otomo SDK**.
3. Check **Project Settings → Globals** (or **Autoload**) lists `Otomo`.

Also make sure you're not using `Otomo.Instance` in a scene that runs before autoloads
exist. Any normal scene's `_Ready` is fine.

### The build fails with errors in `addons/otomo`

- The project isn't a C# project, or it's the non-.NET Godot. See
  [Before you begin](start/before-you-begin.md).
- The addon folder is at the wrong place (it must be `res://addons/otomo/Otomo.cs`, not
  `res://addons/otomo/otomo/Otomo.cs`).
- Godot hasn't noticed the new files: close and reopen the project, then build again.

### "Set Project Settings → otomo/config/base_url" at start-up

The **Base Url** setting is empty. Set it to `https://play.example.com` (or your own
otomo's address) under **Project Settings › Otomo › Config**, with **Advanced Settings**
switched on.

## Starting up and patching

### `patch: OfflineReady`, `login failed: … network`

The game couldn't reach otomo. Check in this order:

1. **Is the Base Url right?** It must start with `https://` and have no typo.
2. **Can your computer reach otomo at all?** In PowerShell, run:

    ```powershell
    curl.exe -i https://play.example.com/patch/v1/live/manifest
    ```

    - `HTTP/1.1 200 OK` means otomo is up and reachable, so the problem is in the game's
      settings.
    - `Could not resolve host` means a typo in the address, or no internet.
    - `Failed to connect` or a timeout means the server is down or blocked on your network
      (some school or office networks block unusual traffic). Try another network, and
      check otomo is running on your VM.

3. **Is otomo healthy?** The admin website's **Overview** page shows every service's
   state.

The game still runs offline with the content from its last successful start.

### `ClientTooOld`

The current release's minimum client version is higher than this build's
**Client Version** setting.

- **If you're a player** (or testing as one): you need a newer build.
- **If you're developing:** check **Project Settings › Otomo › Config › Client Version**.
  An empty setting means `0.0.0` unless **Application › Config › Version** is set.
- **If it was a mistake in the release:** an admin rolls `live` back
  ([Publish your first content update](guide/first-patch.md), Step 8).

### My published change doesn't show up in the game

Work through these:

1. **Did the release reach `live`?** In the admin website, **Releases → live**: is your
   release the **head**? Creating a version isn't enough. It must be in a **published
   release**.
2. **Did you include the right version?** The release may include an older version of the
   namespace. Check the release's **Client manifest**.
3. **Did the game restart?** The game only checks for new content when it starts.
4. **Is the audience Client?** Server-audience namespaces never reach the game.
5. **Do the names match exactly?** `GetInt("balance.player", "max_hp", …)` needs the
   namespace `balance.player` and the key `max_hp`, same spelling and same capitals.
6. **Is the value the right kind?** `"max_hp": "100"` (in quotes) is text, so `GetInt`
   returns the fallback. Write `"max_hp": 100`.

### A file in my pack isn't found

- **Is the path identical?** The first path you gave `add_file` (such as
  `res://patch/first/note.txt`) is the one your code must use, character for character,
  including capitals.
- **Does the game build already contain that path?** Packs can't replace files shipped with
  the game ([Content and patching](concepts/content-and-patching.md)). Use a path the build
  doesn't have.
- **Is it an imported resource?** Textures, sounds and scenes must be exported through
  Godot's **Export PCK/ZIP**, not packed as raw files.

### Every run downloads everything again

The SDK saves what it downloaded in the user data folder. If that folder is being deleted
or isn't writable (some sandboxes and CI runners), every start is a fresh install.

## Logging in

### `validation_failed` when logging in

The saved device ID is malformed, usually because someone opened the `device_id` file and
changed it. Delete `otomo/device_id` from the user data folder. **This creates a new
account.**

### `AuthenticationLost` fires

A request was refused even after the SDK renewed the login. Send the player back to your
start screen and call `StartAsync` again. If it happens repeatedly, report the `requestId`
the signal gives you.

## Joining a match

### `no game server after 30 s`

No game server was free. Ask whoever runs the servers to check them
([Run a new game server build](guide/game-servers.md), Step 6). The Overview page's alerts
also show "no free game servers".

### `proxy refused the ticket: …`

See the Gameplay Proxy table in [Error codes](reference/errors.md). Joining again fixes
most of these.

### `no answer from the gameplay proxy`

The game's UDP messages to the proxy aren't getting answered.

- Your network may block UDP (common on school, office and public Wi-Fi). Try another
  network.
- The proxy may be down: the admin website's Overview page shows its state.

### Connected, then disconnected immediately

Otomo got you in, and then the *game server* turned you away, for example because the
match had already started. The game server's log says why ([Run a new game server build](guide/game-servers.md)).

## The admin website

### `channel 3: open failed: connect failed: Connection refused` in the tunnel window

Your tunnel command says `localhost` where it should say `127.0.0.1`. Use:

```sh
ssh -N -L 8090:127.0.0.1:8090 you@play.example.com
```

### I sign in, and I'm signed out again straight away

You opened `http://127.0.0.1:8090/admin/`. Use `http://localhost:8090/admin/` instead: the
sign-in cookie only works with `localhost`.

### `Host key verification failed`

You ran `ssh` from Windows. Run it from WSL
([Before you begin](start/before-you-begin.md)).

### `Permission denied (publickey)`

The VM doesn't know your SSH key. Add your **public** key to the VM, or send it to
whoever manages it ([Command-line basics](concepts/command-line-basics.md)).

### A button is missing, or a page says "Not available"

Your role doesn't allow it. Publishing to `live`, rolling back and creating namespaces need
**admin** ([Use the admin website](guide/admin-website.md)).

## Exporting and deploying

### `The system cannot find the file specified` in Command Prompt

You typed a placeholder with angle brackets, like `<out>`. Replace the whole placeholder,
brackets included, with a real path.

### `Case mismatch opening requested file 'res://Scenes/…'`

Your code asks for `Scenes/…`, but the folder on disk is `scenes/…` (or the reverse).
Windows doesn't care, but Linux builds and packs do, so the file won't load there. Make the
path in the code match the folder's real capitalisation.

### More deployment problems

[Run a new game server build](guide/game-servers.md) has a table of game-server problems,
and [Deploy otomo on a VM](start/deploy-on-a-vm.md) covers the rest.
