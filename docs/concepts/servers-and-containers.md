# Servers and containers

Otomo runs on one Linux computer, packaged with a tool called **Docker**. You only need
this page if you'll put game server builds online or run otomo yourself. It explains what
Linux, a virtual machine, a container and Compose are, in terms of things you already know.

## The server computer

Otomo runs on a **virtual machine** (VM) in a data centre, reachable at a name such as
`play.example.com`. A virtual machine is a computer simulated in software: one big
physical machine is split into many VMs, each of which behaves exactly like its own
computer, with its own operating system, disk and IP address. For our purposes, "the VM"
and "the server" mean the same thing.

The VM runs **Linux** (specifically Ubuntu), not Windows. Linux is the usual operating
system for servers: it's free, stable, and runs happily without a screen. There's no
desktop to click on, so you control it by typing commands (see
[Command-line basics](command-line-basics.md)).

## The problem Docker solves

Otomo is about twenty programs, written with different tools, each needing its own
settings. Installing all of them directly on the VM would be fragile: one program's update
could break another's, and setting up a second machine would mean repeating every step by
hand.

**Docker** solves this by packaging each program in a **container**.

### Image: the packed program

An **image** is a program together with everything it needs to run (its libraries,
settings files, and a minimal operating system), frozen into one file. It's like a game's
exported build: a single thing you can copy anywhere and run, without installing anything
else first.

Images are built from a recipe called a **Dockerfile**. Here's a slightly simplified
version of the one for your game server (the real one is `deploy/gameserver/Dockerfile`),
with comments:

```dockerfile
# Start from a small Linux image that has what .NET programs need.
FROM mcr.microsoft.com/dotnet/runtime-deps:9.0

# Copy your exported Godot server into the image.
COPY build/ /opt/gameserver/

# Run as an unprivileged user, not as the all-powerful "root".
USER 65532:65532

# The command to run when a container starts from this image.
ENTRYPOINT ["/opt/gameserver/gameserver.x86_64", "--headless", "--"]
```

### Container: a running copy

A **container** is a running instance of an image, the way a running game is an instance
of its exported build. You can run many containers from one image: otomo's `gs-1` and `gs-2`
are two containers from the same game server image.

Each container is sealed off from the others. It has its own files, sees only its own
programs, and can reach only the networks it's been given. If it crashes, nothing else is
affected, and Docker can start it again.

### Registry: where images are stored

A **registry** is a storage place for images, each with a **tag** that names its version,
for example `otomo-gameserver:local` or `otomo-auth:a988f9e`. Keeping old tags around
makes going back easy: to undo a bad update, you start the previous tag again.

## Compose: the whole stack in one file

Starting twenty containers one by one, each with the right settings, would be tedious.
**Docker Compose** does it from one file, `deploy/compose.yaml`, which lists every service:
which image to use, which settings (environment variables) to give it, which files it can
see, which networks it's on, and whether any port is open to the outside.

Here's the entry for one game server, shortened, with comments:

```yaml
gs-1:
  image: 127.0.0.1:5000/otomo-gameserver:local   # which image to run
  restart: always                                 # if it stops, start it again
  environment:                                    # settings passed to the program
    OTOMO_GS_SERVER_ID: gs-1                      #   its name
    OTOMO_GS_PORT: "7777"                         #   the UDP port it listens on
    OTOMO_ALLOCATOR_URL: http://allocator:8080    #   where the Allocator is
  networks: [otomo-net]                           # the private network it joins
```

Notice `http://allocator:8080`. Inside otomo's private network, each service can reach the
others **by name**: `allocator` means the Allocator's container. That network isn't
reachable from the internet, which is why the Allocator is safe from outsiders.

Notice also what's *missing*: there's no `ports:` line, so nothing from the internet can
reach `gs-1` directly. Only the Gameplay Proxy, which is on the same private network, can.

`restart: always` is what makes the game server lifecycle work (see
[Parties and matches](parties-and-matches.md)): when a game server quits after a match,
Docker immediately starts a fresh one.

## Commands you'll meet

You run these on the VM, inside the `~/otomo/deploy` folder. Each is explained where a
guide uses it; this is just a preview.

| Command | What it does |
|---|---|
| `docker compose ps` | Lists otomo's containers and whether each is running |
| `docker compose logs gs-1` | Shows what the `gs-1` container has printed |
| `docker compose build gs-1` | Builds the image that `gs-1` uses, from its Dockerfile |
| `docker compose up -d gs-1 gs-2` | Starts (or restarts, if changed) those containers. `-d` means "in the background" |
| `docker compose stop gs-1` | Stops a container |

## Next

Continue with [Command-line basics](command-line-basics.md), which explains how to type
these commands in the first place.
