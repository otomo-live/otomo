# Otomo Godot SDK (C#): implementation guide

**For:** the programmer writing the Otomo client SDK in **Godot .NET (C#)**.
**Assumes:** you know C# and Godot. It does **not** assume you know how web servers,
HTTP or "REST APIs" work, and you never need to read the Go code of the backend.
Everything the SDK needs to know about the server is in this document.

**Status of each part.** Every section is tagged:

| Tag | Meaning |
|---|---|
| **LIVE** | Running on the server today. You can build and test against it now. |
| **PLANNED** | Designed, not built yet. Field names may still change; code against an interface so a rename is a one-line change. |
| **DESIGN PENDING** | Agreed in principle, details not written yet. Leave a clear seam in the SDK; do not build the details. |

**The whole server side is built**: Patch and Auth behind the public edge, every Session
route (§7), and the Allocator, the Gameplay Proxy and a pool of game servers (§8). A
Windows client goes from patch to a running match through the proxy.

**The SDK exists.** `godot-plugin/addons/otomo` implements Patch (§5), Auth (§6), Session
(§7: profiles, presence, events, friends, parties and the lobby) and launch with the proxy
handshake (§8), plus download-progress signals for game UI. It sends `X-Otomo-Release` on
every Session call (§7.5). Where this guide's code samples and the SDK differ in names or
signatures, the SDK is the reference: the samples explain the protocol, the SDK is what to
ship.

Still open: Session lets requests without `X-Otomo-Release` through unless
`SESSION_REQUIRE_RELEASE_HEADER=true`, and the game server's own ticket check (§8.4 step 4)
is the game's code, not otomo's.

Related design docs (you do not need them to follow this guide, but they are the
source of truth when something here is unclear): `03-patch-minimal.md` (Patch),
`06-auth-identity-contract.md` and `07-auth-techspec.md` (Auth), `04-session-minimal.md`
(Session), `05-gateway-techspec.md` (Gateway), `14-launch-handoff.md` (lobby, launch and the
join ticket).

---

## 1. The big picture

A player's client talks to Otomo in four steps, always in this order:

```
 ┌──────────┐   1. Patch      "Am I up to date? Give me new content and settings."
 │  Godot   │   2. Auth       "This is my device. Give me a login token."
 │  client  │   3. Session    "Here is my token. Show my profile, friends, party/lobby."
 │  (SDK)   │   4. Launch     "My lobby is starting. Which game server do I join?"
 └────┬─────┘
      │  every request goes to ONE address: the Gateway
      ▼
 ┌──────────┐      ┌───────┐ ┌──────┐ ┌─────────┐ ┌───────────┐
 │ Gateway  │ ───► │ Patch │ │ Auth │ │ Session │ │ Allocator │ ──► game server
 └──────────┘      └───────┘ └──────┘ └─────────┘ └───────────┘     (headless Godot)
```

1. **Patch (LIVE).** The first thing the client does, before the login screen. It
   compares what the client has with the current *release* and downloads anything new:
   content packs (Godot `.pck` files) and configuration documents (balance numbers,
   event settings, message of the day…). No login is needed for this.
2. **Auth (LIVE).** The client proves who it is (in M1: with a random device ID it
   generated on first launch) and receives a **token**, a string it attaches to every
   later request.
3. **Session (LIVE).** Everything the player does outside a match: profile and name,
   online status, friends, blocks, and the party/lobby they are in. Session also sends
   the client **events** ("you got a friend request", "your lobby changed").
4. **Launch (LIVE).** When the lobby leader starts the game, Session asks the
   **Allocator** for a game server. Every lobby member then receives, through Session's
   events, the **Gameplay Proxy's** address and their own short-lived **join ticket**.
   They present the ticket to the proxy, and connect through it to the game server with
   Godot's multiplayer (§8).

**The client only ever knows one HTTP address: the Gateway** (in production,
`https://play.example.com`, reached through a TLS edge). The one exception is the match
itself, which uses the UDP address Session hands out at launch. It never talks to Patch, Auth
or Session directly, and it never needs to know where they are. The Gateway looks at the
path of each request (`/patch/...`, `/auth/...`, `/api/player/session/...`) and forwards
it to the right service. This keeps the SDK's configuration to a single base URL.

**Game rules live on the server as data (Level 1).** Things like party size, prices or
loadout limits are values in Otomo's **Config** system, edited by designers in the admin
website. The server enforces them. The client may read client-visible values (they arrive
with Patch, step 1) to show the UI correctly, but the server always has the final say. No
server-side scripting exists in M1.

---

## 2. HTTP in ten minutes

Skip this if you already know HTTP. Everything the SDK does is HTTP.

### 2.1 Request and response

The client sends a **request**; the server sends back exactly one **response**. That is
the whole model: no persistent connection is assumed between calls.

A **request** has:

| Part | Example | What it is |
|---|---|---|
| **Method** | `GET`, `POST`, `PATCH`, `DELETE` | The kind of action. `GET` reads and never changes anything; `POST` creates or does something; `PATCH` changes part of something; `DELETE` removes. |
| **URL** | `https://play.example.com/api/player/session/party` | Where to send it. The **base URL** (`https://play.example.com`) is the Gateway; the **path** (`/api/player/session/party`) says what you want. |
| **Query** | `?after=41` | Extra parameters at the end of the URL, `name=value` pairs joined with `&`. |
| **Headers** | `Authorization: Bearer eyJ…` | Key/value metadata about the request. |
| **Body** | `{"status":"online"}` | Optional data. Otomo always uses **JSON** bodies (except file downloads). |

A **response** has:

| Part | Example | What it is |
|---|---|---|
| **Status code** | `200` | A number saying how it went (table below). **Check this first, always.** |
| **Headers** | `ETag: "4f2a…"` | Metadata about the response. |
| **Body** | `{"display_name":"Tanuki", …}` | The data, usually JSON. Some responses have no body. |

### 2.2 Status codes the SDK will see

The first digit is the family: **2xx** = worked, **3xx** = "nothing new / look
elsewhere", **4xx** = the *client* did something wrong (retrying the same request will
fail again), **5xx** = the *server* had a problem (retrying later may work).

| Code | Name | When you'll see it | What the SDK does |
|---|---|---|---|
| 200 | OK | Success, with a body | Read the body |
| 201 | Created | Success, something was created | Read the body |
| 204 | No Content | Success, deliberately empty body | Nothing to read |
| 206 | Partial Content | Answer to a *resumed* download (§5.6) | Append the bytes to the partial file |
| 304 | Not Modified | "You already have the latest" (§5.3) | Use your cached copy |
| 400 | Bad Request | The request was malformed or failed validation | Bug or bad input: show/log the message, don't retry |
| 401 | Unauthorized | Missing, expired or invalid token | Refresh the token **once**, retry **once** (§4.4) |
| 403 | Forbidden | Token is fine, but you may not do this | Show the refusal. **Never** refresh for a 403: a new token won't change the answer |
| 404 | Not Found | No such thing / no such path | Treat as "doesn't exist" |
| 409 | Conflict | The action clashes with current state (e.g. already in a party) | Re-read the state, show a message |
| 413 | Payload Too Large | Body over the size limit (1 MiB) | Bug: don't send huge bodies |
| 416 | Range Not Satisfiable | Resume position is past the end of the file | Delete the partial file and download from zero |
| 429 | Too Many Requests | You sent requests too fast | Wait and retry later (back off, §4.5) |
| 500, 502, 503 | Server/Gateway errors | Something is down or broken | Retry later with back-off; show "service unavailable" |
| 501 | Not Implemented | The route exists but has no code yet | No M1 route answers it any more |

