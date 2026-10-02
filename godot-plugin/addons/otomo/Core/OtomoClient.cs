#nullable enable
using System;
using System.Net.Http;
using System.Threading;
using System.Threading.Tasks;
using OtomoSdk.Auth;
using OtomoSdk.Http;
using OtomoSdk.Launch;
using OtomoSdk.Patch;
using OtomoSdk.Session;

namespace OtomoSdk;

/// Builds the sub-clients and runs the start-up sequence (guide §9) as far as the server
/// supports it today: Patch → Auth → the first Session call. Godot-free, so the whole flow
/// can run from a console test; the Otomo autoload node wraps it.
public sealed class OtomoClient
{
    public OtomoConfig Config { get; }
    public OtomoHttp Http { get; }
    public RemoteConfig RemoteConfig { get; }
    public PatchClient Patch { get; }
    public AuthClient Auth { get; }
    public SessionClient Session { get; }
    public PresenceClient Presence { get; }
    public EventsClient Events { get; }
    public FriendsClient Friends { get; }
    public PartyClient Party { get; }
    public LaunchClient Launch { get; }

    /// `http` must be shared for the whole game (guide §3.2 rule 1) and have an infinite
    /// Timeout: the SDK sets a timeout per request.
    public OtomoClient(HttpClient http, OtomoConfig config, IPackMounter mounter, ISecretStore? secrets = null)
    {
        Config = config;
        Http = new OtomoHttp(http, config);
        RemoteConfig = new RemoteConfig(config.Log);
        Patch = new PatchClient(http, config, RemoteConfig, mounter);
        Auth = new AuthClient(Http, secrets ?? new FileSecretStore(config.DataDir), config);
        Session = new SessionClient(Http, Auth);
        Presence = new PresenceClient(Http, Auth);
        Events = new EventsClient(Http, Auth);
        Friends = new FriendsClient(Http, Auth);
        Party = new PartyClient(Http, Auth);
        Launch = new LaunchClient(Events, Party, new JoinHandshakeClient());

        // guide §7.2: the heartbeat loop runs exactly while logged in, with no game code
        // needed to start or stop it. SetStatusAsync (in_menus/away) is still the game's call.
        Auth.LoggedIn += () => Presence.Start(PresenceStatus.Online);
        Auth.LoggedOut += () => _ = Presence.StopAsync();

        // guide §7.3: run exactly one long-poll loop while logged in, same lifecycle
        // as Presence above.
        Auth.LoggedIn += () => Events.Start();
        Auth.LoggedOut += () => _ = Events.StopAsync();

        // D5 (doc 12 §7.5, decided 2026-09-29): every Session request carries the release
        // the client actually has loaded, and a 409 release_outdated re-patches and retries
        // once. Patch.Finished already fires after every RunAsync (including the in-menu
        // re-check), so this keeps the header current without any extra plumbing; a run that
        // left ReleaseId null (offline with nothing cached yet) leaves the header as it was.
        Patch.Finished += summary =>
        {
            if (summary.ReleaseId is long releaseId)
                Http.ExtraHeaders["X-Otomo-Release"] = releaseId.ToString();
        };
        Http.RefreshRelease = async ct =>
        {
            var result = await Patch.RunAsync(ct);
            return result is PatchResult.Ready or PatchResult.OfflineReady or PatchResult.Declined;
        };
    }

    /// Patch, then log in, then POST /me/init. Never throws OtomoError: every outcome is in
    /// the result, so a caller can show an offline / update-required / error state.
    public async Task<StartResult> StartAsync(CancellationToken ct = default)
    {
        var patch = await Patch.RunAsync(ct);
        if (patch == PatchResult.ClientTooOld)
            return new StartResult(patch, false, null, SessionStatus.NotAttempted, null);

        try
        {
            await Auth.StartAsync(ct);
        }
        catch (OtomoError e)
        {
            Config.Log?.Invoke($"otomo: login failed: {e}");
            return new StartResult(patch, false, null, SessionStatus.NotAttempted, e);
        }

        try
        {
            var profile = await Session.InitAsync(ct);
            return new StartResult(patch, true, profile, SessionStatus.Available, null);
        }
        catch (OtomoError e) when (e.Status == 501)
        {
            // Session is deployed but PLANNED: a 501 means the gateway and Session accepted
            // the token, which is all that can be verified today.
            Config.Log?.Invoke($"otomo: session not implemented yet; token accepted (request_id {e.RequestId})");
            return new StartResult(patch, true, null, SessionStatus.NotImplemented, null);
        }
        catch (OtomoError e)
        {
            Config.Log?.Invoke($"otomo: session init failed: {e}");
            return new StartResult(patch, true, null, SessionStatus.Failed, e);
        }
    }
}

public enum SessionStatus
{
    NotAttempted,
    /// POST /me/init answered with a profile.
    Available,
    /// Session answered 501: token accepted, routes not built yet.
    NotImplemented,
    Failed,
}

/// Outcome of OtomoClient.StartAsync. `Error` is the failure that stopped the sequence, if any.
public sealed record StartResult(
    PatchResult Patch,
    bool LoggedIn,
    PlayerProfile? Profile,
    SessionStatus Session,
    OtomoError? Error);
