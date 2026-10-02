# How games go online

To connect a game to otomo, it helps to know how two programs on different computers find
each other and talk. This page covers that from the ground up: clients and servers,
addresses, names, ports, the two kinds of connection, and firewalls. No prior networking
knowledge is assumed.

## Clients and servers

Every conversation on the internet has two sides:

- A **server** is a program that sits and waits for others to contact it. It runs all the
  time, usually on a computer in a data centre that nobody sits in front of.
- A **client** is a program that contacts a server when it needs something.

Your Godot game is a client. Otomo is a collection of servers. A game server (the program
that runs a match) is a server as well: your game connects to it as a client.

The word "server" means two things, and people switch between them freely:

- **The program** that waits and answers ("the Patch server").
- **The computer** those programs run on ("our server is in Singapore").

One computer usually runs many server programs at once. A VM running otomo runs
about twenty.

## Addresses: IP addresses

To send anything to a computer, you need its address. On the internet that's an **IP
address**, a number such as `203.0.113.10`. Every computer reachable from the internet has
one, like a street address for a building.

Two special addresses come up in this wiki:

| Address | Meaning |
|---|---|
| `127.0.0.1` | "This same computer." A program uses it to reach another program on its own machine. Also known as **localhost** or the **loopback** address. |
| `0.0.0.0` | Used by a *server* to say "accept connections from anywhere", not only from this computer. |

## Names: DNS

Numbers are hard to remember, so computers also have **names**, such as
`play.example.com`. A worldwide lookup system, **DNS** (Domain Name System), turns a name
into its IP address, much like a phone's contact list turns "Mum" into a number. Your game
uses the name, and the operating system looks up the number for it.

A name has no meaning on its own: whoever controls it decides which IP address it points
to. That's why moving otomo to a new computer mostly means pointing the name at the new
address.

## Ports: which program on the computer

An IP address gets a message to the right *computer*. That computer runs many server
programs, so the message also needs to say which *program* it's for. That's the **port**,
a number from 1 to 65535. Think of the IP address as a building and the port as a flat
number inside it.

A server program **listens** on a port: it tells the operating system "send me anything
that arrives for port 443". A few ports are traditional:

| Port | Normally used for |
|---|---|
| 22 | SSH, the secure remote terminal ([Command-line basics](command-line-basics.md)) |
| 80 | Websites without encryption (`http://`) |
| 443 | Websites with encryption (`https://`) |

Otomo uses others too: for example, the Gameplay Proxy listens on **27000**. You'll often see
an address and a port written together with a colon: `play.example.com:27000`, or
`127.0.0.1:8090`.

!!! note "The client has a port too"
    When your game contacts a server, the operating system gives your game a temporary
    port of its own (the **local port**), so replies can find their way back. Usually you
    never see it. Joining a match through otomo is the one place where it matters, and
    [Parties and matches](parties-and-matches.md) explains why.

## Two ways to send data: TCP and UDP

Programs send data in small chunks called **packets**. There are two common ways to send
them, and a game uses both.

**TCP** is like a phone call. The two sides first **connect**, then everything sent arrives
complete and in order. If a packet gets lost, TCP sends it again automatically. Anything
where every byte matters uses TCP: websites, downloads, logins. All of otomo's services
except the Gameplay Proxy use TCP.

**UDP** is like throwing postcards. There's no connection: each packet (called a
**datagram**) is sent on its own and may arrive late, out of order, or not at all. That
sounds worse, but it's *faster*, and for a real-time game a late position update is
useless anyway: you'd rather have the next one. Godot's multiplayer (ENet) runs over UDP
and adds its own "resend if lost" for the messages that need it. The Gameplay Proxy and
game servers use UDP.

## Firewalls: which doors are open

A **firewall** is a gatekeeper that decides which ports on a computer the outside world can
reach. A server that's listening on a port still can't be reached if the firewall keeps
that port closed. On your VM, these are the only ports otomo opens:

| Open to the internet | For |
|---|---|
| 22 (TCP) | SSH, for you and your team |
| 80 and 443 (TCP) | The public website address and otomo's player services |
| 27000 (UDP) | The Gameplay Proxy, the door to the game servers |

Everything else, including the admin website, is only reachable *from the server itself*.
To use those, you connect to the server with SSH first and ask it to forward a port to you
(an **SSH tunnel**; see [Use the admin website](../guide/admin-website.md)). This is
deliberate. It means someone on the internet can't even try to log in to the admin website.

Home routers do something similar, called **NAT**: your whole household shares one public
IP address, and the router remembers which inside computer started which conversation so it
can pass replies back. That's why a home PC can easily *contact* servers but usually can't
*be* one that strangers connect to. It's also why online games run their servers in data
centres.

## Encryption: HTTPS and TLS

Anything sent over the internet passes through many other computers on the way. Without
protection, any of them could read it. **TLS** is the standard encryption that stops that:
the two sides agree on a secret key, and everything in between sees only scrambled bytes.
TLS also proves the server really is who it claims to be, using a **certificate**: a
signed document saying "this key belongs to `play.example.com`".

When a web address starts with `https://`, the `s` means TLS is on. All of otomo's public
traffic uses it.

## Putting it together

When your game starts and asks otomo for updates, this happens:

1. The game wants `https://play.example.com/patch/v1/live/manifest`.
2. DNS turns `play.example.com` into `203.0.113.10`.
3. The game opens a TCP connection to `203.0.113.10`, port **443** (the default for
   `https://`).
4. TLS scrambles the connection and checks the server's certificate.
5. The game sends its request, and the server sends back the answer.

The next page, [HTTP and APIs](http-and-apis.md), explains step 5: what the game actually
says, and what the server says back.