### 2.3 Every error has the same JSON body

When the status is 4xx or 5xx, every Otomo service answers with exactly this shape:

```json
{"error":{"code":"expired","message":"the access token has expired","request_id":"0192f3a4-7c1e-7b3f-9a2d-4e5f6a7b8c9d"}}
```

- `code` is a fixed machine-readable word. **Branch on this in code** (e.g. `"expired"`).
- `message` is a human sentence. Fine for logs and debug UI; don't branch on it, and
  don't show it raw to players.
- `request_id` identifies this exact request in the server logs. **Always log it** with
  the error; it is how a backend developer finds what happened. The same value is also
  in the `X-Request-Id` response header. (The TLS edge currently repeats that header, so a
  response can carry it twice with the same value: read the first.)

### 2.4 Tokens ("Bearer")

After login (step 2), the client holds an **access token**, an opaque-looking string.
Every Session request carries it in a header:

```
Authorization: Bearer <access token>
```

The token expires after **15 minutes**. You also get a **refresh token**, a second secret
that lasts **30 days** and can be exchanged for a new access token without logging in
again (§6.3). You never need to decode either token; treat both as opaque strings.

### 2.5 Two caching headers used by Patch

- **ETag / If-None-Match.** The server labels a response with a version tag
  (`ETag: "4f2a…"`). Next time, the client sends that tag back
  (`If-None-Match: "4f2a…"`). If nothing changed, the server answers **304** with an
  empty body instead of re-sending everything. This makes "am I up to date?" very cheap.
- **Range.** To resume an interrupted download, the client asks for "the bytes from
  position N onward" (`Range: bytes=N-`). The server answers **206** with only those
  bytes.

---

## 3. Project setup (Godot .NET)

### 3.1 Platforms

Godot's C# support does not cover every export target (web export is the notable gap;
mobile support depends on the Godot version). Check the Godot documentation for your
exact version before committing to a platform. Everything below uses only standard .NET
(`System.Net.Http`, `System.Text.Json`, `System.Security.Cryptography`, `System.IO`),
which works on the desktop targets.

### 3.2 Why .NET's `HttpClient`, not Godot's `HTTPRequest` node

Use `System.Net.Http.HttpClient` for all networking. Compared with Godot's `HTTPRequest`
node it gives you `async`/`await`, cancellation, streaming downloads straight to disk,
and full control of headers (`If-None-Match`, `Range`). `HTTPRequest` would work too, but
the code in this guide assumes `HttpClient`.

Rules:

