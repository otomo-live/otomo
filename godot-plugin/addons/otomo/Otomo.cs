using System;
using System.Net.Http;
using System.Threading;
using System.Threading.Tasks;
using Godot;
using OtomoSdk.Http;
using OtomoSdk.Launch;
using OtomoSdk.Patch;
using OtomoSdk.Session;

namespace OtomoSdk;

/// The SDK autoload, reachable as /root/Otomo (guide §3.3). Reads its configuration from
/// Project Settings (otomo/config/*), owns the game's single HttpClient and exposes the
/// sub-clients of OtomoClient. Everything that isn't Godot glue lives in Core/.
///
/// Typical use from the first scene:
///   var result = await Otomo.Instance.StartAsync();
///   if (result.Patch == PatchResult.ClientTooOld) { … send the player to update … }
public partial class Otomo : Node
{
    public const string SettingBaseUrl = "otomo/config/base_url";
    public const string SettingClientVersion = "otomo/config/client_version";
    public const string SettingChannel = "otomo/config/channel";
    public const string SettingVerboseLog = "otomo/config/verbose_log";
    /// Ask the player before downloading more than this many bytes: -1 never asks (the
    /// default), 0 always asks. See DownloadConfirmationRequested.
    public const string SettingConfirmDownloadOverBytes = "otomo/config/confirm_download_over_bytes";

    /// One HttpClient for the whole game; per-request timeouts are set by the SDK.
    static readonly System.Net.Http.HttpClient SharedHttp = new() { Timeout = Timeout.InfiniteTimeSpan };

    public static Otomo Instance { get; private set; }

    public OtomoClient Client { get; private set; }

    [Signal] public delegate void PatchStateChangedEventHandler(string state);
    [Signal] public delegate void PatchProgressEventHandler(long done, long total);
    [Signal] public delegate void RemoteConfigChangedEventHandler();
    [Signal] public delegate void LoggedInEventHandler();
    [Signal] public delegate void LoggedOutEventHandler();
    /// A request still got 401 after refreshing: the game should return to its login state.
    [Signal] public delegate void AuthenticationLostEventHandler(string code, string requestId);

    /// One event from the long-poll (guide §7.3). payloadJson is the raw JSON text of
    /// the event's payload, or "" when it carries none; parse it yourself if the type
    /// needs one. at is Unix milliseconds.
    [Signal] public delegate void SessionEventReceivedEventHandler(long seq, long at, string type, string payloadJson);

    /// The event stream's cursor was too old or the stream restarted (guide §7.3): the
    /// game should re-fetch friends and party with normal calls, since some events in
    /// between were never delivered.
    [Signal] public delegate void ResyncEventHandler();

    /// This player's own game server (guide §8.2, doc 14 §7): connect now with
    /// JoinHandshakeAsync. ticketExpiresAtUnixMs is Unix milliseconds; if it has already
    /// passed (the event arrived late), ask for a fresh ticket instead
    /// (Client.Party.GetLaunchTicketAsync) rather than using this one.
    [Signal] public delegate void PartyLaunchingEventHandler(string allocationId, string address, int port, string ticket, long ticketExpiresAtUnixMs);

    /// No game server was available. reason is "no_capacity" or "allocator_unavailable";
    /// the lobby is forming again with ready flags kept.
    [Signal] public delegate void PartyLaunchFailedEventHandler(string reason);

    /// The match ended (or this client should leave it): reason is "ended", "expired",
    /// "server_dead" or "server_restarted". The lobby is forming again with every ready
    /// flag cleared.
    [Signal] public delegate void PartyReturnedEventHandler(string reason);

    // --- Download UI: plan → optional confirmation → progress → finished ---

    /// What this patch will download, before anything is downloaded. fileCount 0 means
    /// the game is up to date. Raised once per patch run (not when offline or too old).
    [Signal] public delegate void DownloadPlannedEventHandler(long releaseId, int fileCount, long bytesToDownload, long bytesAlreadyHave);

    /// Download progress, at most every 100 ms plus on every finished file.
    /// etaSeconds is -1 until the speed is known. Format with OtomoFormat.Bytes/Duration.
    [Signal] public delegate void DownloadProgressEventHandler(long bytesDone, long bytesTotal, int filesDone, int filesTotal, string currentFile, double bytesPerSecond, double etaSeconds);

    /// Raised when confirm_download_over_bytes asks for it: show a dialog, then call
    /// RespondToDownload(true/false). Declining keeps the cached release (result Declined).
    [Signal] public delegate void DownloadConfirmationRequestedEventHandler(long bytesToDownload, int fileCount);

    /// The patch run is over: result is a PatchResult name (Ready, OfflineReady, ClientTooOld,
    /// Declined), errorCode is "" on success.
    [Signal] public delegate void PatchFinishedEventHandler(string result, long bytesDownloaded, int filesDownloaded, double seconds, string errorCode);

    /// The latest progress snapshot, for UIs that poll in _Process instead of using signals.
    public PatchProgress CurrentDownload => Client.Patch.CurrentProgress;

    /// The last plan announced, or null before the first patch run.
    public PatchPlan LastPlan { get; private set; }

