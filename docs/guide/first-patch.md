# Publish your first content update

In this guide you'll change your game from the admin website, without rebuilding it:

1. Make a label in your game show a **message of the day** that comes from otomo.
2. Publish a settings document that changes the message and its colour.
3. Publish a small **content pack** with a file the game reads.
4. Watch the game pick both up, then **roll back**.

It takes 30 to 45 minutes the first time. Read
[Content and patching](../concepts/content-and-patching.md) first, because this guide uses its
words (namespace, version, pack, release, channel) without explaining them again.

**You'll need:**

- the SDK installed, and `StartAsync` called from your first scene
  ([Install the SDK](install-the-sdk.md));
- the admin website open in your browser ([Use the admin website](admin-website.md));
- an **admin** role, because this guide publishes to `live`. With **live_ops**, you can
  follow every step up to publishing, then ask an admin to publish.

!!! danger "This guide changes what every player gets"
    The game can only read the **`live`** channel: `dev` and `staging` need a staff
    login, which a player's game doesn't have. So a test release published here reaches
    **every player** who starts the game while it's live. Do this at a quiet time, use
    harmless content, and **always finish with the rollback in Step 8**.

## Step 1: Show a setting in your game

First, make your game display a value from otomo, with a **fallback** for when it's missing.

1. In a scene that loads **after** `StartAsync` has finished (your main menu, for example),
   add a **Label** node and name it `Motd`.
2. Attach a new C# script to it, `Motd.cs`, with this code:

    ```csharp
    using Godot;
    using OtomoSdk;

    public partial class Motd : Label
    {
        public override void _Ready()
        {
            var config = Otomo.Instance.RemoteConfig;

            // Namespace "ui.motd", key "text". The third value is the fallback, used when
            // the release has no such document or key (for example, today).
            Text = config.GetString("ui.motd", "text", "Welcome!");

            // Keys with dots reach inside nested objects: "style" → "color".
            string color = config.GetString("ui.motd", "style.color", "#ffffff");
            Modulate = new Color(color);

            // A switch your team can turn off without a new build.
            Visible = config.GetBool("ui.motd", "show", true);
        }
    }
    ```

3. Run the game. The label shows **Welcome!** in white, the fallback values. That's
   expected: nothing has been published yet.

!!! tip "Why fallbacks matter"
    A player who starts the game for the first time with no internet has no settings at
    all, so every value comes from the fallbacks. Choose fallbacks that make the game work
    properly on their own.

## Step 2: Create the namespace

A namespace is created once; after that you only add versions to it. You need the
**admin** role for this step.

1. In the admin website, open **Namespaces**.
2. Press **Create namespace**.
3. Fill in:
    - **Name:** `ui.motd`. Lowercase, with a dot between the words.
    - **Audience:** **Client**. The game must be able to read it. This can't be changed
      later.
    - **Description:** something like `Message of the day on the main menu`.
4. Press **Create**.

If `ui.motd` already exists (someone did this guide before you), skip to Step 3 and use it.

## Step 3: Write the settings and create a version

1. In **Namespaces**, click **ui.motd** to open its editor.
2. Switch to the **JSON** tab. It shows the draft as text.
3. Replace everything in the box with:

    ```json
    {
      "text": "Hello from the admin website!",
      "style": { "color": "#66ccff" },
      "show": true
    }
    ```

    The key names (`text`, `style`, `color`, `show`) must match what `Motd.cs` asks for,
    character for character.

4. Press **Save draft**. If the JSON has a mistake, the editor marks the line; check for a
   missing comma, or a comma after the last item (see
   [HTTP and APIs](../concepts/http-and-apis.md)).
5. Press **Create version** and give it a short message, such as `First test`. The editor
   shows the new version number, for example **v1**.

Nothing has reached players yet. A version is only a frozen copy; players get it when a
**release** includes it (Step 6).

## Step 4: Make a content pack

A pack is a Godot `.pck` file. Here you'll make one containing a single text file, using a
small script that runs inside the Godot editor, with no command line.

1. **Outside** your project folder, make a folder for pack sources, for example
   `C:\GameContent\first-pack\`. In it, create a text file `note.txt` containing:

    ```text
    This text came from a content pack, delivered by otomo.
    ```

    !!! warning "Keep pack sources outside the project"
        If `note.txt` were inside your project, it would also ship inside the game build,
        and a pack can't replace files that shipped with the game (see
        [Content and patching](../concepts/content-and-patching.md)). Keeping sources
        outside the project folder avoids that trap.

2. In your project, create a new script at `res://tools/make_first_pack.gd` (**GDScript**:
   Godot's editor-script tool is easiest in GDScript, and a C# project can contain
   GDScript files) with:

    ```gdscript
    @tool
    extends EditorScript

    # Runs in the editor when you choose File → Run in the script editor.
    func _run():
        var packer := PCKPacker.new()
        # Where to write the pack. Change the paths to match your folders.
        packer.pck_start("C:/GameContent/first_pack.pck")
        # add_file(path INSIDE the game, file ON YOUR DISK).
        # The first path is what your game will load: res://patch/first/note.txt
        packer.add_file("res://patch/first/note.txt", "C:/GameContent/first-pack/note.txt")
        packer.flush(true)
        print("Pack written to C:/GameContent/first_pack.pck")
    ```

    Note the forward slashes `/` in the paths, even on Windows. Godot accepts them
    everywhere.

