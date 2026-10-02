#nullable enable
using System;
using System.Net;
using System.Net.Sockets;
using System.Text;
using System.Threading;
using System.Threading.Tasks;
using OtomoSdk.Launch;
using Xunit;

namespace Otomo.Sdk.Tests;

/// These tests run the real handshake against a real loopback UDP socket playing the
/// proxy's part (guide §8.4 calls this "a local fake proxy" in the acceptance criteria) --
/// no HttpClient/FakeHandler involved, since this is a different transport entirely.
public sealed class JoinHandshakeClientTests
{
    private sealed class FakeProxy : IDisposable
    {
        private readonly UdpClient _socket;
        private IPEndPoint? _lastSender;

        public FakeProxy()
        {
            _socket = new UdpClient(new IPEndPoint(IPAddress.Loopback, 0));
        }

        public int Port => ((IPEndPoint)_socket.Client.LocalEndPoint!).Port;

        public async Task<byte[]> ReceiveAsync(CancellationToken ct = default)
        {
            var result = await _socket.ReceiveAsync(ct);
            _lastSender = result.RemoteEndPoint;
            return result.Buffer;
        }

        public async Task ReplyAsync(byte[] datagram)
        {
            if (_lastSender is null)
                throw new InvalidOperationException("FakeProxy: no datagram received yet");
            await _socket.SendAsync(datagram, _lastSender);
        }

        public void Dispose() => _socket.Dispose();
    }

    private static Task AwaitOrTimeout(Task task) => task.WaitAsync(TimeSpan.FromSeconds(5));

    private static Task<T> AwaitOrTimeout<T>(Task<T> task) => task.WaitAsync(TimeSpan.FromSeconds(5));

    [Fact]
    public async Task ConnectAsync_ReceivesOTOK_ReturnsAcceptedWithLocalPort()
    {
        using var proxy = new FakeProxy();
        var server = Task.Run(async () =>
        {
            await proxy.ReceiveAsync();
            await proxy.ReplyAsync(Encoding.ASCII.GetBytes("OTOK"));
        });

        var client = new JoinHandshakeClient();
        var result = await AwaitOrTimeout(client.ConnectAsync("127.0.0.1", proxy.Port, "ticket-1"));
        await AwaitOrTimeout(server);

        Assert.Equal(JoinHandshakeOutcome.Accepted, result.Outcome);
        Assert.True(result.LocalPort > 0, "expected a real local port");
    }

    [Fact]
    public async Task ConnectAsync_ReceivesOTNO_ReturnsRefusedWithReason()
    {
        using var proxy = new FakeProxy();
        var server = Task.Run(async () =>
        {
            await proxy.ReceiveAsync();
            await proxy.ReplyAsync(new byte[] { (byte)'O', (byte)'T', (byte)'N', (byte)'O', 2 });
        });

        var client = new JoinHandshakeClient();
        var result = await AwaitOrTimeout(client.ConnectAsync("127.0.0.1", proxy.Port, "ticket-1"));
        await AwaitOrTimeout(server);

        Assert.Equal(JoinHandshakeOutcome.Refused, result.Outcome);
        Assert.Equal(JoinRefusalReason.Expired, result.RefusalReason);
    }

    [Theory]
    [InlineData(1, JoinRefusalReason.Invalid)]
    [InlineData(3, JoinRefusalReason.Reused)]
    [InlineData(4, JoinRefusalReason.UnknownServer)]
    public async Task ConnectAsync_ReceivesOTNO_MapsEveryReasonByte(byte reasonByte, JoinRefusalReason expected)
    {
        using var proxy = new FakeProxy();
        var server = Task.Run(async () =>
        {
            await proxy.ReceiveAsync();
            await proxy.ReplyAsync(new byte[] { (byte)'O', (byte)'T', (byte)'N', (byte)'O', reasonByte });
        });

        var client = new JoinHandshakeClient();
        var result = await AwaitOrTimeout(client.ConnectAsync("127.0.0.1", proxy.Port, "ticket-1"));
        await AwaitOrTimeout(server);

        Assert.Equal(expected, result.RefusalReason);
    }

    [Fact]
    public async Task ConnectAsync_EncodesHandshakeAsMagicLengthThenTicketBytes()
    {
        using var proxy = new FakeProxy();
        byte[]? received = null;
        var server = Task.Run(async () =>
        {
            received = await proxy.ReceiveAsync();
            await proxy.ReplyAsync(Encoding.ASCII.GetBytes("OTOK"));
        });

        var client = new JoinHandshakeClient();
        await AwaitOrTimeout(client.ConnectAsync("127.0.0.1", proxy.Port, "abc"));
        await AwaitOrTimeout(server);

        Assert.NotNull(received);
        Assert.Equal("OTJ1", Encoding.ASCII.GetString(received!, 0, 4));
        var length = (received![4] << 8) | received[5];
        Assert.Equal(3, length);
        Assert.Equal("abc", Encoding.UTF8.GetString(received, 6, length));
    }

    [Fact]
    public async Task ConnectAsync_IgnoresUnrelatedDatagram_ThenAccepts()
    {
        using var proxy = new FakeProxy();
        var server = Task.Run(async () =>
        {
            await proxy.ReceiveAsync();
            await proxy.ReplyAsync(Encoding.ASCII.GetBytes("XXXX"));
            await proxy.ReplyAsync(Encoding.ASCII.GetBytes("OTOK"));
        });

        var client = new JoinHandshakeClient();
        var result = await AwaitOrTimeout(client.ConnectAsync("127.0.0.1", proxy.Port, "ticket-1"));
        await AwaitOrTimeout(server);

        Assert.Equal(JoinHandshakeOutcome.Accepted, result.Outcome);
    }

    [Fact]
    public async Task ConnectAsync_NoAnswer_ResendsAFixedNumberOfTimesThenGivesUp()
    {
        using var proxy = new FakeProxy();
        var received = 0;
        using var stop = new CancellationTokenSource();
        var server = Task.Run(async () =>
        {
            try
            {
                while (true)
                {
                    await proxy.ReceiveAsync(stop.Token);
                    Interlocked.Increment(ref received);
                }
            }
            catch (OperationCanceledException)
            {
            }
        });

        var client = new JoinHandshakeClient();
        var result = await client.ConnectAsync("127.0.0.1", proxy.Port, "ticket-1")
            .WaitAsync(TimeSpan.FromSeconds(10));

        // The last resend's datagram is sent right as ConnectAsync gives up; give the
        // proxy's receive loop a moment to actually process it (loopback, so this is
        // generous) before stopping it and reading the count it saw.
        await Task.Delay(100);
        stop.Cancel();
        try
        {
            await server.WaitAsync(TimeSpan.FromSeconds(2));
        }
        catch (OperationCanceledException)
        {
        }

        Assert.Equal(JoinHandshakeOutcome.NoAnswer, result.Outcome);
        Assert.Equal(JoinHandshakeClient.Attempts, received);
    }
}