    TaskCompletionSource<bool> _pendingConfirmation;

    /// Non-null while the in-menu Patch re-check is running.
    Godot.Timer _inMenuPatchTimer;

    public override void _EnterTree()
    {
        Instance = this;
        var config = new OtomoConfig(ResolveBaseUrl(), ProjectSettings.GlobalizePath(ResolveDataDir()))
        {
            Channel = (string)ProjectSettings.GetSetting(SettingChannel, "live"),
            ClientVersion = ResolveClientVersion(),
            Log = (bool)ProjectSettings.GetSetting(SettingVerboseLog, true) ? GD.Print : null,
        };
        Client = new OtomoClient(SharedHttp, config, new GodotPackMounter());

        // Most of these fire from continuations of awaits started on the main thread, which
        // Godot's synchronisation context resumes there (guide §3.2 rule 3). The events and
        // presence loops run on the thread pool, though, and anything they trigger (events,
        // launch, a 401 or release_outdated re-patch) fires there too -- so every signal goes
        // through Emit, which defers to the main thread when needed.
        Client.Patch.StateChanged += s => Emit(SignalName.PatchStateChanged, s.ToString());
        Client.Patch.Progress += (done, total) => Emit(SignalName.PatchProgress, done, total);
        Client.RemoteConfig.Changed += () => Emit(SignalName.RemoteConfigChanged);
        Client.Auth.LoggedIn += () => Emit(SignalName.LoggedIn);
        Client.Auth.LoggedOut += () => Emit(SignalName.LoggedOut);
        Client.Http.AuthenticationLost += e => Emit(SignalName.AuthenticationLost, e.Code, e.RequestId);
        Client.Events.EventReceived += e => Emit(SignalName.SessionEventReceived, e.Seq, e.At, e.Type, e.PayloadRawJson);
        Client.Events.Resync += () => Emit(SignalName.Resync);
        Client.Launch.Launching += info => Emit(SignalName.PartyLaunching,
            info.AllocationId, info.Address, info.Port, info.Ticket, info.TicketExpiresAt.ToUnixTimeMilliseconds());
        Client.Launch.LaunchFailed += info => Emit(SignalName.PartyLaunchFailed, info.Reason);
        Client.Launch.Returned += info => Emit(SignalName.PartyReturned, info.Reason);

        Client.Patch.Planned += plan =>
        {
            LastPlan = plan;
            Emit(SignalName.DownloadPlanned, plan.ReleaseId, plan.FileCount, plan.BytesToDownload, plan.AlreadyHaveBytes);
        };
        Client.Patch.ProgressChanged += p => Emit(SignalName.DownloadProgress,
            p.BytesDone, p.BytesTotal, p.FilesDone, p.FilesTotal, p.CurrentFile, p.BytesPerSecond, p.EtaSeconds ?? -1.0);
        Client.Patch.Finished += s => Emit(SignalName.PatchFinished,
            s.Result.ToString(), s.BytesDownloaded, s.FilesDownloaded, s.Duration.TotalSeconds, s.Error?.Code ?? "");

        // The no-code confirmation: when the project asks for it, a big download waits for
        // the game's answer through RespondToDownload. A C# game can instead set
        // Client.Patch.ConfirmDownload itself; that replaces this hook.
        var confirmOver = (long)ProjectSettings.GetSetting(SettingConfirmDownloadOverBytes, -1L);
        if (confirmOver >= 0)
            Client.Patch.ConfirmDownload = (plan, ct) => AskToDownload(plan, confirmOver, ct);

        GD.Print($"otomo: gateway {config.BaseUrl}, channel {config.Channel}, client {config.ClientVersion}, data {config.DataDir}");
    }

    public override void _ExitTree()
    {
        if (Instance == this) Instance = null;
    }

    /// Patch → Auth → Session /me/init. Call from the main thread and await it.
    public Task<StartResult> StartAsync(CancellationToken ct = default) => Client.StartAsync(ct);

    /// Tells Session the player is in a menu, away, or (the default while logged in) online
    /// (guide §7.2). The heartbeat loop itself starts/stops automatically with login/logout;
    /// this only changes what it reports.
    public Task SetPresenceStatusAsync(PresenceStatus status, CancellationToken ct = default) =>
        Client.Presence.SetStatusAsync(status, ct);

    /// Runs the proxy handshake for a PartyLaunching signal (address/port/ticket straight
    /// off it) and returns the local port ENetMultiplayerPeer.CreateClient must also bind
    /// to (guide §8.4). Retries once on an expired/reused ticket by fetching a fresh one;
    /// any other outcome (including a second refusal) is returned as-is. This does not
    /// create the ENetMultiplayerPeer or wire SceneMultiplayer auth -- that stays in the
    /// game layer, same as it already is for connecting to the game server directly.
    public Task<JoinHandshakeResult> JoinMatchAsync(string address, int port, string ticket, CancellationToken ct = default) =>
        Client.Launch.JoinAsync(new LaunchingInfo { Address = address, Port = port, Ticket = ticket }, ct);

