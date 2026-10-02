# Install the SDK

This guide adds otomo's Godot SDK to a Godot C# project and checks that the game can reach
otomo. It takes about 15 minutes.

**You'll need:** Godot 4.7.2 .NET and a project that builds (see
[Before you begin](../start/before-you-begin.md)), and otomo running on your VM with a
public HTTPS address ([Deploy otomo on a VM](../start/deploy-on-a-vm.md)).

## What you're installing

The SDK is a Godot **addon**: a folder you copy into your project's `addons/` folder, like
any addon from the Asset Library. It gives your project:

- an **autoload** called `Otomo`: a node that Godot creates when the game starts and keeps
  for the whole run, reachable from any script as `Otomo.Instance`;
- a few **project settings** under **Otomo › Config**, such as the address of the server;
- a **test scene** that checks your game can reach otomo.

## Step 1: Get the SDK files

The SDK lives in the otomo repository on GitHub
(`https://github.com/otomo-live/otomo`), in the folder `godot-plugin/addons/otomo`.

1. Open the repository on GitHub and make sure the branch selector (top left, above the
   file list) says **`main`**, the latest release. Use the same release of the SDK as
   the otomo running on your VM.
2. Click the green **Code** button → **Download ZIP**.
3. Unzip the file anywhere, for example your Downloads folder.

## Step 2: Copy it into your project

1. In the unzipped files, open `godot-plugin` → `addons`. Inside is a folder named `otomo`.
2. Copy that `otomo` folder into your game project's `addons` folder, so you end up with
   `<your project>/addons/otomo/Otomo.cs`. If your project has no `addons` folder, create
   one next to `project.godot`.

!!! warning "Keep the folder name and place exactly"
    The addon must be at `res://addons/otomo/`. Godot looks for the plugin there, and a
    different folder name or an extra level of folders (such as
    `addons/otomo/otomo/`) means Godot won't find it.

## Step 3: Build the project

The SDK is written in C#, and Godot can't use C# code until it's been **built** (compiled).

1. Open your project in Godot.
2. Click **Build** (the hammer icon, top right).
3. Wait for it to finish. The **MSBuild** panel at the bottom shows the result. There
   should be no errors mentioning `addons/otomo`.

!!! tip "If the build fails right after copying"
    Close Godot and open the project again, then build once more. Godot sometimes needs a
    restart to notice a new C# folder.

## Step 4: Enable the plugin

1. Open **Project → Project Settings…** and select the **Plugins** tab.
2. Find **Otomo SDK** in the list and tick **Enable**.

Enabling the plugin does two things automatically:

- It adds the `Otomo` autoload. You can see it in the **Globals** tab (called **Autoload**
  in some versions of Godot): `Otomo` → `res://addons/otomo/Otomo.cs`.
- It adds the SDK's settings. In the **General** tab, switch on **Advanced Settings** (top
  right), then scroll to **Otomo › Config**:

| Setting | Default | What it's for |
|---|---|---|
| **Base Url** | `http://localhost:8080` | The address of your otomo. **Change it** to your public HTTPS address, such as `https://play.example.com`. |
| **Client Version** | *(empty)* | The version of this game build, like `1.0.0`. When empty, the SDK uses **Application › Config › Version**, or `0.0.0` if that's empty too. Content can require a minimum version; see [Content and patching](../concepts/content-and-patching.md). |
| **Channel** | `live` | Which stream of content to get. Leave it at `live` for anything players run. |
| **Verbose Log** | on | Print what the SDK does in the **Output** panel. It never prints secrets. |
| **Confirm Download Over Bytes** | `-1` | Ask the player before a big download. See [Build a download screen](download-screen.md). |

Set **Base Url** to your otomo's address now, without a slash at the end. Set
**Client Version** too (for example `1.0.0`), and increase it with every build you give to
players.

## Step 5: Check the game reaches otomo

The SDK comes with a test scene that runs the whole start-up sequence and prints each step.

1. In the **FileSystem** panel, open `res://addons/otomo/Diagnostics/OtomoSmokeTest.tscn`.
2. Press <kbd>F6</kbd> (**Run Current Scene**). An empty window opens. Look at the
   **Output** panel at the bottom of the editor.

You should see lines like these. This is a real run; your release number, lists and
player name will differ:

```text
otomo: gateway https://play.example.com, channel live, client 1.0.0
patch: Checking
otomo smoke: patch state Checking
patch: Mounting
otomo smoke: patch state Mounting
patch: Ready
otomo smoke: patch state Ready
patch: ready (release_id=4)
otomo auth: logged in
otomo smoke: patch=Ready release=4 config_docs=[ui.motd] packs=[otomo_patch_demo]
otomo smoke: logged_in=True session=Available profile=Player1676#9062
otomo smoke: OK
```

Line by line:

- `patch: Ready`: the game asked for the current content and has it. `config_docs` lists
  the settings documents in the current release, and `packs` the content packs. Empty
  lists (`[]`) are fine: they just mean the release has none.
- `otomo auth: logged in`: the game made (or reused) its device ID and got login tokens.
  See [Logins and tokens](../concepts/logins-and-tokens.md).
- `session=Available profile=…`: Session answered and created this player's profile, with a
  temporary name.
- `otomo smoke: OK`: everything worked.

Close the game window when you're done.

If you see `FAILED` or an error instead, the line before it says why. The most common
causes are in [Troubleshooting](../troubleshooting.md).

## Step 6: Start otomo from your game

The test scene proves it works. Now make your own game do the same when it starts. In the
script of your **first scene** (the one that opens when the game launches, often a splash
screen or main menu), add:

```csharp
using Godot;
using OtomoSdk;          // Otomo, StartResult
using OtomoSdk.Patch;    // PatchResult

public partial class Boot : Node
{
    public override async void _Ready()
    {
        // Patch, then log in, then say hello to Session. This waits until all three are done.
        StartResult result = await Otomo.Instance.StartAsync();

        if (result.Patch == PatchResult.ClientTooOld)
        {
            // The content needs a newer build of the game. Tell the player to update.
            GD.Print("Please update the game.");
            return;
        }

        if (!result.LoggedIn)
        {
            // No connection to otomo. The game can still run offline, with the content it
            // had last time; online features won't work.
            GD.Print($"Offline: {result.Error?.Message}");
        }

        GetTree().ChangeSceneToFile("res://scenes/MainMenu.tscn");
    }
}
```

A few things to know about this code:

- **`async` and `await`.** Talking to a server takes time, from milliseconds to seconds.
  `await` means "pause this function here until the answer arrives, *without* freezing the
  game". The game keeps drawing frames while it waits. `_Ready` is marked `async` so it's
  allowed to use `await`.
- **Call it before loading patched content.** Anything the release delivers (settings,
  packs) exists only after `StartAsync` finishes. Load your main menu afterwards, as above.
- **Call it once**, from the first scene. The `Otomo` autoload lives for the whole game, so
  every later scene can use `Otomo.Instance` directly.

## Updating the SDK later

When otomo's SDK gets new features, repeat Steps 1 to 3. Delete your old
`addons/otomo` folder first, then copy the new one in, but **keep the `.uid` files** Godot
created next to the scripts, if your team commits them. The settings you chose survive, because
they're stored in `project.godot`, not in the addon. Update the SDK when you update otomo
on your VM, so both are the same release.

## Next

- [Publish your first content update](first-patch.md): change something from the admin
  website and watch the game pick it up.
- [Build a download screen](download-screen.md): show the player what's downloading.
- [SDK reference](../reference/sdk.md): everything the SDK offers.