1. **Create one `HttpClient` for the whole game** (in the SDK's autoload) and reuse it.
   Creating one per request leaks connections.
2. **Never block on a task on the main thread.** `task.Result` or `task.Wait()` inside
   `_Ready`, `_Process` or a signal handler can freeze the game forever. Always `await`.
3. **Godot objects only on the main thread.** Godot .NET installs a synchronisation
   context, so an `await` that *started* on the main thread (e.g. inside `_Ready` or a
   button handler) continues on the main thread afterwards. Keep it that way:
   - don't add `.ConfigureAwait(false)` in SDK code that later touches nodes, signals or
     `ProjectSettings`;
   - if you move heavy work to a background thread with `Task.Run` (hashing a large file
     is a good candidate), come back to the main thread before touching Godot objects
     (e.g. use `CallDeferred`, or simply `await` the `Task.Run` from main-thread code).
4. **Timeouts per request**, not per client: use a `CancellationTokenSource` with a
   timeout for each call (§4.2), because the long-poll call (§7.3) needs a much longer
   timeout than everything else.

### 3.3 Structure

The SDK in `godot-plugin/addons/otomo` (namespace `OtomoSdk`) is laid out like this.
Everything under `Core/` is plain .NET with no Godot APIs, so it is unit-tested with
`dotnet test` (`godot-plugin/tests/Otomo.Sdk.Tests`) without a Godot install:

```
addons/otomo/
  plugin.cfg, OtomoPlugin.cs   // editor plugin: adds the autoload and the otomo/config/* settings
  Otomo.cs                     // autoload node: reads settings, owns the one HttpClient, signals
  Diagnostics/OtomoSmokeTest.tscn   // "does this build reach Otomo?" (§10.2)
  Core/
    OtomoConfig.cs             // base URL, channel, client version, timeouts (§3.4)
    OtomoClient.cs             // builds the sub-clients; StartAsync = Patch → Auth → /me/init (§9)
    Http/OtomoHttp.cs          // shared request/response/error handling (§4)
    Http/OtomoError.cs, Http/Backoff.cs
    Patch/PatchClient.cs       // step 1 (§5), with RemoteConfig, VersionCompare, IPackMounter
    Auth/AuthClient.cs         // step 2 (§6), with FileSecretStore (device ID, refresh token)
    Session/SessionClient.cs   // step 3 (§7.1 only, so far)
```

Still to add as Session lands: the event long-poll (§7.3), the presence heartbeat (§7.2),
friends/party/lobby calls (§7.4–§7.5) and launch (§8). Each client sits behind an interface
(`IPatchClient`, `IAuthClient`, `ISessionClient`) so game code and tests can use fakes.

With the plugin enabled, game code reaches the SDK as `/root/Otomo` (`Otomo.Instance` in C#).
State changes reach game code as Godot signals on the autoload (`PatchStateChanged`,
`PatchProgress`, `RemoteConfigChanged`, `LoggedIn`, `LoggedOut`, `AuthenticationLost`); the
examples in this guide use C# events for brevity.

### 3.4 Configuration the SDK needs

| Setting | Example | Notes |
|---|---|---|
| Gateway base URL | `https://play.example.com` | Your deployment's `OTOMO_PUBLIC_BASE_URL`, over TLS. Take it from configuration, never hard-code it. For development over an SSH tunnel it's `http://localhost:8080` (§10). No trailing slash. |
| Channel | `live` | Players always use `live`. `dev`/`staging` need a *staff* token and are for internal testing only; out of scope for M1's SDK. |
| Client version | `1.4.0` | The version of the **game executable** (not content). Compared with the server's minimum (§5.3). Use `major.minor.patch`. |

### 3.5 Local files

Everything the SDK stores goes under `user://otomo/` (Godot's per-user data folder).
Convert to an OS path for .NET file APIs with `ProjectSettings.GlobalizePath("user://otomo")`.

```
user://otomo/
  device_id              // §6.1  (a secret: identifies the player's account)
  refresh_token          // §6.3  (a secret)
  patch/manifest.json    // last fully-verified manifest
  patch/etag.txt         // its ETag
  patch/blobs/<sha256>   // downloaded, verified files
  patch/blobs/<sha256>.part  // downloads in progress
```

---

## 4. The shared HTTP layer

Every step uses the same small layer. Build and test this first.

### 4.1 Error type

```csharp
public sealed class OtomoError : Exception
{
    public int Status { get; }          // HTTP status, or 0 for "no response at all"
    public string Code { get; }         // e.g. "expired", "not_found", "network"
    public string RequestId { get; }    // log this

    public OtomoError(int status, string code, string message, string requestId)
        : base(message) { Status = status; Code = code; RequestId = requestId; }

    public bool IsRetryableLater => Status == 0 || Status == 429 || Status >= 500;
}
```

### 4.2 Sending a JSON request

```csharp
using System.Net.Http;
using System.Net.Http.Headers;
using System.Text;
using System.Text.Json;

public sealed class OtomoHttp
{
    readonly HttpClient _http;
    readonly string _baseUrl;
    public Func<string?> GetAccessToken = () => null;   // set by AuthClient
    public Func<Task<bool>> RefreshAccessToken = () => Task.FromResult(false);

    static readonly JsonSerializerOptions Json = new() { PropertyNameCaseInsensitive = true };

    public OtomoHttp(HttpClient http, string baseUrl) { _http = http; _baseUrl = baseUrl; }

    /// Sends a request and returns the parsed JSON body (or default for 204).
    /// Retries once after refreshing the token when the server says 401.
    public async Task<T?> SendAsync<T>(HttpMethod method, string path, object? body = null,
        bool authenticated = true, TimeSpan? timeout = null, CancellationToken ct = default)
    {
        for (var attempt = 0; ; attempt++)
        {
            using var req = new HttpRequestMessage(method, _baseUrl + path);
            req.Headers.Accept.Add(new MediaTypeWithQualityHeaderValue("application/json"));
            if (authenticated && GetAccessToken() is { } token)
                req.Headers.Authorization = new AuthenticationHeaderValue("Bearer", token);
            if (body is not null)
                req.Content = new StringContent(JsonSerializer.Serialize(body), Encoding.UTF8, "application/json");

            using var cts = CancellationTokenSource.CreateLinkedTokenSource(ct);
            cts.CancelAfter(timeout ?? TimeSpan.FromSeconds(15));

            HttpResponseMessage res;
            try { res = await _http.SendAsync(req, cts.Token); }
            catch (Exception e) when (e is HttpRequestException or TaskCanceledException && !ct.IsCancellationRequested)
            {
                throw new OtomoError(0, "network", e.Message, "");
            }

            using (res)
            {
                if (res.IsSuccessStatusCode)
                {
                    if (res.StatusCode == System.Net.HttpStatusCode.NoContent) return default;
                    var text = await res.Content.ReadAsStringAsync(cts.Token);
                    return JsonSerializer.Deserialize<T>(text, Json);
                }

                var error = await ReadError(res);
                // 401: the token is missing/expired/invalid. Refresh once, retry once.
                if ((int)res.StatusCode == 401 && authenticated && attempt == 0 && await RefreshAccessToken())
                    continue;
                throw error;
            }
        }
    }

    static async Task<OtomoError> ReadError(HttpResponseMessage res)
    {
        var requestId = res.Headers.TryGetValues("X-Request-Id", out var ids) ? ids.First() : "";
        try
        {
            var text = await res.Content.ReadAsStringAsync();
            using var doc = JsonDocument.Parse(text);
            var e = doc.RootElement.GetProperty("error");
            return new OtomoError((int)res.StatusCode,
                e.GetProperty("code").GetString() ?? "unknown",
                e.GetProperty("message").GetString() ?? "",
                e.TryGetProperty("request_id", out var rid) ? rid.GetString() ?? requestId : requestId);
        }
        catch
        {
            // Not an Otomo error body (e.g. a proxy error page). Keep the status.
            return new OtomoError((int)res.StatusCode, "unknown", res.ReasonPhrase ?? "", requestId);
        }
    }
}
```

### 4.3 JSON field names

The server uses `snake_case` names (`display_name`, `release_id`). Map them explicitly
with `[JsonPropertyName("display_name")]` on your C# properties rather than relying on
naming policies; it makes the contract visible in your code.

### 4.4 The 401 / 403 rule (important)

- **401** means "your token is not usable". Refresh it once and retry the request once.
  If the retry is also 401, the session is gone: clear tokens and go back to login.
- **403** means "your token is fine, but you may not do this". Do **not** refresh. A new
  token would carry the same rights, and refreshing on 403 creates an infinite loop.

The layer above does exactly this; don't add other refresh triggers.

### 4.5 Back-off for 429, 5xx and network errors

When a call fails with `IsRetryableLater`, wait before trying again, and wait longer each
time: 1 s, 2 s, 4 s, … up to 30 s, plus a little randomness so thousands of clients don't
retry in lockstep. Reset to 1 s after a success. Only background loops (heartbeat,
events, patch re-check) retry automatically; for a button press, show an error and let
the player retry.

---

## 5. Step 1: Patch (LIVE)

### 5.1 What it does for the player

On launch, before the login screen:

1. Ask the server which release is current (the **manifest**).
2. If the game executable is too old, stop and tell the player to update the game.
3. Download any files the release needs that aren't on disk yet.
4. Verify every downloaded file.
5. Load the configuration documents, and mount the content packs **before** loading any
   scene that uses them.
6. Continue to login.

If the server can't be reached, start with the last good release on disk (or, on a fresh
install, with the game's built-in defaults and no packs).

A state machine keeps this understandable and gives the UI something to show:

```
CHECKING ─► DOWNLOADING ─► VERIFYING ─► MOUNTING ─► READY
   │                                                 ▲
   ├─► CLIENT_TOO_OLD   (stop: player must update the game executable)
   └─► OFFLINE_READY    (server unreachable: using last good cache or defaults)
```

**What the SDK gives the UI.** A launcher-style download screen follows the
same flow most games use: a **plan** before anything downloads (how many files, how many
bytes, how much a previous attempt already fetched), an optional **confirmation** (a big
download on a metered connection deserves a question), **progress** (bytes and files, current
file, speed, time left), and a **finish** (result, bytes, duration). The SDK raises these as
the `DownloadPlanned`, `DownloadConfirmationRequested`, `DownloadProgress` and
`PatchFinished` signals. A declined download keeps the last good release, with the result
`Declined`. Usage is in `godot-plugin/README.md` ("A download screen").

### 5.2 The manifest

```
GET /patch/v1/live/manifest
```

No token needed. A real response from the development server:

```
HTTP/1.1 200 OK
Content-Type: application/json
ETag: "476ad8d1e4ef9ea21b8cb8f5b07dd25f893a786825b8a81dbe51acf5005c07f4"
Cache-Control: no-cache
X-Min-Client-Version: 0.0.0
X-Request-Id: 01a0e2f2-5512-77ae-be18-8fbb0e16c7c7

{"channel":"live","config":{},"format":1,"min_client_version":"0.0.0","packs":[],"release_id":3}
```

A manifest with content looks like this (formatted for reading):

```json
{
  "format": 1,
  "channel": "live",
  "release_id": 41,
  "created_at": "2026-09-27T10:29:00Z",
  "min_client_version": "1.4.0",
  "config": {
    "balance.player": { "version": 7, "sha256": "9c1e…", "size": 1834 },
    "ui.motd":        { "version": 3, "sha256": "02ab…", "size": 211 }
  },
  "packs": [
    { "name": "nature_biome", "sha256": "5d4f…", "size": 48213904 }
  ]
}
```

| Field | Meaning |
|---|---|
| `format` | Manifest format version. The SDK supports `1`; for anything else, stay on the cache and log it. |
| `release_id` | Number of this release. Every publish gets a new, higher number, but a rollback points the channel back at an earlier release, so the number a client sees can go **down** (seen live: 4 → 3). Never treat a lower id as "stale"; the ETag (§5.3) is what says whether anything changed. Keep it; later steps will send it to the server (§7.5). |
| `min_client_version` | Oldest game executable allowed to play this release (same as the header). |
| `config` | One entry per **configuration document**, keyed by its name (e.g. `balance.player`). Each is a JSON file you download by its `sha256`. |
| `packs` | Content packs (`.pck`) to download by `sha256` and mount. |
| `sha256` | The file's fingerprint (64 lowercase hex characters) and its download name. If even one byte differs, the fingerprint differs. |
| `size` | File size in bytes, for progress bars and a quick sanity check. |

Manifest classes:

```csharp
public sealed class Manifest
{
    [JsonPropertyName("format")] public int Format { get; set; }
    [JsonPropertyName("channel")] public string Channel { get; set; } = "";
    [JsonPropertyName("release_id")] public long ReleaseId { get; set; }
    [JsonPropertyName("min_client_version")] public string MinClientVersion { get; set; } = "0.0.0";
    [JsonPropertyName("config")] public Dictionary<string, ManifestFile> Config { get; set; } = new();
    [JsonPropertyName("packs")] public List<ManifestPack> Packs { get; set; } = new();
}
public class ManifestFile
{
    [JsonPropertyName("sha256")] public string Sha256 { get; set; } = "";
    [JsonPropertyName("size")] public long Size { get; set; }
    [JsonPropertyName("version")] public int Version { get; set; }   // config only
}
public sealed class ManifestPack : ManifestFile
{
    [JsonPropertyName("name")] public string Name { get; set; } = "";
}
```

### 5.3 Checking for updates cheaply (ETag)

Send the ETag you saved last time. If nothing changed you get **304** and no body: the
whole check costs a few hundred bytes.

```csharp
async Task<(Manifest? manifest, string? etag, bool notModified)> FetchManifest(string? cachedEtag, CancellationToken ct)
{
    using var req = new HttpRequestMessage(HttpMethod.Get, $"{_baseUrl}/patch/v1/live/manifest");
    if (cachedEtag is not null)
        req.Headers.IfNoneMatch.Add(EntityTagHeaderValue.Parse(cachedEtag));  // keep the quotes

    using var res = await _http.SendAsync(req, ct);

    // Check the executable version on EVERY answer, including 304.
    if (res.Headers.TryGetValues("X-Min-Client-Version", out var v) &&
        IsOlder(OtomoConfig.ClientVersion, v.First()))
        throw new ClientTooOldException(v.First());

    if (res.StatusCode == HttpStatusCode.NotModified) return (null, cachedEtag, true);
    if (!res.IsSuccessStatusCode) throw await ReadError(res);   // same helper as §4

    var manifest = JsonSerializer.Deserialize<Manifest>(await res.Content.ReadAsStringAsync(ct));
    return (manifest, res.Headers.ETag?.ToString(), false);
}

// "1.4.0" vs "1.10.2": compare number by number, not as text.
static bool IsOlder(string mine, string minimum)
{
    int[] P(string s) => s.Split('.').Select(x => int.TryParse(x, out var n) ? n : 0)
                          .Concat(new[] { 0, 0, 0 }).Take(3).ToArray();
    var (a, b) = (P(mine), P(minimum));
    for (var i = 0; i < 3; i++) if (a[i] != b[i]) return a[i] < b[i];
    return false;
}
```

**About `CLIENT_TOO_OLD`:** Patch can deliver content and configuration, but it **cannot
replace the game executable itself**. When the executable is older than
`min_client_version`, the SDK must stop and the game must send the player to wherever
executables come from (store page, launcher). *Which* place that is hasn't been decided;
make it a callback the game provides.

### 5.4 Deciding what to download

For every file in `config` and `packs`: if `user://otomo/patch/blobs/<sha256>` exists,
it's already verified (only verified files ever get their final name, §5.6), so skip it.
Otherwise, download it.

### 5.5 Downloading a file

```
GET /patch/v1/blob/<sha256>
```

No token needed. Both config documents and packs come from this same address. The
answer is the raw file (`Content-Type: application/octet-stream`). These files never
change (the name *is* the content fingerprint), so the server marks them cacheable
forever.

- Download at most **2 files at the same time**.
- **Stream to disk**; never load a whole pack into memory (packs can be hundreds of MB).
- Write to `<sha256>.part`, and only rename to `<sha256>` after verifying (§5.6).

### 5.6 Resuming and verifying

If a `.part` file exists from an earlier attempt, ask only for the rest:

```csharp
async Task DownloadBlob(ManifestFile f, IProgress<long>? progress, CancellationToken ct)
{
    var dir = ProjectSettings.GlobalizePath("user://otomo/patch/blobs");
    Directory.CreateDirectory(dir);
    var final = Path.Combine(dir, f.Sha256);
    var part = final + ".part";
    long have = File.Exists(part) ? new FileInfo(part).Length : 0;

    using var req = new HttpRequestMessage(HttpMethod.Get, $"{_baseUrl}/patch/v1/blob/{f.Sha256}");
    if (have > 0) req.Headers.Range = new RangeHeaderValue(have, null);   // "bytes=<have>-"

    // ResponseHeadersRead: start reading as soon as headers arrive, stream the body.
    using var res = await _http.SendAsync(req, HttpCompletionOption.ResponseHeadersRead, ct);

    FileMode mode;
    if (res.StatusCode == HttpStatusCode.PartialContent) mode = FileMode.Append;       // 206: resume
    else if (res.StatusCode == HttpStatusCode.OK) { mode = FileMode.Create; have = 0; } // 200: from zero
    else if ((int)res.StatusCode == 416) { File.Delete(part); await DownloadBlob(f, progress, ct); return; }
    else throw await ReadError(res);

    await using (var src = await res.Content.ReadAsStreamAsync(ct))
    await using (var dst = new FileStream(part, mode, FileAccess.Write, FileShare.None, 1 << 16, useAsync: true))
    {
        var buf = new byte[1 << 16];
        int n;
        while ((n = await src.ReadAsync(buf, ct)) > 0)
        {
            await dst.WriteAsync(buf.AsMemory(0, n), ct);
            progress?.Report(have += n);
        }
    }

    // Verify: the SHA-256 of the whole file must equal the name we asked for.
    var actual = await Task.Run(() => Sha256Hex(part), ct);   // off the main thread
    if (actual != f.Sha256)
    {
        File.Delete(part);
        throw new OtomoError(0, "hash_mismatch", $"{f.Sha256} failed verification", "");
    }
    File.Move(part, final, overwrite: true);
}

static string Sha256Hex(string path)
{
    using var sha = System.Security.Cryptography.SHA256.Create();
    using var fs = File.OpenRead(path);
    return Convert.ToHexString(sha.ComputeHash(fs)).ToLowerInvariant();
}
```

On `hash_mismatch`, retry the download once from zero; if it fails again, give up on
this update (go to `OFFLINE_READY` with the previous release).

### 5.7 Loading configuration documents

Each file in `config` is a JSON document. Load them into a `RemoteConfig` autoload with
**typed getters that always have a built-in default**, so the game still runs if a value
or a whole document is missing:

```csharp
public float GetFloat(string doc, string key, float fallback) { … }
// usage: var hp = RemoteConfig.GetFloat("balance.player", "max_hp", 100f);
```

The shape of each document is decided by the game designers per namespace (it has a
JSON Schema on the server); the SDK should not hard-code it.

These are **client-visible** documents only. Server-only rules (prices the server
charges, limits it enforces) never appear in the manifest.

### 5.8 Mounting packs

```csharp
foreach (var pack in manifest.Packs)
{
    var path = $"user://otomo/patch/blobs/{pack.Sha256}";
    if (!ProjectSettings.LoadResourcePack(path, replaceFiles: false))
        throw new OtomoError(0, "pack_mount_failed", pack.Name, "");
}
```

- Mount **before** loading any scene that uses the pack's resources.
- Decide with the team whether packs may **replace** files shipped with the game
  (`replaceFiles`). `false` is the safer default.
- A pack can contain scripts, i.e. code. Only mount packs whose fingerprint came from a
  manifest fetched from our server over HTTPS, and that verified in §5.6.
- New packs take effect on the next launch or return to the main menu; don't swap
  content under a running level.

### 5.9 Saving, cleaning up and re-checking

- Save `manifest.json` and `etag.txt` **only after** every file in it verified and
  mounted. A half-finished update must never become the "last good" release.
- After a successful update, delete blobs the current manifest no longer lists.
- While in menus, re-check every ~5 minutes (§5.3; it's one 304 when nothing changed).
  Apply changed *config* documents immediately (e.g. the message of the day); keep pack
  changes for the next launch.

---

## 6. Step 2: Auth (LIVE)

Three calls, all `POST` with a JSON body, all through the gateway. Keep the request and
response classes in one file.

### 6.1 The device ID

In M1 a player has no username or password. On first launch the SDK creates a random
**device ID** and keeps it forever: 32 bytes from a cryptographically secure random
source, written as base64url with no padding (43 characters).

```csharp
using System.Security.Cryptography;

static string LoadOrCreateDeviceId()
{
    var path = ProjectSettings.GlobalizePath("user://otomo/device_id");
    if (File.Exists(path)) return File.ReadAllText(path).Trim();
    var id = Convert.ToBase64String(RandomNumberGenerator.GetBytes(32))
        .TrimEnd('=').Replace('+', '-').Replace('/', '_');
    Directory.CreateDirectory(Path.GetDirectoryName(path)!);
    File.WriteAllText(path, id);
    return id;
}
```

- The server accepts only `^[A-Za-z0-9_-]{22,128}$` and answers `400 validation_failed`
  to anything else. (A GUID string also passes, so an install that already saved one
  keeps working; new installs use the 32-byte form.)
- **The same ID always gives the same account**, so the player's profile, friends and
  progress follow it.
- **It is effectively the account password.** Anyone who copies it becomes that player.
  Never log it, never send it anywhere except the login call.
- **If the file is lost, the account is lost** (e.g. the player deletes game data or
  plays on another PC). That is an accepted M1 limitation; platform login replaces it
  later.
- Don't use `OS.GetUniqueId()`: it isn't available on every platform, can't be reset,
  and is not a secret.

### 6.2 Logging in

```
POST /auth/anonymous
Content-Type: application/json

{"device_id":"<the device ID>"}
```

Success is `200` with:

```json
{
  "schema_version": 1,
  "access_token": "eyJhbGciOiJFZERTQSIs…",
  "expires_in": 900,
  "refresh_token": "n3Jq…"
}
```

| Field | Meaning |
|---|---|
| `schema_version` | `1`. If it is higher than the SDK knows, still read the fields you know; unknown keys are never an error. |
| `access_token` | Put this in `Authorization: Bearer …` on every Session call. Keep it **in memory only**. |
| `expires_in` | Seconds until it expires (900 = 15 min). Informational: the SDK refreshes on a 401, not on a timer. |
| `refresh_token` | Save to `user://otomo/refresh_token`. Used to get a new access token (§6.3). A secret: never log it. |

**There is no `services` object** (decision D2, 2026-09-29). Auth does not tell the client
where to go next: every service is at a fixed path under the gateway base URL the SDK is
configured with (§3.4), so Session is always `{gateway base URL}/api/player/session`. Older
servers sent a `services` object; ignore it if it appears, and never send the token to an
address taken from a response.

```csharp
public sealed class LoginResponse
{
    [JsonPropertyName("schema_version")] public int SchemaVersion { get; set; }
    [JsonPropertyName("access_token")]   public string AccessToken { get; set; } = "";
    [JsonPropertyName("expires_in")]     public int ExpiresIn { get; set; }
    [JsonPropertyName("refresh_token")]  public string RefreshToken { get; set; } = "";
}
```

Errors: `400 validation_failed` means the device ID is malformed (a bug: don't retry);
`429` and `5xx` are retried with back-off (§4.5).

### 6.3 Refreshing

```
POST /auth/refresh
Content-Type: application/json

{"refresh_token":"<saved refresh token>"}
```

Success is `200` with **the same body as login, including a new `refresh_token`** (valid
another 30 days). Every refresh token works **once**:

- **Replace the saved refresh token immediately**, before doing anything else.
- **Only one refresh may be in flight at a time.** If two requests hit a 401 together and
  both refresh with the same token, the server treats the second use as theft and
  **cancels the whole login**, including the token the first refresh just returned.
- **Any 401 from `/auth/refresh` (code `invalid_token`) means: log in again with the
  device ID.** It covers an expired, revoked, reused or unknown token alike. Never
  refresh again after it.
- A `400 validation_failed` means the body was not the JSON above: a bug.

```csharp
readonly SemaphoreSlim _refreshGate = new(1, 1);

public async Task<bool> RefreshAccessToken()
{
    var tokenBefore = _accessToken;
    await _refreshGate.WaitAsync();
    try
    {
        // Another caller refreshed while we waited: just use its result.
        if (_accessToken != tokenBefore && _accessToken is not null) return true;

        var saved = TokenStore.LoadRefreshToken();
        if (saved is null) return false;   // §9 then logs in with the device ID
        try
        {
            // authenticated: false, so a 401 here is never itself "refreshed".
            var r = await _http.SendAsync<LoginResponse>(HttpMethod.Post, "/auth/refresh",
                new { refresh_token = saved }, authenticated: false);
            TokenStore.SaveRefreshToken(r!.RefreshToken);   // first!
            _accessToken = r.AccessToken;
            return true;
        }
        catch (OtomoError e) when (e.Status == 401)
        {
            // invalid_token: log in again with the device ID. Never refresh again.
            TokenStore.ClearRefreshToken();
            _accessToken = null;
            return await LoginWithDeviceId();
        }
    }
    finally { _refreshGate.Release(); }
}
```

On startup, if a refresh token is saved, try a refresh first; fall back to device login.

### 6.4 Logging out

```
POST /auth/logout
Content-Type: application/json

{"refresh_token":"<saved refresh token>"}
```

The answer is `204 No Content` whatever the token was. Then delete the saved refresh
token and forget the access token, whatever the answer (even a `500`). Keep the device
ID: logging out is not deleting the account.

### 6.5 The rules, in one place

| Response | SDK does |
|---|---|
| `401` from a Session call | Refresh once (§6.3), retry the call once. A second `401`: clear tokens, log in again. |
| `401` from `/auth/refresh` | Log in again via `/auth/anonymous`. **Never** refresh again. |
| `403` from anything | Show the refusal. **Never** refresh: a new token carries the same rights. |
| `400 validation_failed` | A bug in the request. Log it with `request_id`; don't retry. |
| `429`, `5xx`, network | Back off and retry (§4.5). |

An access token may keep working for up to 30 s after `expires_in` runs out (verifiers
allow for clock skew). That is expected, and another reason to refresh on 401 rather than
on a timer.

---

## 7. Step 3: Session (LIVE)

Every path below is under `/api/player/session` and needs the access token. Every route is
built (SE-1…SE-8, LB-1…LB-4), and the SDK implements all of it.

### 7.1 First call after login (LIVE)

```
POST /api/player/session/me/init
```

Creates the player's profile on their very first login (with a provisional name such as
`Player4417`) and returns it; on later logins it just returns it. It answers `200` either
way, so it is safe to call after every login.

Profile:

```json
{"player_id":"…","display_name":"Tanuki","discriminator":4417}
```

`discriminator` is a JSON number from 1 to 9999; show it with four digits. `display_name` +
`discriminator` is how players find each other ("Tanuki#4417"), because several players
may share a display name.

| Call | Purpose |
|---|---|
| `GET /me` | Read the profile. `404 profile_not_found` if `/me/init` was never called |
| `PATCH /me` with `{"display_name":"NewName"}` | Rename. Answers `200` with the new profile |

Rename rules (the defaults; the server's `session.rules` can change them):

- Surrounding spaces are removed. The name must then be 3 to 16 characters of `A` to `Z`,
  `a` to `z`, `0` to `9` and `_`, and not a reserved word such as `admin`. Otherwise
  **400** `invalid_display_name`, with a message that says which rule failed.
- One rename per 24 hours. Too soon gives **429** `rate_limit_exceeded` with a
  `Retry-After` header in seconds. The provisional name doesn't count, so the first rename
  is always allowed, and sending the current name again changes nothing.
- The player keeps their discriminator unless someone else already has the new name with
  it; then they get a new one. Show the discriminator from the response, not the old one.
- **409** `name_unavailable` means too many players have that name: ask for another.

### 7.2 Presence heartbeat (LIVE)

While the game is running and logged in, every **20 seconds**:

```
POST /api/player/session/presence/heartbeat
Content-Type: application/json

{"status":"online"}
```

`status` is `online`, `in_menus` or `away`; anything else is `400 invalid_status`. The
answer is `204` with no body.

If heartbeats stop for 60 seconds, the server marks the player offline on its own, so
there's nothing to do on quit. Run it as a background loop (e.g. `await Task.Delay(20s)`
in a loop started after login, cancelled on logout). Heartbeats are limited to one per
10 seconds, so don't send extra ones: a heartbeat sent too soon gets `429` with a
`Retry-After` header. When the status changes, online friends get `presence.changed`
with `player_id` and `status` (`offline` when heartbeats stop). `GET /party` shows each
member's `status`.

### 7.3 Receiving events (long-poll) (LIVE)

The server can't push to the client over plain HTTP, so the client keeps a request
**open** and the server answers it when something happens (or after ~25 seconds with
"nothing happened"). Then the client immediately opens the next one. This is called
**long-polling**.

```
GET /api/player/session/events?after=<last seq you processed>
```

Possible answers:

| Answer | Meaning | Do |
|---|---|---|
| `200` with a list of events | Something happened | Handle each, remember the highest `seq`, poll again |
| `200` with `[]` | 25 s passed, nothing happened, or a newer poll of yours replaced this one | Poll again immediately |
| `200` with `{"resync":true}` | You were away too long; events were dropped, or your stream expired and started again | Re-fetch friends and party with normal calls, then poll again with `after=0` |
| `400 invalid_after` | `after` is not a whole number of 0 or more | Fix the cursor; leaving `after` out means 0 |

Event:

```json
[{"seq":413,"at":1789000000000,"type":"party.updated","payload":{"party_id":"…","revision":57}}]
```

Rules for the loop:

- **Use a long timeout for this call only: 35 seconds** (the server holds up to 25 s).
- Run **one** loop per logged-in client. Starting a second one cancels the first on the
  server.
- Remember `seq` in memory. Start with `after=0` after login.
- On errors, use the back-off from §4.5; on 401, the shared layer refreshes and retries.
- **Events are hints, not the truth.** An event says "the party changed"; to show the
  party, fetch it (`GET /party`). If an event is missed, nothing breaks; the UI is just
  late. Use `revision` in party events to ignore stale ones (a lower revision than the
  one you already have).

Event types planned for M1: `friend.request`, `friend.accepted`, `friend.removed`,
`party.invite`, `party.updated`, `party.kicked`, `party.disbanded`,
`presence.changed`, and for the lobby and launch `party.launching`, `party.launch_failed`
and `party.returned` (§8.2). Ignore types you don't know.

### 7.4 Friends, blocks and party (LIVE)

| Call | Purpose |
|---|---|
| `GET /friends` | Friends with online status, plus incoming and outgoing requests |
| `POST /friends/requests` `{"display_name":"Tanuki","discriminator":4417}` | Send a friend request |
| `POST /friends/requests/{player_id}/accept` · `/decline` | Answer a request |
| `DELETE /friends/{player_id}` | Remove a friend |
| `POST /blocks/{player_id}` · `DELETE /blocks/{player_id}` | Block / unblock (blocking also removes friendship and invites) |
| `POST /party` | Create a party (**409** `already_in_party` if already in one) |
| `GET /party` | Current party: members and `revision` (**404** `not_in_party`) |
| `POST /party/invites` `{"player_id":"…"}` | Invite someone. Any member may invite |
| `POST /party/invites/{invite_id}/accept` · `/decline` | Answer an invite |
| `POST /party/leave` | Leave (if you were leader, the longest-standing member becomes leader) |
| `POST /party/kick/{player_id}` · `/promote/{player_id}` with `{"revision":57}` | Leader only (**403** `not_leader` otherwise) |

The party routes are **LIVE** once SE-6 is deployed, and the friends and blocks
routes once SE-5 is.

**Friends.** `GET /friends` returns `{"friends":[…],"incoming":[…],"outgoing":[…]}`; each entry
is `{"player_id","display_name","discriminator","since"}` with `discriminator` a number, and
entries in `friends` also carry the friend's presence `status`. `POST /friends/requests`
answers `{"player_id","display_name","discriminator","state"}`: `state` is `pending`, or
`accepted` if they had already asked you. Other answers: **404** `player_not_found` (no one
with that name and number), **403** `blocked`, **409** `already_friends` / `friend_limit`,
**429** `rate_limit_exceeded` with `Retry-After` (at most 20 requests an hour). Accept answers
the new friend; decline, remove, block and unblock answer **204**. Blocking also ends the
friendship and any party invites between you, and unblocking doesn't bring the friendship
back. Events: `friend.request` and `friend.accepted` carry `player_id`, `display_name` and
`discriminator`; `friend.removed` carries `player_id`.

**Leader calls carry the revision.** Kick and promote (and later the lobby's settings and
launch, §7.5) send the `revision` of the party the leader is looking at. If the party has
changed since, the answer is **409** `revision_mismatch`: fetch `GET /party` and let the
leader try again. A missing `revision` is **400** `revision_required`.

`GET /party` returns `{"party_id","leader_id","revision","max_size","members":[{"player_id",
"display_name","discriminator","joined_at"}]}`, with the longest-standing member first.
`max_size` is the limit in force now; show it rather than hard-coding 4. Other answers to
expect: `409 party_full`, `410 invite_expired` (invites last 5 minutes), `404
invite_not_found`, `403 blocked`.

Events: the invitee gets `party.invite` (`invite_id`, `party_id`, `from_player`,
`expires_at`); members get `party.updated` (`party_id`, `revision`) on every change; a kicked
player gets `party.kicked`.

### 7.5 Lobby and content version (LIVE)

**The party is the lobby.** On top of the party calls in §7.4, the party has a **state**,
leader-owned **settings** and a per-member **ready** flag (`14-launch-handoff.md` §2). The
state, settings, ready and launch calls are **LIVE**.

| State | Meaning | What the UI allows |
|---|---|---|
| `forming` | Normal lobby | Everything: invite, kick, settings, ready, launch |
| `launching` | The leader pressed launch; the server is being found | Only **leave**. Other changes get `409 party_locked` |
| `in_game` | The match is running | Only **leave**, and fetching a fresh ticket (§8.3) |

`GET /party` then also returns `state`, `settings`, `members[].ready` and, when `in_game`,
`match: {allocation_id, address, port}` (no ticket).

| Call | Who | Notes |
|---|---|---|
| `PATCH /party/settings` `{"settings":{…},"revision":57}` | leader | Clears everyone's ready. Keys you leave out keep their values. `400 invalid_settings` (a key that isn't a lobby setting, or a value it doesn't allow), `403 not_leader`, `409 revision_mismatch` / `party_locked` |
| `POST /party/ready` `{"ready":true}` | any member | `409 party_locked` outside `forming` |
| `POST /party/launch` `{"revision":57}` | leader | `202` and the state becomes `launching`. `409 not_ready` unless everyone is ready. Pressing twice is safe |

`settings` is a flat object of strings, for example
`{"expedition":"expedition_1","difficulty":"hard"}`. The keys and their allowed values come
from the server's `session.rules`; a new lobby starts with every default. `party.updated`
events carry `state`, `settings` and a `ready` map (`{"<player_id>": true}`) besides
`party_id` and `revision`, so a lobby screen can update without a fetch.

Send the `revision` you last saw. A `409 revision_mismatch` means someone changed the
lobby first: re-fetch `GET /party` and try again.

**Content version check (server side LIVE; the SDK doesn't send it yet; D5 in
doc 13).** Send the `release_id` the client loaded (§5.2) on every Session request as
`X-Otomo-Release: <release_id>`. Session answers `409` with code `release_outdated` when it
isn't the live release. That includes a *higher* id after a rollback (§5.2), so the server
compares for equality. A value that isn't a positive integer is `400 invalid_release`. On
`release_outdated`, re-run Patch (§5), then retry the request once. The SDK sends the header
through `OtomoHttp.ExtraHeaders`. Session lets requests without it through unless
`SESSION_REQUIRE_RELEASE_HEADER=true`, which a deployment should set once every client in
use sends it.

---

## 8. Step 4: Launch and game server (LIVE)

The contract is `14-launch-handoff.md`. All of this is live:
- the Allocator;
- Session's launch routes (`POST /party/launch`, `POST /party/launch/ticket`);
- the Gameplay Proxy;
- a pool of two game servers.

The proxy listens on UDP 27000 by default, but a deployment can change it
(`GAMEPLAY_PROXY_PORT`). The ticket answer always names the right port, so never hard-code
it.

The game server's own ticket check (§8.4 step 4) is the game's code;
`docs/guide/game-servers.md` describes it.

### 8.1 The flow

1. The leader calls `POST /party/launch`. Everyone gets `party.updated` with state `launching`.
2. Session asks the Allocator for a game server (a few seconds at most).
3. Each member gets **their own** `party.launching` event carrying the proxy's address,
   the UDP port, and a **join ticket** valid for **60 seconds**.
4. The client connects to the **Gameplay Proxy** at that address (§8.4), which forwards it
   to the game server.
5. When the match ends (or the server dies), everyone gets `party.returned`, and the lobby
   is back in `forming` with every ready flag cleared.

If no server is free, everyone gets `party.launch_failed` and the lobby is `forming` again.
Ready flags are kept, so the leader can simply retry.

### 8.2 The events

| Type | Payload (plus `party_id`, `revision`) | What to do |
|---|---|---|
| `party.launching` | `allocation_id`, `address`, `port`, `ticket`, `ticket_expires_at` | Connect **now** (§8.4). If the ticket already expired, get a new one (§8.3) |
| `party.launch_failed` | `reason`: `no_capacity`, `allocator_unavailable` | Show it; stay in the lobby |
| `party.returned` | `reason`: `ended`, `expired`, `server_dead`, `server_restarted` | Leave the match scene, back to the lobby |

`expired` means nobody connected within 60 s of the launch.

### 8.3 Getting a fresh ticket

`POST /party/launch/ticket` (no body) returns
`{"address", "port", "ticket", "ticket_expires_at"}` while the party is `in_game`. Use it:
- when the `party.launching` event arrived late (after a resync, or the app was in the
  background);
- to rejoin after a crash or a dropped connection;
- whenever the ticket you hold has expired.

Tickets are single-use. **Never store one**; ask for a new one each time.

### 8.4 Connecting: the proxy handshake

The ticket can't ride in ENet's connect packet, so the client first sends it in a small UDP
**handshake datagram**, from the **same local port** ENet will then use:

1. Pick a free local port *P*. Bind a `PacketPeerUDP` to it, and send to `address:port`:
   the 4 ASCII bytes `OTJ1`, then the ticket's length as a 2-byte big-endian number, then
   the ticket's UTF-8 bytes.
2. Wait for the answer. `OTOK` means accepted. `OTNO` followed by one byte means refused:
   `1` invalid, `2` expired, `3` reused, `4` unknown server. With no answer, resend up to
   5 times, 500 ms apart.
3. On `OTOK`, close the `PacketPeerUDP` and connect with
   `ENetMultiplayerPeer.CreateClient(address, port, …, localPort: P)`.
4. The game server checks the ticket again. Send the **same ticket** with
   `SceneMultiplayer.SendAuth` when it asks, and wait for `CompleteAuth`. Turn the client's
   auth step on only when there is a ticket: a server without the check never completes
   auth, and the client would time out. A game server that doesn't check accepts any
   connection the proxy forwards.

On `OTNO 2` or `OTNO 3`, get a fresh ticket (§8.3) and start again from step 1. Keep this
UDP code separate from the SDK's HTTP layer.

## 9. Putting it together: the launch sequence

```csharp
public override async void _Ready()
{
    // 1. Patch — before any content scene loads.
    var patch = await Patch.RunAsync();          // READY, OFFLINE_READY or CLIENT_TOO_OLD
    if (patch == PatchResult.ClientTooOld) { ShowUpdateGameScreen(); return; }

    // 2. Auth — reuse a saved login when possible.
    if (!await Auth.RefreshAccessToken())
        await Auth.LoginWithDeviceId();

    // 3. Session.
    var me = await Session.InitAsync();          // POST /me/init
    Session.StartHeartbeat();                    // every 20 s
    Session.StartEventLoop();                    // long-poll

    // 4. Launch: react to the lobby events (§8).
    Session.Events.On("party.launching", e => Match.ConnectAsync(e.Address, e.Port, e.Ticket));
    Session.Events.On("party.launch_failed", e => Lobby.ShowLaunchFailed(e.Reason));
    Session.Events.On("party.returned", e => Match.LeaveAndShowLobby(e.Reason));

    // …show the main menu / lobby UI.
}
```

(`async void` is acceptable for Godot callbacks like `_Ready`; wrap the body in
`try/catch` so an exception is shown to the player instead of disappearing.)

If Patch ends in `OFFLINE_READY`, the server is probably unreachable: show an "offline"
state and retry login with back-off rather than failing hard.

---

## 10. Developing and testing

### 10.1 Reaching the server

The team server is public over HTTPS. Use base URL **`https://play.example.com`**: nothing
to set up, and it works from any network. It has a real (Let's Encrypt) certificate, so
don't disable certificate checks.

To reach the gateway directly (for example to compare with what the edge returns), an SSH
tunnel still works (ask for access to `you@play.example.com`):

```sh
ssh -N -L 8080:127.0.0.1:8080 you@play.example.com
```

Then use base URL `http://localhost:8080`.

### 10.2 Trying calls by hand

`curl` (built into Windows, macOS and Linux terminals) shows exactly what the server says:

```sh
curl -i https://play.example.com/patch/v1/live/manifest
curl -i -H 'If-None-Match: "<etag from the previous answer>"' https://play.example.com/patch/v1/live/manifest   # expect 304
curl -i -X POST -H 'Content-Type: application/json' \
     -d '{"device_id":"test-device-0123456789abcdef"}' https://play.example.com/auth/anonymous   # expect 200
curl -i -X POST https://play.example.com/api/player/session/me/init -H "Authorization: Bearer <access_token>"   # expect 200 and your profile
```

`-i` prints the status line and headers, which is usually where the answer to "why
didn't that work?" is.

From inside the game, run `res://addons/otomo/Diagnostics/OtomoSmokeTest.tscn` (F6 in the
editor). It runs Patch → Auth → `/me/init` and prints each step. Headless, it exits 0 when
patched and logged in:

```sh
godot --headless --path <game> res://addons/otomo/Diagnostics/OtomoSmokeTest.tscn -- --otomo-smoke-quit
```

`-- --otomo-base-url=http://localhost:8080` points one run at the SSH tunnel (§10.1) without
changing the project setting.

### 10.3 What works today

| Step | Server today | SDK today | How to develop the rest |
|---|---|---|---|
| Patch | **Works** (`live` channel). The live release is empty until someone publishes one in the admin website. | **Done** (`PatchClient`) | To see real content, publish a release with a client-audience config document and a small `.pck`, then roll back: the steps are in `docs/guide/first-patch.md`. |
| Auth | **Works** (`/auth/anonymous`, `/auth/refresh`, `/auth/logout`) | **Done** (`AuthClient`) | — |
| Session | **Works**: every route in §7 | **Done** (`SessionClient`, `PresenceClient`, `EventsClient`, `FriendsClient`, `PartyClient`) | A fake `ISessionClient` with in-memory friends/party and a fake event source for unit tests; the live server for end-to-end runs. |
| Launch | **Works** end to end through the proxy to a game server | **Done** (`LaunchClient`, `JoinHandshakeClient`) | A local fake proxy that answers the handshake with `OTOK`/`OTNO` for unit tests. To run a server build yourself, see `docs/guide/game-servers.md`. |

Keep each client behind its interface (`IPatchClient`, `IAuthClient`, `ISessionClient`); that
makes the fakes trivial and keeps tests independent of the server. The Core tests use a fake
`HttpMessageHandler` (`tests/Otomo.Sdk.Tests/FakeHandler.cs`); `LiveServerTests` runs the real
sequence when `OTOMO_LIVE_BASE_URL` is set.

### 10.4 Test checklist for Patch

The SDK's unit tests cover each of these with a fake server (`PatchTests.cs`). Fresh
install, second launch and a dropped pack were also checked against a live server.

- Fresh install, server has content → everything downloads, verifies and mounts.
- Second launch, nothing changed → a single 304, no downloads.
- Kill the game mid-download, relaunch → download resumes (206), doesn't restart.
- Corrupt one byte of a `.part` file → detected, re-downloaded. (A file that already has its
  final `<sha256>` name is trusted and not re-hashed at start-up, per §5.4.)
- Server unreachable with a cache → `OFFLINE_READY` with the last good release.
- Server unreachable, fresh install → built-in defaults, no packs.
- `min_client_version` above the game's version → `CLIENT_TOO_OLD`, nothing mounted.
- A release that drops a pack → the old blob is cleaned up.

---

## 11. Pitfalls checklist

- [ ] One `HttpClient` for the whole game.
- [ ] No `.Result` / `.Wait()` on the main thread.
- [ ] Godot objects touched only on the main thread.
- [ ] 401 → refresh once, retry once. 403 → never refresh.
- [ ] Only one token refresh in flight; save the new refresh token first.
- [ ] Device ID and refresh token never logged.
- [ ] Every error logged with its `request_id`.
- [ ] Long-poll uses a 35 s timeout; everything else ~15 s.
- [ ] Downloads streamed to `.part`, verified by SHA-256, then renamed.
- [ ] Manifest saved as "last good" only after everything verified and mounted.
- [ ] Packs mounted before scenes that use them.
- [ ] Unknown JSON fields and unknown event types ignored, not treated as errors.
- [ ] Gateway base URL is configuration, not a constant.
- [ ] No cookies: the player API is bearer-token only, and the server drops cookies.
- [ ] Join tickets are used at once and never stored; expired or refused means get a fresh one (§8.3).
- [ ] The UDP handshake and ENet use the same local port.