    /// Re-runs Patch every 5 minutes while the player is in menus, so a config
    /// change applies live and a new pack is ready to mount next launch -- without ever
    /// landing mid-match (call StopInMenuPatchRecheck before loading into one). Calling this
    /// again while already running is a no-op.
    public void StartInMenuPatchRecheck()
    {
        if (_inMenuPatchTimer is not null)
            return;

        _inMenuPatchTimer = new Godot.Timer
        {
            WaitTime = 300.0,
            OneShot = false,
            Autostart = true,
        };
        _inMenuPatchTimer.Timeout += OnInMenuPatchRecheckTimeout;
        AddChild(_inMenuPatchTimer);
    }

    /// Stops the in-menu re-check started by StartInMenuPatchRecheck. Safe to call even if
    /// it was never started, or twice in a row.
    public void StopInMenuPatchRecheck()
    {
        if (_inMenuPatchTimer is null)
            return;

        _inMenuPatchTimer.Timeout -= OnInMenuPatchRecheckTimeout;
        _inMenuPatchTimer.QueueFree();
        _inMenuPatchTimer = null;
    }

    async void OnInMenuPatchRecheckTimeout()
    {
        try
        {
            await Client.Patch.RunAsync();
        }
        catch (Exception e)
        {
            // RunAsync reports failure through PatchSummary/Finished, not by throwing; this
            // is a last-resort net so a timer callback can never take the game down.
            GD.PushWarning($"otomo: in-menu patch re-check failed: {e.Message}");
        }
    }

    public RemoteConfig RemoteConfig => Client.RemoteConfig;

    /// The player's answer to DownloadConfirmationRequested. Ignored when nothing is waiting.
    public void RespondToDownload(bool accept)
    {
        var pending = _pendingConfirmation;
        _pendingConfirmation = null;
        pending?.TrySetResult(accept);
    }

    Task<bool> AskToDownload(PatchPlan plan, long confirmOver, CancellationToken ct)
    {
        if (plan.BytesToDownload <= confirmOver)
            return Task.FromResult(true);
        // RunContinuationsAsynchronously: the patch resumes on its own continuation, not
        // inside the button handler that called RespondToDownload.
        var tcs = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
        ct.Register(() => tcs.TrySetCanceled(ct));
        _pendingConfirmation = tcs;
        Emit(SignalName.DownloadConfirmationRequested, plan.BytesToDownload, plan.FileCount);
        return tcs.Task;
    }

    /// EmitSignal on the main thread: directly when already there, otherwise deferred to the
    /// next idle frame. Deferred calls run in the order they were queued, so events keep
    /// their order.
    void Emit(StringName signal, params Variant[] args)
    {
        if (OS.GetThreadCallerId() == OS.GetMainThreadId())
        {
            EmitSignal(signal, args);
            return;
        }

        var deferredArgs = new Variant[args.Length + 1];
        deferredArgs[0] = signal;
        args.CopyTo(deferredArgs, 1);
        CallDeferred(GodotObject.MethodName.EmitSignal, deferredArgs);
    }

    /// `--otomo-data-dir=<dir>` after `--` on the command line keeps this run's device id,
    /// tokens and downloads apart from the default `user://otomo` -- so a second copy of the
    /// game on the same PC signs in as a different player (testing friends, invites,
    /// parties). The folder may be a `user://` path or an absolute one. A testing aid: a
    /// release export ignores it, so players always use the default folder.
    static string ResolveDataDir()
    {
        if (!OS.IsDebugBuild())
            return "user://otomo";
        foreach (var arg in OS.GetCmdlineUserArgs())
            if (arg.StartsWith("--otomo-data-dir=", StringComparison.Ordinal))
                return arg["--otomo-data-dir=".Length..];
        return "user://otomo";
    }

    /// `--otomo-base-url=<url>` after `--` on the command line wins over the project
    /// setting, so a developer can point a build at an SSH tunnel (guide §10.1).
    static string ResolveBaseUrl()
    {
        foreach (var arg in OS.GetCmdlineUserArgs())
            if (arg.StartsWith("--otomo-base-url=", StringComparison.Ordinal))
                return arg["--otomo-base-url=".Length..];
        var url = (string)ProjectSettings.GetSetting(SettingBaseUrl, "");
        if (string.IsNullOrWhiteSpace(url))
            throw new InvalidOperationException($"Otomo: set Project Settings → {SettingBaseUrl} (the gateway base URL).");
        return url;
    }

    static string ResolveClientVersion()
    {
        var v = (string)ProjectSettings.GetSetting(SettingClientVersion, "");
        if (string.IsNullOrWhiteSpace(v)) v = (string)ProjectSettings.GetSetting("application/config/version", "");
        return string.IsNullOrWhiteSpace(v) ? "0.0.0" : v;
    }

    sealed class GodotPackMounter : IPackMounter
    {
        // replaceFiles: false: a pack may add content but not replace files shipped with the
        // game (guide §5.8). Change only after agreeing it with the team.
        public bool Mount(string osPath, ManifestPack pack) => ProjectSettings.LoadResourcePack(osPath, replaceFiles: false);
    }
}