3. With the script open in the script editor, choose **File → Run** (or press
   <kbd>Ctrl</kbd>+<kbd>Shift</kbd>+<kbd>X</kbd>). The **Output** panel shows
   `Pack written to …`, and `first_pack.pck` appears in `C:\GameContent\`.

4. Add a second label to your scene, `PackNote`, with this script, so you can see the file
   once the pack arrives:

    ```csharp
    using Godot;

    public partial class PackNote : Label
    {
        public override void _Ready()
        {
            const string path = "res://patch/first/note.txt";
            // The file only exists once the pack has been downloaded and mounted.
            Text = FileAccess.FileExists(path)
                ? FileAccess.GetFileAsString(path)
                : "(no pack yet)";
        }
    }
    ```

Run the game: it shows **(no pack yet)**.

!!! note "Textures, sounds and scenes"
    A plain file like `note.txt` can be packed as-is. Godot resources such as textures and
    scenes are *imported* by the editor first, and the pack must contain the imported
    versions. For those, use Godot's own **Project → Export… → Export PCK/ZIP…** with
    **Export selected resources**, and exclude the same files from your main game export
    so they aren't shipped twice.

## Step 5: Upload the pack

You need **live_ops** or **admin** for this step.

1. In the admin website, open **Packs**.
2. Under **Upload a content pack**, drag `first_pack.pck` into the box, or click
   **choose one** and pick it.
3. Check the name. It's filled in from the file name (`first_pack`). Names may use
   lowercase letters, digits and underscores only.
4. Press **Upload**. When it finishes, the pack appears in the list with its size and
   fingerprint.

## Step 6: Compose and publish a release to live

A release is what players actually receive. It's a whole snapshot, so it must include
everything live should serve, not only your new items.

1. Open **Releases**, choose the **live** channel, and look at the **Channel head** panel: it
   shows what live serves right now. Note its release number; you'll roll back to it in
   Step 8.
2. Press **Compose release**.
3. In **Namespaces**, include `ui.motd` at the version you created (v1, or higher). Keep
   whatever else the current head includes.
4. In **Packs**, tick `first_pack`. Keep any packs the current head has.
5. Leave **Minimum client version** as it is (for example `0.0.0` or `1.0.0`). Raising it
   would lock out every game build older than that version.
6. In **Message**, write what this release is: `First content test (will roll back)`.
7. Check the **Preview**: under **Client manifest** you should see `ui.motd` and
   `first_pack`.
8. Type `live` in the confirmation box (it reads **Type live to publish to players**) and
   press **Publish release**.

The **Releases** page now shows your release as the head of `live`, with a new, higher
release number.

## Step 7: See it in the game

1. Run your game from the editor.
2. Watch the **Output** panel. This time the SDK finds a new release:

    ```text
    patch: Checking
    patch: Downloading
    patch: Verifying
    patch: Mounting
    patch: Ready
    patch: ready (release_id=…)
    ```

3. The labels now show **Hello from the admin website!** in light blue, and **This text
   came from a content pack, delivered by otomo.**

Everything on screen came from the server: the message and colour from the `ui.motd`
settings document, and the text from inside the downloaded pack.

4. **Run the game once more.** This time there's no `Downloading` line. The SDK asked
   whether anything changed, Patch answered `304 Not Modified`, and the game used the files
   it had already verified. This is what nearly every player sees on nearly every launch:
   a check that costs almost nothing.

!!! tip "Where the files went"
    In Godot, **Project → Open User Data Folder**, then `otomo/patch/blobs/`. The
    downloaded files are there, named by their SHA-256 fingerprints.

## Step 8: Roll back

Put `live` back exactly as it was.

1. In the admin website, open **Releases** → **live**.
2. In the release history, find the release that was the head before yours (the number
   you noted in Step 6) and press **Roll back** on it.
3. Confirm. The channel head is that older release again.
4. Run the game. The labels go back to **Welcome!** and **(no pack yet)**, and the SDK
   deletes the downloaded files the release no longer lists.

Nothing was deleted from the admin website: your namespace version and pack are still
there, ready to be included in a future release.

## What you've learned

- Game code reads settings with `RemoteConfig`, always with a fallback.
- A settings change is: edit draft → **Save draft** → **Create version** → include in a
  **release** → **publish**.
- A pack is built with Godot's `PCKPacker` (or export), uploaded under **Packs**, and
  included in a release.
- Players get a release the next time their game starts; an unchanged release costs one
  tiny `304` check.
- **Roll back** undoes a release by pointing the channel at an older one.

## Next

- [Build a download screen](download-screen.md), so players see progress when a big
  release downloads.
- [SDK reference](../reference/sdk.md): every `RemoteConfig` function.
