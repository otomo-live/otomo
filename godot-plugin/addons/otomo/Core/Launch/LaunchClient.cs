#nullable enable
using System;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;
using OtomoSdk.Http;
using OtomoSdk.Session;

namespace OtomoSdk.Launch;

/// <inheritdoc cref="ILaunchClient"/>
public sealed class LaunchClient : ILaunchClient
{
    private readonly IPartyClient _party;
    private readonly IJoinHandshakeClient _handshake;

    public LaunchClient(IEventsClient events, IPartyClient party, IJoinHandshakeClient handshake)
    {
        _party = party ?? throw new ArgumentNullException(nameof(party));
        _handshake = handshake ?? throw new ArgumentNullException(nameof(handshake));
        if (events is null) throw new ArgumentNullException(nameof(events));

        events.EventReceived += OnEvent;
    }

    public event Action<LaunchingInfo>? Launching;
    public event Action<LaunchFailedInfo>? LaunchFailed;
    public event Action<ReturnedInfo>? Returned;

    public async Task<JoinHandshakeResult> JoinAsync(LaunchingInfo info, CancellationToken ct = default)
    {
        var result = (await _handshake.ConnectAsync(info.Address, info.Port, info.Ticket, ct))
            .For(info.Address, info.Port, info.Ticket);

        if (result.Outcome != JoinHandshakeOutcome.Refused ||
            result.RefusalReason is not (JoinRefusalReason.Expired or JoinRefusalReason.Reused))
            return result;

        // guide §8.4: "On OTNO 2 or OTNO 3, get a fresh ticket (§8.3) and start again from
        // step 1." Capped at one retry: if the fresh ticket is refused too, something more
        // persistent is wrong (the match already ended, say), and looping would not help.
        var fresh = await _party.GetLaunchTicketAsync(ct);
        return (await _handshake.ConnectAsync(fresh.Address, fresh.Port, fresh.Ticket, ct))
            .For(fresh.Address, fresh.Port, fresh.Ticket);
    }

    private void OnEvent(SessionEvent evt)
    {
        switch (evt.Type)
        {
            case "party.launching":
                if (TryParse<LaunchingInfo>(evt, out var launching))
                    Launching?.Invoke(launching);
                break;

            case "party.launch_failed":
                if (TryParse<LaunchFailedInfo>(evt, out var failed))
                    LaunchFailed?.Invoke(failed);
                break;

            case "party.returned":
                if (TryParse<ReturnedInfo>(evt, out var returned))
                    Returned?.Invoke(returned);
                break;
        }
    }

    /// Malformed payloads are dropped rather than thrown from an event-raising callback
    /// (events are hints, per the guide, and there is no caller here to catch an exception).
    private static bool TryParse<T>(SessionEvent evt, out T value) where T : class
    {
        try
        {
            var parsed = evt.Payload.Deserialize<T>(OtomoHttp.Json);
            if (parsed is not null)
            {
                value = parsed;
                return true;
            }
        }
        catch (JsonException)
        {
        }

        value = null!;
        return false;
    }
}
