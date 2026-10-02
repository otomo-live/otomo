#nullable enable
using System;
using System.Collections.Generic;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;
using OtomoSdk.Http;
using OtomoSdk.Launch;
using OtomoSdk.Session;
using Xunit;

namespace Otomo.Sdk.Tests;

public sealed class LaunchClientTests
{
    private sealed class FakeEventsClient : IEventsClient
    {
        public bool IsRunning { get; private set; }
        public void Start(CancellationToken ct = default) => IsRunning = true;
        public Task StopAsync() { IsRunning = false; return Task.CompletedTask; }
        public event Action<SessionEvent>? EventReceived;
        public event Action? Resync;
        public void Raise(SessionEvent evt) => EventReceived?.Invoke(evt);
        public void RaiseResync() => Resync?.Invoke();
    }

    private sealed class FakePartyClient : IPartyClient
    {
        public LaunchTicket? TicketToReturn { get; set; }
        public int GetLaunchTicketCalls { get; private set; }

        public Task<Party> CreateAsync(CancellationToken ct = default) => throw new NotImplementedException();
        public Task<Party> GetAsync(CancellationToken ct = default) => throw new NotImplementedException();
        public Task<PartyInvite> InviteAsync(string playerId, CancellationToken ct = default) => throw new NotImplementedException();
        public Task<Party> AcceptInviteAsync(string inviteId, CancellationToken ct = default) => throw new NotImplementedException();
        public Task DeclineInviteAsync(string inviteId, CancellationToken ct = default) => throw new NotImplementedException();
        public Task LeaveAsync(CancellationToken ct = default) => throw new NotImplementedException();
        public Task<Party> KickAsync(string playerId, int revision, CancellationToken ct = default) => throw new NotImplementedException();
        public Task<Party> PromoteAsync(string playerId, int revision, CancellationToken ct = default) => throw new NotImplementedException();

        public Task<Party> UpdateSettingsAsync(
            IReadOnlyDictionary<string, string> settings, int revision, CancellationToken ct = default) =>
            throw new NotImplementedException();

        public Task<Party> SetReadyAsync(bool ready, CancellationToken ct = default) => throw new NotImplementedException();
        public Task<Party> LaunchAsync(int revision, CancellationToken ct = default) => throw new NotImplementedException();

        public Task<LaunchTicket> GetLaunchTicketAsync(CancellationToken ct = default)
        {
            GetLaunchTicketCalls++;
            return Task.FromResult(TicketToReturn ?? throw new InvalidOperationException("no ticket configured"));
        }
    }

    private sealed class FakeHandshakeClient : IJoinHandshakeClient
    {
        private readonly Queue<JoinHandshakeResult> _results = new();
        public List<(string Address, int Port, string Ticket)> Calls { get; } = new();

        public FakeHandshakeClient Respond(JoinHandshakeResult result)
        {
            _results.Enqueue(result);
            return this;
        }

        public Task<JoinHandshakeResult> ConnectAsync(
            string address, int port, string ticket, CancellationToken ct = default)
        {
            Calls.Add((address, port, ticket));
            var result = _results.Count > 0 ? _results.Dequeue() : JoinHandshakeResult.NoAnswer;
            return Task.FromResult(result);
        }
    }

    private static SessionEvent MakeEvent(string type, string payloadJson)
    {
        var json = $"{{\"seq\":1,\"at\":1000,\"type\":\"{type}\",\"payload\":{payloadJson}}}";
        return JsonSerializer.Deserialize<SessionEvent>(json, OtomoHttp.Json)!;
    }

    [Fact]
    public void PartyLaunchingEvent_RaisesTypedLaunchingEvent()
    {
        var events = new FakeEventsClient();
        var launch = new LaunchClient(events, new FakePartyClient(), new FakeHandshakeClient());
        LaunchingInfo? received = null;
        launch.Launching += info => received = info;

        events.Raise(MakeEvent("party.launching",
            "{\"allocation_id\":\"a1\",\"address\":\"1.2.3.4\",\"port\":5040,\"ticket\":\"tok\"," +
            "\"ticket_expires_at\":\"2026-01-01T00:01:00Z\"}"));

        Assert.NotNull(received);
        Assert.Equal("a1", received!.AllocationId);
        Assert.Equal("1.2.3.4", received.Address);
        Assert.Equal(5040, received.Port);
        Assert.Equal("tok", received.Ticket);
    }

    [Fact]
    public void PartyLaunchFailedEvent_RaisesTypedEvent()
    {
        var events = new FakeEventsClient();
        var launch = new LaunchClient(events, new FakePartyClient(), new FakeHandshakeClient());
        LaunchFailedInfo? received = null;
        launch.LaunchFailed += info => received = info;

        events.Raise(MakeEvent("party.launch_failed", "{\"reason\":\"no_capacity\"}"));

        Assert.NotNull(received);
        Assert.Equal("no_capacity", received!.Reason);
    }

    [Fact]
    public void PartyReturnedEvent_RaisesTypedEvent()
    {
        var events = new FakeEventsClient();
        var launch = new LaunchClient(events, new FakePartyClient(), new FakeHandshakeClient());
        ReturnedInfo? received = null;
        launch.Returned += info => received = info;

        events.Raise(MakeEvent("party.returned", "{\"reason\":\"server_dead\"}"));

        Assert.NotNull(received);
        Assert.Equal("server_dead", received!.Reason);
    }

    [Fact]
    public void UnrelatedEventType_IsIgnored()
    {
        var events = new FakeEventsClient();
        var launch = new LaunchClient(events, new FakePartyClient(), new FakeHandshakeClient());
        var fired = false;
        launch.Launching += _ => fired = true;
        launch.LaunchFailed += _ => fired = true;
        launch.Returned += _ => fired = true;

        events.Raise(MakeEvent("friend.request", "{\"player_id\":\"p1\"}"));

        Assert.False(fired);
    }

