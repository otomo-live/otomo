# Build a download screen

When a release contains large packs, the game might download for a while before the main
menu appears. This guide builds a simple download screen: a message saying how much will be
downloaded, a progress bar with speed and time left, and optionally an "are you sure?"
question before a big download. It takes about 20 minutes.

**You'll need:** the SDK installed ([Install the SDK](install-the-sdk.md)). To see real
progress, you'll also need a release with a pack in it
([Publish your first content update](first-patch.md)).

## How the SDK reports progress

While `StartAsync` patches, the `Otomo` autoload emits **signals**, Godot's usual way for
a node to announce that something happened. Your screen **connects** to them: it gives
the SDK a function to call each time.

They come in this order:

```
DownloadPlanned ──▶ (DownloadConfirmationRequested) ──▶ DownloadProgress × many ──▶ PatchFinished
   "here's what           "is this OK?"                   "this far, this fast"       "done, and
    I'll download"         (only if you ask for it)                                    here's how"
```

| Signal | When | What it tells you |
|---|---|---|
| `DownloadPlanned` | Once, before downloading anything | The release number, how many files, how many bytes to download, and how many bytes were already downloaded last time (the download resumes from there). **0 files means the game is up to date.** |
| `DownloadConfirmationRequested` | Only if you turned it on (Step 4) and the download is big | How many bytes and files. The download waits until you answer. |
| `DownloadProgress` | At most 10 times a second, and after each finished file | Bytes and files done out of the total, the current file's name, the speed in bytes per second, and the seconds left (`-1` while unknown). |
| `PatchFinished` | Once, at the end | The result, how much was downloaded, how long it took, and an error code if something failed. |

[SDK reference](../reference/sdk.md) lists every argument.

## Step 1: Make the scene

1. Create a new scene with a **Control** root node named `Boot`. This will be the first
   scene of your game.
2. Add two children to `Boot`:
    - a **Label** named `Status`;
    - a **ProgressBar** named `Bar`.
3. Arrange them however you like, for example centred, with the label above the bar.
4. Set this scene as the main scene: **Project → Project Settings → General →
   Application › Run › Main Scene**.

## Step 2: The script

Attach a C# script, `Boot.cs`, to the `Boot` node:

```csharp
using Godot;
using OtomoSdk;
using OtomoSdk.Patch;

public partial class Boot : Control
{
    private Label _status;
    private ProgressBar _bar;

    public override async void _Ready()
    {
        _status = GetNode<Label>("Status");
        _bar = GetNode<ProgressBar>("Bar");
        _bar.Visible = false;
        _status.Text = "Checking for updates…";

        var otomo = Otomo.Instance;

        // Connect to the signals BEFORE starting, so we don't miss any.
        otomo.DownloadPlanned += OnPlanned;
        otomo.DownloadProgress += OnProgress;
        otomo.PatchFinished += OnFinished;

        StartResult result = await otomo.StartAsync();

        // Disconnect: this scene is about to go away, and the autoload stays.
        otomo.DownloadPlanned -= OnPlanned;
        otomo.DownloadProgress -= OnProgress;
        otomo.PatchFinished -= OnFinished;

        if (result.Patch == PatchResult.ClientTooOld)
        {
            _status.Text = "A new version of the game is available. Please update.";
            return;
        }

        GetTree().ChangeSceneToFile("res://scenes/MainMenu.tscn");
    }

    private void OnPlanned(long releaseId, int fileCount, long bytesToDownload, long bytesAlreadyHave)
    {
        if (fileCount == 0)
        {
            _status.Text = "Up to date";
            return;
        }
        // OtomoFormat.Bytes turns 12900000 into "12.3 MB".
        _status.Text = $"Downloading update: {OtomoFormat.Bytes(bytesToDownload)}";
        _bar.Visible = true;
    }

    private void OnProgress(long bytesDone, long bytesTotal, int filesDone, int filesTotal,
                            string currentFile, double bytesPerSecond, double etaSeconds)
    {
        _bar.Value = bytesTotal > 0 ? 100.0 * bytesDone / bytesTotal : 0;

        string speed = OtomoFormat.Bytes((long)bytesPerSecond) + "/s";
        // etaSeconds is -1 until the SDK has measured the speed; Duration(null) shows "--".
        string timeLeft = OtomoFormat.Duration(etaSeconds < 0 ? null : etaSeconds);
        _status.Text = $"{OtomoFormat.Bytes(bytesDone)} of {OtomoFormat.Bytes(bytesTotal)} " +
                       $"({speed}, {timeLeft} left)";
    }

    private void OnFinished(string result, long bytesDownloaded, int filesDownloaded,
                            double seconds, string errorCode)
    {
        _bar.Visible = false;
        if (result == "OfflineReady")
            _status.Text = "Offline: playing with the content from last time.";
    }
}
```

