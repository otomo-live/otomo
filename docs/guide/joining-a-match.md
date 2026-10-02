# Join a match

This guide adds a **Join** button to your game: it puts the player in a party, launches
it, and connects to a game server through otomo's Gameplay Proxy. It takes about 30
minutes. [Parties and matches](../concepts/parties-and-matches.md) explains the ideas
behind each step this code performs.

**You'll need:**

- the SDK installed and `StartAsync` called at start-up ([Install the SDK](install-the-sdk.md));
- at least one game server running on your otomo ([Run a new game server build](game-servers.md));
- a game that already uses Godot's multiplayer with **ENet** (`ENetMultiplayerPeer`).

## How joining works

```
 Your game                         otomo                          Game server
 ─────────                         ─────                          ───────────
 1. Party.CreateAsync()  ───────▶  Session: a party of one
 2. Party.SetReadyAsync(true) ──▶  Session: ready
 3. Party.LaunchAsync()  ───────▶  Session asks the Allocator for a free server
                                   Allocator reserves gs-1, writes a ticket
 4. PartyLaunching signal ◀──────  address, port, ticket (valid 60 s)
 5. JoinMatchAsync()     ───────▶  Gameplay Proxy checks the ticket: "OK"
 6. ENet CreateClient(…, localPort) ─────── through the proxy ──────▶  gs-1
```

Steps 1 to 3 are ordinary SDK calls. Step 4 arrives as a **signal** on the `Otomo`
autoload, because finding a server takes a moment and happens after `LaunchAsync`
returns. Steps 5 and 6 are the only part that's specific to otomo's proxy.

## Step 1: Put the player in a party and launch

Every launch starts from a party, even for one player. The party is also the **lobby**: it
holds the lobby settings your game allows (`session.rules`) and each member's ready flag.

```csharp
using Godot;
using OtomoSdk;
using OtomoSdk.Http;      // OtomoError
using OtomoSdk.Session;   // Party

public partial class JoinButton : Button
{
    public override void _Ready()
    {
        Pressed += OnPressed;
        Otomo.Instance.PartyLaunching += OnPartyLaunching;
        Otomo.Instance.PartyLaunchFailed += reason => Fail($"No server available ({reason}).");
        Otomo.Instance.PartyReturned += reason => GD.Print($"Back in the lobby ({reason}).");
    }

    private async void OnPressed()
    {
        Disabled = true; // stop double clicks while we work
        try
        {
            var party = Otomo.Instance.Client.Party;

            // Make a party of one, or use the one the player is already in.
            Party p;
            try { p = await party.CreateAsync(); }
            catch (OtomoError e) when (e.Code == "already_in_party") { p = await party.GetAsync(); }

            p = await party.SetReadyAsync(true);
            await party.LaunchAsync(p.Revision);
            GD.Print("Looking for a game server...");
            // Nothing more to do here: OnPartyLaunching runs when a server is reserved.
        }
        catch (OtomoError e)
        {
            Fail($"{e.Message} (request {e.RequestId})");
        }
    }
```

- `Revision` is the party's version number. Calls that only the leader can make send the
  revision they're looking at, so two changes made at the same moment can't silently
  overwrite each other.
- If anything goes wrong, the SDK throws an `OtomoError` with a fixed `Code` and a
  readable `Message` ([Error codes](../reference/errors.md)).

## Step 2: Connect when the server is ready

When the Allocator reserves a server, every member of the party gets the `PartyLaunching`
signal with the Gameplay Proxy's address and port and **their own join ticket**. Add these
methods to the same class:

```csharp
    private async void OnPartyLaunching(string allocationId, string address, int port,
                                        string ticket, long ticketExpiresAtUnixMs)
    {
        // 1. UDP: show the proxy the ticket. Retries once with a fresh ticket if needed.
        var join = await Otomo.Instance.JoinMatchAsync(address, port, ticket);
        if (join.Outcome != OtomoSdk.Launch.JoinHandshakeOutcome.Accepted)
        {
            Fail($"The proxy didn't let us in: {join.Outcome} {join.RefusalReason}");
            return;
        }

        // 2. ENet: connect to the proxy FROM THE SAME LOCAL PORT the handshake used.
        var peer = new ENetMultiplayerPeer();
        Error err = peer.CreateClient(join.Address, join.Port, localPort: join.LocalPort);
        if (err != Error.Ok)
        {
            Fail($"Could not start ENet: {err}");
            return;
        }
        Multiplayer.MultiplayerPeer = peer;
        // From here on it's ordinary Godot multiplayer: wait for ConnectedToServer, etc.
    }

    private void Fail(string message)
    {
        GD.PushWarning($"Join failed: {message}");
        Disabled = false;
    }
}
```

The only change from a normal ENet client is the `localPort:` argument. The proxy
recognises a player by the address and port their handshake came from. Without
`localPort`, ENet picks a random port, the proxy doesn't recognise the player, and the
connection times out.

Use `join.Address`, `join.Port` and `join.Ticket` rather than the signal's values: if the
first ticket had expired, `JoinMatchAsync` fetched a fresh one, possibly for a different
address.

## Step 3: Let the game server check the ticket too (recommended)

The proxy already refuses players without a valid ticket. For defence in depth, your game
server can check the same ticket itself, through Godot's multiplayer authentication. The
client side is a few lines before `CreateClient`:

```csharp
        var mp = (SceneMultiplayer)Multiplayer;
        mp.AuthCallback = new Callable(this, MethodName.OnAuthData);
        mp.PeerAuthenticating += id =>
        {
            mp.SendAuth((int)id, System.Text.Encoding.UTF8.GetBytes(join.Ticket));
            mp.CompleteAuth((int)id);
        };
```

with an empty `private void OnAuthData(int id, byte[] data) { }` method in the class. The
server side is described in
[Run a new game server build › What your game server must do](game-servers.md#what-your-game-server-must-do).
Turn the auth step on only when you have a ticket: a server running in the editor has no
check, and a client waiting for auth would time out.

## What can go wrong

| You see | What it means | What to do |
|---|---|---|
| `No server available (no_capacity)` | Every game server is busy, or none is registered | Check the game servers ([Run a new game server build](game-servers.md), Step 6) |
| `No server available (allocator_unavailable)` | The Allocator didn't answer | Check it on the VM: `docker compose ps allocator` |
| `The proxy didn't let us in: Refused Expired` | More than 60 seconds passed, twice | Press Join again |
| `The proxy didn't let us in: Refused UnknownServer` | The reserved game server restarted at an unlucky moment | Press Join again |
| `The proxy didn't let us in: NoAnswer` | The proxy didn't reply to five attempts | Check UDP 27000 is open in `ufw` and your cloud firewall ([Troubleshooting](../troubleshooting.md)) |
| `not_ready` error | Someone in the party isn't ready | Every member calls `SetReadyAsync(true)` |
| Connected, then disconnected at once | The game server itself refused the player | Read the game server's log |

!!! tip "A match from last time"
    If the game crashed during a match, the player's party may still be `in_game`. Call
    `Client.Party.GetLaunchTicketAsync()` to get a fresh ticket for that match and connect
    with it, or leave the party (`LeaveAsync`) and start again.

## When the match ends

When everyone has left, the game server tells otomo the match is over and restarts itself
with a fresh world. Every member gets the `PartyReturned` signal, the party is back in the
lobby with ready flags cleared, and pressing **Join** again starts a new match.

## Going further

The same `Client.Party` object invites friends (`InviteAsync`), changes lobby settings
(`UpdateSettingsAsync`), and hands over leadership (`PromoteAsync`). The
[SDK reference](../reference/sdk.md) lists them all.

## Next

- [Run a new game server build](game-servers.md): put your own game server behind this.
- [Troubleshooting](../troubleshooting.md).
