#nullable enable
using System.Threading;
using System.Threading.Tasks;

namespace OtomoSdk.Launch;

/// Why the proxy refused a ticket (guide §8.4, doc 14 §8's reason byte after "OTNO").
public enum JoinRefusalReason
{
    /// Malformed ticket, bad signature, or wrong iss/aud/alg.
    Invalid = 1,
    /// Correctly signed but past its expiry (plus leeway).
    Expired = 2,
    /// The ticket's jti was already accepted from another address -- tickets are single-use.
    Reused = 3,
    /// The ticket's srv is not in the proxy's server directory.
    UnknownServer = 4,
}

/// How the handshake ended.
public enum JoinHandshakeOutcome
{
    /// The proxy answered "OTOK". LocalPort is the port to bind ENet's client to.
    Accepted,
    /// The proxy answered "OTNO" plus a reason. Retrying with the same ticket will not
    /// help; Expired or Reused means get a fresh ticket (guide §8.3) and start over.
    Refused,
    /// No answer after every resend. The proxy or the network path to it may be down.
    NoAnswer,
}

/// Result of one handshake attempt (one ticket, one round of resends).
public sealed class JoinHandshakeResult
{
    public JoinHandshakeOutcome Outcome { get; }

    /// Valid when Outcome is Accepted: the local UDP port the handshake was sent from,
    /// which ENetMultiplayerPeer.CreateClient must bind to as well (guide §8.4 step 3) --
    /// the proxy maps traffic by source address, and that mapping was learned from this
    /// port.
    public int LocalPort { get; }

    /// Valid when Outcome is Refused.
    public JoinRefusalReason? RefusalReason { get; }

    /// The proxy address, port and ticket this attempt used. Set by ILaunchClient.JoinAsync,
    /// which may have swapped in a fresh ticket (and with it a fresh address) after an
    /// expired/reused refusal: these, not the party.launching event's, are what ENet must
    /// connect to and what to send via SceneMultiplayer.SendAuth.
    public string Address { get; private init; } = "";
    public int Port { get; private init; }
    public string Ticket { get; private init; } = "";

    private JoinHandshakeResult(JoinHandshakeOutcome outcome, int localPort, JoinRefusalReason? reason)
    {
        Outcome = outcome;
        LocalPort = localPort;
        RefusalReason = reason;
    }

    internal JoinHandshakeResult For(string address, int port, string ticket) =>
        new(Outcome, LocalPort, RefusalReason) { Address = address, Port = port, Ticket = ticket };

    public static JoinHandshakeResult Accepted(int localPort) => new(JoinHandshakeOutcome.Accepted, localPort, null);
    public static JoinHandshakeResult Refused(JoinRefusalReason reason) => new(JoinHandshakeOutcome.Refused, 0, reason);
    public static readonly JoinHandshakeResult NoAnswer = new(JoinHandshakeOutcome.NoAnswer, 0, null);
}

/// The Gameplay Proxy's UDP join handshake (guide §8.4, doc 14 §8). Plain .NET sockets, no
/// Godot APIs, so it is unit-testable against a real local UDP "fake proxy" with `dotnet
/// test`. The Godot layer (Otomo.cs / the game) takes a successful result's LocalPort and
/// creates the actual ENetMultiplayerPeer -- this type never touches ENet or SceneMultiplayer.
public interface IJoinHandshakeClient
{
    /// Sends "OTJ1" + the ticket to address:port from a fresh local port, and waits for
    /// "OTOK" or "OTNO"+reason, resending on silence. Never throws for a normal protocol
    /// outcome (accepted/refused/no answer); only for a local problem (can't resolve the
    /// address, can't open a socket) or genuine cancellation.
    Task<JoinHandshakeResult> ConnectAsync(string address, int port, string ticket, CancellationToken ct = default);
}