    [Fact]
    public async Task JoinAsync_PassesInfoFieldsToHandshake()
    {
        var handshake = new FakeHandshakeClient().Respond(JoinHandshakeResult.Accepted(4242));
        var launch = new LaunchClient(new FakeEventsClient(), new FakePartyClient(), handshake);
        var info = new LaunchingInfo { Address = "1.2.3.4", Port = 5040, Ticket = "tok-1" };

        var result = await launch.JoinAsync(info);

        Assert.Equal(JoinHandshakeOutcome.Accepted, result.Outcome);
        Assert.Equal(4242, result.LocalPort);
        var call = Assert.Single(handshake.Calls);
        Assert.Equal("1.2.3.4", call.Address);
        Assert.Equal(5040, call.Port);
        Assert.Equal("tok-1", call.Ticket);
        Assert.Equal("tok-1", result.Ticket);
        Assert.Equal("1.2.3.4", result.Address);
        Assert.Equal(5040, result.Port);
    }

    [Fact]
    public async Task JoinAsync_OnExpired_FetchesFreshTicketAndRetriesOnce()
    {
        var handshake = new FakeHandshakeClient()
            .Respond(JoinHandshakeResult.Refused(JoinRefusalReason.Expired))
            .Respond(JoinHandshakeResult.Accepted(1111));
        var party = new FakePartyClient
        {
            TicketToReturn = new LaunchTicket { Address = "5.6.7.8", Port = 6000, Ticket = "fresh-1" },
        };
        var launch = new LaunchClient(new FakeEventsClient(), party, handshake);
        var info = new LaunchingInfo { Address = "1.2.3.4", Port = 5040, Ticket = "stale-1" };

        var result = await launch.JoinAsync(info);

        Assert.Equal(JoinHandshakeOutcome.Accepted, result.Outcome);
        Assert.Equal(1, party.GetLaunchTicketCalls);
        Assert.Equal(2, handshake.Calls.Count);
        Assert.Equal("stale-1", handshake.Calls[0].Ticket);
        Assert.Equal("fresh-1", handshake.Calls[1].Ticket);
        Assert.Equal("5.6.7.8", handshake.Calls[1].Address);

        // The game must connect to, and present, what was actually accepted.
        Assert.Equal("fresh-1", result.Ticket);
        Assert.Equal("5.6.7.8", result.Address);
        Assert.Equal(6000, result.Port);
    }

    [Fact]
    public async Task JoinAsync_OnReused_FetchesFreshTicketAndRetriesOnce()
    {
        var handshake = new FakeHandshakeClient()
            .Respond(JoinHandshakeResult.Refused(JoinRefusalReason.Reused))
            .Respond(JoinHandshakeResult.Accepted(1111));
        var party = new FakePartyClient
        {
            TicketToReturn = new LaunchTicket { Address = "5.6.7.8", Port = 6000, Ticket = "fresh-1" },
        };
        var launch = new LaunchClient(new FakeEventsClient(), party, handshake);

        var result = await launch.JoinAsync(new LaunchingInfo { Address = "1.2.3.4", Port = 5040, Ticket = "stale-1" });

        Assert.Equal(JoinHandshakeOutcome.Accepted, result.Outcome);
        Assert.Equal(1, party.GetLaunchTicketCalls);
    }

    [Fact]
    public async Task JoinAsync_OnInvalidReason_DoesNotRetry()
    {
        var handshake = new FakeHandshakeClient().Respond(JoinHandshakeResult.Refused(JoinRefusalReason.Invalid));
        var party = new FakePartyClient();
        var launch = new LaunchClient(new FakeEventsClient(), party, handshake);

        var result = await launch.JoinAsync(new LaunchingInfo { Address = "1.2.3.4", Port = 5040, Ticket = "t" });

        Assert.Equal(JoinHandshakeOutcome.Refused, result.Outcome);
        Assert.Equal(JoinRefusalReason.Invalid, result.RefusalReason);
        Assert.Equal(0, party.GetLaunchTicketCalls);
        Assert.Single(handshake.Calls);
    }

    [Fact]
    public async Task JoinAsync_SecondAttemptAlsoRefused_DoesNotLoopAgain()
    {
        var handshake = new FakeHandshakeClient()
            .Respond(JoinHandshakeResult.Refused(JoinRefusalReason.Expired))
            .Respond(JoinHandshakeResult.Refused(JoinRefusalReason.Expired));
        var party = new FakePartyClient
        {
            TicketToReturn = new LaunchTicket { Address = "5.6.7.8", Port = 6000, Ticket = "fresh-1" },
        };
        var launch = new LaunchClient(new FakeEventsClient(), party, handshake);

        var result = await launch.JoinAsync(new LaunchingInfo { Address = "1.2.3.4", Port = 5040, Ticket = "stale-1" });

        Assert.Equal(JoinHandshakeOutcome.Refused, result.Outcome);
        Assert.Equal(1, party.GetLaunchTicketCalls);
        Assert.Equal(2, handshake.Calls.Count);
    }

    [Fact]
    public async Task JoinAsync_NoAnswer_DoesNotRetry()
    {
        var handshake = new FakeHandshakeClient().Respond(JoinHandshakeResult.NoAnswer);
        var party = new FakePartyClient();
        var launch = new LaunchClient(new FakeEventsClient(), party, handshake);

        var result = await launch.JoinAsync(new LaunchingInfo { Address = "1.2.3.4", Port = 5040, Ticket = "t" });

        Assert.Equal(JoinHandshakeOutcome.NoAnswer, result.Outcome);
        Assert.Equal(0, party.GetLaunchTicketCalls);
    }
}
