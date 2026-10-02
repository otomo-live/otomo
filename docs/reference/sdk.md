# SDK reference

Everything the otomo Godot SDK offers your game code, in one page. For step-by-step use,
see the guides: [Install the SDK](../guide/install-the-sdk.md),
[Publish your first content update](../guide/first-patch.md) and
[Build a download screen](../guide/download-screen.md).

All types are in the **`OtomoSdk`** namespace (C#'s way of grouping names, unrelated to
otomo's settings namespaces), or a sub-namespace such as `OtomoSdk.Patch`. Add the
matching `using` lines at the top of your script:

```csharp
using OtomoSdk;           // Otomo, StartResult, SessionStatus, OtomoFormat
using OtomoSdk.Patch;     // PatchResult, RemoteConfig, PatchProgress, PatchPlan
using OtomoSdk.Http;      // OtomoError
using OtomoSdk.Session;   // PlayerProfile
```

You only need the lines for the types your script names. `Otomo.Instance.RemoteConfig`
works with just `using OtomoSdk;`.

**On this page:**
[Project settings](#project-settings) ·
[The Otomo autoload](#the-otomo-autoload) ·
[Signals](#signals) ·
[StartResult](#startresult) ·
[RemoteConfig](#remoteconfig) ·
[OtomoFormat](#otomoformat) ·
[OtomoError](#otomoerror) ·
[Progress types](#progress-types) ·
[Advanced: the sub-clients](#advanced-the-sub-clients) ·
[Files on the player's computer](#files-on-the-players-computer)

---

## Project settings

Under **Project Settings › General › Otomo › Config** (switch on **Advanced Settings** to
see them). Enabling the plugin creates them.

| Setting | Type | Default | Meaning |
|---|---|---|---|
| `otomo/config/base_url` | text | `http://localhost:8080` | The address of your otomo's gateway, such as `https://play.example.com`. The SDK stops with an error at start-up if it's empty. |
| `otomo/config/client_version` | text | *(empty)* | This build's version, `major.minor.patch` such as `1.2.0`. Empty means: use `application/config/version`, or `0.0.0`. Compared with a release's minimum client version. |
| `otomo/config/channel` | text | `live` | Which content channel to download. Players can only read `live`. |
| `otomo/config/verbose_log` | on/off | on | Print the SDK's steps to the Output panel. Secrets are never printed. |
| `otomo/config/confirm_download_over_bytes` | number | `-1` | Ask before downloading more than this many bytes: `-1` never asks, `0` always asks. See `DownloadConfirmationRequested`. |

**Overriding the address for one run.** Launching the game with
`-- --otomo-base-url=<address>` on the command line uses that address instead of the
setting, for that run only. This is useful with an SSH tunnel:

```bat
Godot_v4.7.2_console.exe --path C:\MyGame -- --otomo-base-url=http://localhost:8080
```

**A second player on the same PC.** The SDK keeps the player's identity (the device id) and
tokens in `user://otomo`, which every copy of the game on one PC shares, so they all sign in
as the same player. Launching with `-- --otomo-data-dir=<folder>` keeps that run's files in
another folder, so it signs in as a new player. This is for testing only: it works in the
editor and in debug exports, and a release export ignores it. The folder can be a `user://` path or an
absolute one. In the editor, open **Debug › Customize Run Instances**, set **Run Instances**
to 2, and give instance 2 the launch arguments `-- --otomo-data-dir=user://otomo-p2` (the first `--` separates the game's own arguments from Godot's). Pressing
Play then starts two players who can befriend and invite each other.

---

## The Otomo autoload

The plugin adds a node named `Otomo` that Godot creates at start-up and keeps for the
whole game. Reach it from any C# script as `Otomo.Instance`.

### Otomo.Instance

The one `Otomo` node.

```csharp
public static Otomo Instance { get; }
```

It's `null` only if the plugin isn't enabled, or before autoloads have been created (so
not in any scene's `_Ready`).

### StartAsync

Runs otomo's start-up sequence: **patch**, then **log in**, then create or fetch the
player's **profile**. Call it once, from your first scene, and `await` it before loading
any scene that uses patched content.

```csharp
public Task<StartResult> StartAsync(CancellationToken ct = default)
```

| Argument | Meaning |
|---|---|
| `ct` | Optional. Lets you cancel the sequence (for example if the player quits during a download). Leave it out normally. |

**Returns** a [`StartResult`](#startresult) saying how far it got. It doesn't throw when
the server can't be reached; check the result instead.

```csharp
StartResult result = await Otomo.Instance.StartAsync();
if (result.Patch == PatchResult.ClientTooOld) { /* ask the player to update the game */ }
else if (!result.LoggedIn) { /* offline: the game still has last time's content */ }
```

Calling it again later, for example after `AuthenticationLost`, runs the sequence again.
Packs already mounted aren't mounted twice.

### JoinMatchAsync

```csharp
Task<JoinHandshakeResult> JoinMatchAsync(string address, int port, string ticket, CancellationToken ct = default)
```

Shows the Gameplay Proxy a join ticket (the values from the `PartyLaunching` signal) and
waits for its answer. If the ticket had expired or was already used, it fetches a fresh one
and tries once more. The result's `Outcome` is `Accepted`, `Refused` (with a
`RefusalReason`) or `NoAnswer`. On `Accepted`, connect ENet to the result's `Address` and
`Port` **from** its `LocalPort`. See [Join a match](../guide/joining-a-match.md).

### SetPresenceStatusAsync

```csharp
Task SetPresenceStatusAsync(PresenceStatus status, CancellationToken ct = default)
```

Changes the status friends see: online (the default while logged in), in a menu, or away.
The heartbeat that keeps the player "online" starts and stops by itself with login.

### RemoteConfig

The settings documents from the current release. See [RemoteConfig](#remoteconfig).

```csharp
public RemoteConfig RemoteConfig { get; }
```

### RespondToDownload

Answers a `DownloadConfirmationRequested` signal.

```csharp
public void RespondToDownload(bool accept)
```

| Argument | Meaning |
|---|---|
| `accept` | `true` downloads the release. `false` skips it: the game keeps the content it already has, and the patch result is `Declined`. |

Calling it when nothing is waiting does nothing.

### CurrentDownload

The latest download progress, for UI that updates every frame in `_Process` instead of
using the `DownloadProgress` signal.

```csharp
public PatchProgress CurrentDownload { get; }
```

See [PatchProgress](#progress-types).

### LastPlan

The last `DownloadPlanned` information, or `null` before the first patch.

```csharp
public PatchPlan LastPlan { get; }
```

### Client

The SDK's inner parts, for advanced use. See
[Advanced: the sub-clients](#advanced-the-sub-clients).

```csharp
public OtomoClient Client { get; }
```

---

## Signals

Connect with `+=` and a function whose parameters match, and disconnect with `-=` when
your node leaves the scene tree:

```csharp
public override void _EnterTree() => Otomo.Instance.LoggedIn += OnLoggedIn;
public override void _ExitTree()  => Otomo.Instance.LoggedIn -= OnLoggedIn;
private void OnLoggedIn() => GD.Print("Logged in!");
```

### Download signals

Emitted during `StartAsync`, in this order:
`DownloadPlanned` → (`DownloadConfirmationRequested`) → `DownloadProgress` (repeatedly) →
`PatchFinished`.

**DownloadPlanned**: once, after checking and before downloading anything. Not emitted
when offline or when the build is too old.

| Argument | Type | Meaning |
|---|---|---|
| `releaseId` | `long` | The release being downloaded |
| `fileCount` | `int` | Files still to download. **0 means up to date.** |
| `bytesToDownload` | `long` | Bytes still to download |
| `bytesAlreadyHave` | `long` | Bytes already on disk from an interrupted download |

**DownloadConfirmationRequested**: only when `confirm_download_over_bytes` is `0` or
more, and the download is larger than that. The download waits until you call
`RespondToDownload`.

| Argument | Type | Meaning |
|---|---|---|
| `bytesToDownload` | `long` | Bytes to download |
| `fileCount` | `int` | Files to download |

**DownloadProgress**: at most every 100 milliseconds, and after every finished file.

| Argument | Type | Meaning |
|---|---|---|
| `bytesDone` | `long` | Bytes downloaded so far |
| `bytesTotal` | `long` | Bytes in the whole download |
| `filesDone` | `int` | Files finished |
| `filesTotal` | `int` | Files in the download |
| `currentFile` | `string` | The name of the file being downloaded |
| `bytesPerSecond` | `double` | Current speed |
| `etaSeconds` | `double` | Seconds left; **`-1` while unknown** |

**PatchFinished**: once, at the end of patching.

| Argument | Type | Meaning |
|---|---|---|
| `result` | `string` | `Ready`, `OfflineReady`, `ClientTooOld` or `Declined` (see [PatchResult](#startresult)) |
| `bytesDownloaded` | `long` | Bytes actually downloaded |
| `filesDownloaded` | `int` | Files actually downloaded |
| `seconds` | `double` | How long patching took |
| `errorCode` | `string` | Empty on success, otherwise an [error code](errors.md) such as `network` |

### Other signals

| Signal | Arguments | When |
|---|---|---|
| `PatchStateChanged` | `string state` | Patching moved to another step: `Checking`, `Downloading`, `Verifying`, `Mounting`, `Ready`, `OfflineReady`, `ClientTooOld` |
| `PatchProgress` | `long done, long total` | Simple byte progress. `DownloadProgress` gives more detail. |
| `RemoteConfigChanged` | *(none)* | New settings documents were loaded. Re-read any values you cached. |
| `LoggedIn` | *(none)* | The player is logged in |
| `LoggedOut` | *(none)* | The player logged out |
| `AuthenticationLost` | `string code, string requestId` | A request was refused even after renewing the login. Return to your start screen and call `StartAsync` again. See [Logins and tokens](../concepts/logins-and-tokens.md). |
| `SessionEventReceived` | `long seq, long at, string type, string payloadJson` | One event from Session (a friend came online, the party changed, an invite arrived). `at` is Unix milliseconds; `payloadJson` is the raw payload or empty. |
| `Resync` | *(none)* | Some events were missed (the game was offline, or Session restarted). Re-fetch the friends list and party with normal calls. |
| `PartyLaunching` | `string allocationId, string address, int port, string ticket, long ticketExpiresAtUnixMs` | A game server was reserved for this player. Pass `address`, `port` and `ticket` to `JoinMatchAsync`. See [Join a match](../guide/joining-a-match.md). |
| `PartyLaunchFailed` | `string reason` | No game server was available: `no_capacity` or `allocator_unavailable`. The lobby is forming again. |
| `PartyReturned` | `string reason` | The match is over (`ended`, `expired`, `server_dead` or `server_restarted`). The lobby is forming again with ready flags cleared. |

---

## StartResult

What `StartAsync` returns.

| Property | Type | Meaning |
|---|---|---|
| `Patch` | `PatchResult` | How patching ended (below) |
| `LoggedIn` | `bool` | Whether the player is logged in |
| `Profile` | `PlayerProfile` | The player's profile, or `null` if Session couldn't be reached |
| `Session` | `SessionStatus` | How the Session step went (below) |
| `Error` | `OtomoError` | What stopped the sequence, or `null` if nothing did |

**PatchResult** (in `OtomoSdk.Patch`):

| Value | Meaning | What your game should do |
|---|---|---|
| `Ready` | The newest content is downloaded and loaded | Carry on |
| `OfflineReady` | Otomo couldn't be reached; the last good content is loaded (or none, on a first run) | Carry on; online features won't work |
| `ClientTooOld` | This build is older than the release's minimum client version. Nothing was loaded. | Tell the player to update the game |
| `Declined` | The player declined the download; the old content is loaded | Carry on |

**SessionStatus**:

| Value | Meaning |
|---|---|
| `Available` | Session answered, and `Profile` is set |
| `NotAttempted` | Session wasn't tried, because the login failed |
| `Failed` | Session was tried and failed; see `Error` |
| `NotImplemented` | Left over from early development; current servers never cause it |

**PlayerProfile**:

| Property | Type | Example |
|---|---|---|
| `PlayerId` | `string` | `01a0ecdd-ed9c-758a-…` (a unique ID) |
| `DisplayName` | `string` | `Player1676` (a temporary name until the player chooses one) |
| `Discriminator` | `int` | `9062`, the number that tells apart players with the same name |

`profile.ToString()` gives `Player1676#9062`.

---

## RemoteConfig

The client settings documents of the current release. Each `Get…` function takes the
**namespace** (the document's name), a **key**, and a **fallback** returned when the value
is missing or has the wrong type. It never throws.

**Keys with dots** reach into nested objects: with the document
`{"spells": {"fireball": {"damage": 20}}}`, the key `spells.fireball.damage` gives `20`.
An empty key `""` means the whole document.

### GetInt, GetFloat, GetBool, GetString

```csharp
public int    GetInt   (string doc, string key, int fallback)
public float  GetFloat (string doc, string key, float fallback)
public bool   GetBool  (string doc, string key, bool fallback)
public string GetString(string doc, string key, string fallback)
```

| Argument | Meaning |
|---|---|
| `doc` | The namespace, such as `"balance.player"` |
| `key` | The key inside it, such as `"max_hp"` or `"spells.fireball.damage"` |
| `fallback` | Returned if the namespace or key doesn't exist, or the value is the wrong kind (text where a number was expected) |

```csharp
var config = Otomo.Instance.RemoteConfig;
int maxHp = config.GetInt("balance.player", "max_hp", 100);
```

`GetInt` returns the fallback for a number with decimals (`5.5`); use `GetFloat` for those.

### TryGet

For values the `Get…` functions don't cover, such as lists. Returns `true` and the raw
JSON value if it exists.

```csharp
public bool TryGet(string doc, string key, out System.Text.Json.JsonElement value)
```

```csharp
if (config.TryGet("events.winter", "regions", out var regions))
    foreach (var region in regions.EnumerateArray())
        GD.Print(region.GetString());
```

### Names

The names of all loaded documents.

```csharp
public IReadOnlyCollection<string> Names { get; }
```

---

## OtomoFormat

Helpers for showing sizes and times to players.

```csharp
public static string OtomoFormat.Bytes(long bytes)
public static string OtomoFormat.Duration(double? seconds)
```

| Call | Result |
|---|---|
| `OtomoFormat.Bytes(0)` | `0 B` |
| `OtomoFormat.Bytes(1536)` | `1.5 KB` |
| `OtomoFormat.Bytes(12_900_000)` | `12.3 MB` |
| `OtomoFormat.Duration(null)` | `--` (unknown) |
| `OtomoFormat.Duration(12)` | `12 s` |
| `OtomoFormat.Duration(185)` | `3 min 5 s` |
| `OtomoFormat.Duration(3720)` | `1 h 2 min` |

Sizes are in steps of 1024.

---

## OtomoError

Every problem the SDK reports is an `OtomoError`, a C# exception with these properties:

| Property | Type | Meaning |
|---|---|---|
| `Status` | `int` | The HTTP status code, or `0` if the server never answered (no connection, timeout) |
| `Code` | `string` | A short, fixed word such as `network` or `not_in_party`. See [Error codes](errors.md). |
| `Message` | `string` | A sentence for humans |
| `RequestId` | `string` | The request's ID in the server logs. **Include it in bug reports.** |
| `IsRetryableLater` | `bool` | `true` when trying again later might work (no connection, `429`, `5xx`) |

---

## Progress types

In `OtomoSdk.Patch`.

**PatchProgress** (from `CurrentDownload`): `BytesDone`, `BytesTotal`, `FilesDone`,
`FilesTotal`, `CurrentFile`, `BytesPerSecond`, and `EtaSeconds` (`null` while unknown).
The same values as the `DownloadProgress` signal.

**PatchPlan** (from `LastPlan`): `ReleaseId`, `FileCount`, `TotalBytes`,
`AlreadyHaveBytes`, `BytesToDownload`, `IsUpToDate`, and `Files`, the list of files to
download, each with its `Kind` (`config` or `pack`), `Name`, `Sha256`, `Size` and
`AlreadyHave` (bytes already on disk).

---

## Advanced: the sub-clients

`Otomo.Instance.Client` holds the parts the autoload is built from. Most games never need
them.

| Property | What it is | Useful members |
|---|---|---|
| `Patch` | The updater | `RunAsync()`: check for new content again (for example every few minutes in the main menu). `RequiredClientVersion`: the minimum version when the result was `ClientTooOld`. `ConfirmDownload`: your own async "ask the player" function, replacing the project setting. |
| `Auth` | Logins | `IsLoggedIn`. `LogoutAsync()`: log out and forget the saved login (the device ID is kept). |
| `Session` | Session calls | `GetMeAsync()`: the player's profile. |
| `Presence` | Online status | Started and stopped automatically with login. Change what it reports with `Otomo.Instance.SetPresenceStatusAsync`. |
| `Friends` | Friends and blocks | `ListAsync()`, `RequestAsync(displayName, discriminator)`, `AcceptAsync(playerId)` and more. |
| `Party` | Parties and the lobby | `CreateAsync()`, `GetAsync()`, `InviteAsync`, `AcceptInviteAsync`, `LeaveAsync`, `KickAsync`, `PromoteAsync`, `UpdateSettingsAsync`, `SetReadyAsync`, `LaunchAsync`, `GetLaunchTicketAsync`. See [Join a match](../guide/joining-a-match.md). |
| `Launch` | The Gameplay Proxy handshake | `JoinAsync(info)`, which `Otomo.Instance.JoinMatchAsync` wraps. |
| `Http` | The shared HTTP sender | `SendAsync<T>(method, url, body)`: an authenticated request to any otomo route, with the login renewed automatically. |

```csharp
// Check for new content again, from the main menu.
PatchResult r = await Otomo.Instance.Client.Patch.RunAsync();
```

---

## Files on the player's computer

In the game's user data folder (**Project → Open User Data Folder** in the editor), under
`otomo/`:

| Path | Contents | If deleted |
|---|---|---|
| `device_id` | The player's identity. **It is the account.** | A new, empty account is created |
| `refresh_token` | The saved login | The game logs in again with `device_id` |
| `patch/manifest.json` | The last good release | Downloaded again |
| `patch/blobs/` | Downloaded files, named by fingerprint | Downloaded again |
