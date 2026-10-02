# Otomo Godot SDK (C#)

Client SDK for Godot .NET 4.x games talking to Otomo. The user documentation (installing,
patching, download screens, the full API) is on the technical wiki: **Guides** and
**Reference › SDK reference** (`docs/guide/`, `docs/reference/sdk.md`). The protocol it
implements is [`design/12-godot-sdk-guide.md`](../design/12-godot-sdk-guide.md).

| Step | Server | In the SDK |
|---|---|---|
| Patch | LIVE | `PatchClient`: manifest with ETag/304, client-version gate, resumable SHA-256-verified downloads, config documents (`RemoteConfig`), pack mounting, last-good cache and offline start, download plan/progress/confirmation hooks |
| Auth | LIVE | `AuthClient`: device ID, anonymous login, single-flight refresh with rotation, logout |
| Session | LIVE (every route) | `SessionClient`: `POST /me/init`, `GET /me`. Presence, events, friends, parties, lobby and launch: see the SDK reference |
| Launch | LIVE (Allocator, Gameplay Proxy, game servers) | Not yet. The game repository's `Scripts/Network/OtomoLaunch.cs` is the working stopgap |

## Install

1. Copy `addons/otomo/` into the game's `addons/` folder.
2. Build the C# project once (the editor plugin is C#).
3. Project → Project Settings → Plugins → enable **Otomo SDK**. This adds the `Otomo`
   autoload and the settings below.

| Project setting | Default | |
|---|---|---|
| `otomo/config/base_url` | `http://localhost:8080` | Gateway base URL: your otomo's public HTTPS address, e.g. `https://play.example.com`. Override per run with `-- --otomo-base-url=<url>` |
| `otomo/config/client_version` | *(empty: uses `application/config/version`, else `0.0.0`)* | Game executable version, compared with the release's `min_client_version` |
| `otomo/config/channel` | `live` | Players always use `live` |
| `otomo/config/verbose_log` | `true` | SDK diagnostics to the Godot output. Secrets are never logged |
| `otomo/config/confirm_download_over_bytes` | `-1` | Ask the player before downloading more than this many bytes (`-1` never, `0` always); see below |

## Use

```csharp
using OtomoSdk;
using OtomoSdk.Patch;

public override async void _Ready()
{
    try
    {
        var r = await Otomo.Instance.StartAsync();        // Patch → Auth → Session /me/init
        if (r.Patch == PatchResult.ClientTooOld) { ShowUpdateGameScreen(Otomo.Instance.Client.Patch.RequiredClientVersion); return; }
        if (!r.LoggedIn) { ShowOffline(r.Error); return; }
        var maxHp = Otomo.Instance.RemoteConfig.GetFloat("balance.player", "max_hp", 100f);
        // …main menu
    }
    catch (Exception e) { GD.PushError(e.ToString()); }
}
```

Call `StartAsync` before loading any scene that uses patched content. Signals on the
autoload: `PatchStateChanged`, `PatchProgress`, `RemoteConfigChanged`, `LoggedIn`,
`LoggedOut`, and `AuthenticationLost` (a request still got 401 after a refresh: go back to the
login state). Sub-clients are on `Otomo.Instance.Client` (`Patch`, `Auth`, `Session`, `Http`).

Files live in `user://otomo/`: `device_id` and `refresh_token` (secrets: they are the
player's account), and `patch/` (manifest, ETag, verified blobs).

### A download screen

`StartAsync` patches first. These signals on the autoload drive a launcher-style screen:
plan, then an optional confirmation, then progress, then the finish.

| Signal | When | Arguments |
|---|---|---|
| `DownloadPlanned` | once per run, before any download (`fileCount` 0 = up to date) | `releaseId`, `fileCount`, `bytesToDownload`, `bytesAlreadyHave` (resumed) |
| `DownloadConfirmationRequested` | only with `confirm_download_over_bytes` ≥ 0 and a bigger download | `bytesToDownload`, `fileCount`; answer with `Otomo.Instance.RespondToDownload(bool)` |
| `DownloadProgress` | at most every 100 ms, and on every finished file | `bytesDone`, `bytesTotal`, `filesDone`, `filesTotal`, `currentFile`, `bytesPerSecond`, `etaSeconds` (-1 = unknown) |
| `PatchFinished` | once per run | `result` (`Ready`, `OfflineReady`, `ClientTooOld`, `Declined`), `bytesDownloaded`, `filesDownloaded`, `seconds`, `errorCode` |

```csharp
var otomo = Otomo.Instance;
otomo.DownloadPlanned += (release, files, bytes, _) =>
    _label.Text = files == 0 ? "Up to date" : $"Update: {OtomoFormat.Bytes(bytes)}";
otomo.DownloadConfirmationRequested += (bytes, files) =>
    ShowDialog($"Download {OtomoFormat.Bytes(bytes)}?", yes => otomo.RespondToDownload(yes));
otomo.DownloadProgress += (done, total, _, _, file, bps, eta) =>
{
    _bar.Value = total > 0 ? 100.0 * done / total : 0;
    _label.Text = $"{file}: {OtomoFormat.Bytes(done)} / {OtomoFormat.Bytes(total)}, " +
                  $"{OtomoFormat.Bytes((long)bps)}/s, {OtomoFormat.Duration(eta < 0 ? null : eta)} left";
};
var result = await otomo.StartAsync();
```

Declining keeps the last good release on disk; the result is `Declined` and the game carries
on to login. A UI that polls instead can read `Otomo.Instance.CurrentDownload` (a
`PatchProgress` snapshot) and `LastPlan`. C# games can also set
`Otomo.Instance.Client.Patch.ConfirmDownload` to their own async check; that replaces the
project-setting hook.

## Check a build reaches Otomo

Run `res://addons/otomo/Diagnostics/OtomoSmokeTest.tscn` (F6), or headless:

```sh
godot --headless --path <game> res://addons/otomo/Diagnostics/OtomoSmokeTest.tscn -- --otomo-smoke-quit
```

It prints each step and exits 0 when patched and logged in.

## Layout and tests

```
addons/otomo/
  Otomo.cs, OtomoPlugin.cs, plugin.cfg   Godot glue: autoload, settings, pack mounting
  Diagnostics/                           smoke-test scene
  Core/                                  plain .NET, no Godot APIs
    OtomoConfig.cs, OtomoClient.cs       configuration, composition + start-up sequence
    Http/                                OtomoHttp (errors, 401 refresh-once, timeouts), Backoff
    Auth/                                AuthClient, FileSecretStore
    Patch/                               PatchClient, RemoteConfig, VersionCompare
    Session/                             SessionClient
tests/Otomo.Sdk.Tests/                   xUnit tests for Core (fake HTTP handler)
```

Core has no Godot dependency, so it is tested with plain .NET 8:

```sh
cd tests/Otomo.Sdk.Tests
dotnet test
OTOMO_LIVE_BASE_URL=https://play.example.com dotnet test --filter Live   # against a real gateway
```

Core rules (guide §3.2, §11): one shared `HttpClient`; never `.Result`/`.Wait()`; no
`ConfigureAwait(false)` (continuations must come back to Godot's main thread); every error is
an `OtomoError` with `Status`, `Code` and `RequestId`.
