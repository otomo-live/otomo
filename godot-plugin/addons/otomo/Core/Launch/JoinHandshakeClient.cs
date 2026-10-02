#nullable enable
using System;
using System.Net;
using System.Net.Sockets;
using System.Text;
using System.Threading;
using System.Threading.Tasks;

namespace OtomoSdk.Launch;

/// <inheritdoc cref="IJoinHandshakeClient"/>
public sealed class JoinHandshakeClient : IJoinHandshakeClient
{
    private static readonly byte[] Magic = Encoding.ASCII.GetBytes("OTJ1");

    /// guide §8.4 / doc 14 §8: resend up to this many times, this far apart, until the
    /// proxy answers.
    public static readonly int Attempts = 5;
    public static readonly TimeSpan Interval = TimeSpan.FromMilliseconds(500);

    public async Task<JoinHandshakeResult> ConnectAsync(
        string address, int port, string ticket, CancellationToken ct = default)
    {
        var ip = await ResolveAsync(address, ct);
        var packet = EncodeHandshake(ticket);

        using var udp = new UdpClient(0);
        udp.Connect(ip, port);

        for (var attempt = 0; attempt < Attempts; attempt++)
        {
            await udp.SendAsync(packet, ct);

            using var wait = CancellationTokenSource.CreateLinkedTokenSource(ct);
            wait.CancelAfter(Interval);

            try
            {
                while (true)
                {
                    var received = await udp.ReceiveAsync(wait.Token);
                    var (reply, reason) = DecodeReply(received.Buffer);

                    if (reply == RawReply.Ok)
                    {
                        var localPort = ((IPEndPoint)udp.Client.LocalEndPoint!).Port;
                        return JoinHandshakeResult.Accepted(localPort);
                    }

                    if (reply == RawReply.Refused)
                        return JoinHandshakeResult.Refused(reason);

                    // Not a handshake answer (wrong magic/shape, or a bogus reason byte);
                    // keep waiting out this attempt's window in case the real answer is
                    // still coming.
                }
            }
            catch (OperationCanceledException) when (!ct.IsCancellationRequested)
            {
                // This attempt's window elapsed with nothing usable; resend.
            }
        }

        return JoinHandshakeResult.NoAnswer;
    }

    private static async Task<IPAddress> ResolveAsync(string address, CancellationToken ct)
    {
        if (IPAddress.TryParse(address, out var literal))
            return literal;

        var addresses = await Dns.GetHostAddressesAsync(address, AddressFamily.InterNetwork, ct);
        if (addresses.Length == 0)
            throw new InvalidOperationException($"cannot resolve {address}");

        return addresses[0];
    }

    private static byte[] EncodeHandshake(string ticket)
    {
        var ticketBytes = Encoding.UTF8.GetBytes(ticket);
        if (ticketBytes.Length > ushort.MaxValue)
            throw new ArgumentException("ticket is too long for the handshake's u16 length field", nameof(ticket));

        var packet = new byte[Magic.Length + 2 + ticketBytes.Length];
        Buffer.BlockCopy(Magic, 0, packet, 0, Magic.Length);
        packet[Magic.Length] = (byte)(ticketBytes.Length >> 8);
        packet[Magic.Length + 1] = (byte)ticketBytes.Length;
        Buffer.BlockCopy(ticketBytes, 0, packet, Magic.Length + 2, ticketBytes.Length);
        return packet;
    }

    private enum RawReply
    {
        /// Not "OTOK" or a well-formed "OTNO"+reason -- stray traffic, ignore and keep
        /// waiting within the same attempt's window.
        Unrecognized,
        Ok,
        Refused,
    }

    private static (RawReply Reply, JoinRefusalReason Reason) DecodeReply(byte[] datagram)
    {
        if (datagram.Length >= 4 && Ascii(datagram) == "OTOK")
            return (RawReply.Ok, default);

        if (datagram.Length >= 5 && Ascii(datagram) == "OTNO")
        {
            var reason = (JoinRefusalReason)datagram[4];
            if (Enum.IsDefined(typeof(JoinRefusalReason), reason))
                return (RawReply.Refused, reason);
        }

        return (RawReply.Unrecognized, default);
    }

    private static string Ascii(byte[] data) => Encoding.ASCII.GetString(data, 0, 4);
}