What's happening:

- **`+=` connects a signal to a function.** `otomo.DownloadPlanned += OnPlanned;` means
  "whenever `DownloadPlanned` fires, call my `OnPlanned` function". The function's
  parameters must match the signal's arguments, in the same order.
- **`-=` disconnects it.** The `Otomo` autoload lives for the whole game, but this scene
  doesn't. If you forget to disconnect, the autoload would later try to call functions on
  a scene that no longer exists, and Godot reports an error.
- **`result` in `OnFinished`** is the name of the outcome as text: `Ready`,
  `OfflineReady`, `ClientTooOld` or `Declined`.

## Step 3: Try it

- **With nothing new to download**, the screen shows "Up to date" briefly and moves on.
- **To see a real download**, publish a release with a pack, as in
  [Publish your first content update](first-patch.md), and run the game. To see it
  again without publishing anything, delete the downloaded files: **Project → Open User
  Data Folder**, then delete `otomo/patch/` and run again.

## Step 4 (optional): Ask before big downloads

On mobile data, players appreciate being asked before a large download. The SDK can pause
before downloading and wait for your answer.

1. In **Project Settings → Otomo › Config**, set **Confirm Download Over Bytes** to a
   size in bytes. For example `52428800` is 50 MB (50 × 1024 × 1024). Then:
    - downloads **larger** than this ask first;
    - `0` asks before every download;
    - `-1` (the default) never asks.
2. Add a **ConfirmationDialog** node named `Ask` to your `Boot` scene.
3. Add this to `Boot.cs`:

    ```csharp
    // In _Ready, next to the other connections:
    //   otomo.DownloadConfirmationRequested += OnConfirm;
    // and disconnect it again after StartAsync, like the others.

    private void OnConfirm(long bytesToDownload, int fileCount)
    {
        var ask = GetNode<ConfirmationDialog>("Ask");
        ask.DialogText = $"Download {OtomoFormat.Bytes(bytesToDownload)} of new content?";
        // OK → download. Cancel → play with what's already on this device.
        ask.Confirmed += () => Otomo.Instance.RespondToDownload(true);
        ask.Canceled += () => Otomo.Instance.RespondToDownload(false);
        ask.PopupCentered();
    }
    ```

If the player declines, nothing is downloaded, the game keeps the content it had, and
`PatchFinished` reports `Declined`. The game then carries on to log in as usual.

!!! warning "Always answer"
    While the SDK waits for `RespondToDownload`, `StartAsync` doesn't finish. Make sure
    every way of closing your dialog calls `RespondToDownload` with `true` or `false`.

## Polling instead of signals

If your UI prefers to update in `_Process` every frame, read
`Otomo.Instance.CurrentDownload` instead of connecting to `DownloadProgress`. It holds the
same numbers (`BytesDone`, `BytesTotal`, `FilesDone`, `FilesTotal`, `CurrentFile`,
`BytesPerSecond`, `EtaSeconds`). `Otomo.Instance.LastPlan` holds the last plan.

## Next

[SDK reference](../reference/sdk.md) has every signal and property in detail.
