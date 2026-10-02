# Deploy otomo on a VM

This guide takes you from an empty Linux virtual machine to a complete otomo for your
game: every service, the admin website, a public HTTPS address for players, and a pool of
game servers. Everything after this page assumes you've done it.

Plan for **two to three hours**, most of it waiting and checking. The guide is entirely in
the terminal; if that's new to you, read
[Command-line basics](../concepts/command-line-basics.md) and
[Servers and containers](../concepts/servers-and-containers.md) first.

Throughout this guide, `play.example.com` stands for **your** domain name and
`203.0.113.10` for your VM's IP address. Replace them with your own every time.

## What's yours and what's otomo's

Before starting, it helps to know where the line is:

| Part | Who provides it |
|---|---|
| Auth, Gateway, Patch, Session, Allocator, Gameplay Proxy, Dashboard, admin website, edge, wiki | **Otomo.** You run and configure them; you don't change their code for your game. |
| Settings documents and their schemas, content packs | **Your game.** You create them in the admin website ([Content and patching](../concepts/content-and-patching.md)). |
| Lobby rules (party size, allowed lobby settings) | **Your game**, as the server namespace `session.rules`. |
| The game server | **Your game**: your Godot server export ([Run a new game server build](../guide/game-servers.md)). |
| The game client | **Your game**, with the SDK ([Install the SDK](../guide/install-the-sdk.md)). |
| The public website at `/` | **Your game**, from its own repository ([Wikis and sites](../guide/wiki.md)). |

## What you need

- **A Linux VM** from any cloud provider, running **Ubuntu 24.04**, with at least 2 CPU
  cores, 4 GB of memory and 40 GB of disk. You need to be able to log in to it with SSH
  and use `sudo`.
- **A domain name** you control, such as `play.example.com`, with an **A record** pointing
  at the VM's IP address. Your domain provider's DNS settings page is where you add it.
  It's needed for the HTTPS certificate. Check it from your PC: `nslookup play.example.com`
  should print the VM's IP.
- **Ports open in your cloud provider's firewall** (often called a *security group*):
  TCP 22 (SSH), TCP 80 and 443 (HTTPS), and UDP 27000 (the Gameplay Proxy).
- **On your PC:** an SSH client. On Windows, use WSL
  ([Before you begin](before-you-begin.md)).

Nothing else is needed on the VM besides Docker: no Go, no Node, no database. Everything
runs in containers.

## Step 1: Prepare the VM

Log in to the VM from your PC:

```sh
ssh you@203.0.113.10
```

Every command from here to the end of Step 7 runs **on the VM**.

### Install Docker, git and openssl

These are the commands from Docker's own "Install Docker Engine on Ubuntu" page. Run them
one block at a time:

```sh
sudo apt-get update
sudo apt-get install -y ca-certificates curl git openssl
sudo install -m 0755 -d /etc/apt/keyrings
sudo curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
sudo chmod a+r /etc/apt/keyrings/docker.asc
```

```sh
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] \
https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
  | sudo tee /etc/apt/sources.list.d/docker.list > /dev/null
sudo apt-get update
sudo apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
```

Let your user run Docker without `sudo`, then log out and back in so it takes effect:

```sh
sudo usermod -aG docker "$USER"
exit
```

```sh
ssh you@203.0.113.10
docker compose version
```

The last command should print a version number such as `Docker Compose version v2.29.1`.

### Turn on the firewall

`ufw` is Ubuntu's firewall. Allow SSH **before** enabling it, or you'll lock yourself out:

```sh
sudo ufw allow OpenSSH
sudo ufw enable
```

!!! warning "Docker-published ports bypass ufw"
    Docker writes its own firewall rules, so a port a container publishes is reachable even
    without a `ufw allow` rule. Otomo therefore publishes only the ports it needs to (80,
    443 and the Gameplay Proxy's UDP port) and binds everything else to the VM's own
    address. The `ufw` rules below record your intent; your cloud provider's firewall is
    the other layer.

## Step 2: Get otomo onto the VM

```sh
git clone https://github.com/otomo-live/otomo.git ~/otomo
cd ~/otomo
git checkout main
```

- `git clone` downloads the repository into a new folder, `~/otomo` (`~` is your home
  folder).
- `git checkout main` makes sure you're on `main`, the branch with the latest **release**.
  `staging` holds the work going into the next release, and can change at any time.

## Step 3: Generate secrets

Otomo needs passwords for its databases, and keys that let its services recognise each
other. A script makes them all:

```sh
deploy/scripts/generate-secrets.sh
```

It creates:

- `deploy/.env`, a text file of settings and passwords, one `NAME=value` per line;
- `deploy/secrets/`, a folder of key files;
- `deploy/valkey/valkey.conf`, the settings for Valkey (see
  [How otomo fits together](../concepts/how-otomo-fits-together.md)).

It's safe to run again: it never overwrites a secret that already exists.

### Give the key folders to the right owner

Otomo's containers run as a restricted user with the number **65532**, not as you, so the
folders they write keys into must belong to that user. The script prints the exact
commands to run at the end. They look like this:

```sh
sudo chown -R 65532:65532 /home/you/otomo/deploy/secrets/admin_auth
sudo chown -R 65532:65532 /home/you/otomo/deploy/secrets/service_keys /home/you/otomo/deploy/secrets/allocator
```

Copy the lines the script printed, not these: yours contain your own folder path.

`chown` changes who owns a file; `-R` applies it to everything inside the folder too.

## Step 4: Start everything

```sh
deploy/scripts/up.sh
```

This is the one command that brings otomo up. It takes 10 to 20 minutes the first time,
most of it building images. In order, it:

1. **builds** every service's image;
2. **starts** the database (Postgres), Valkey and the local image registry, and waits for
   them;
3. **prepares the databases** (runs each service's *migrations*, which create the tables
   it needs);
4. **generates signing keys** for logins, once, and creates the emergency admin account;
5. **starts every service**;
6. **checks** each one answers, printing `ok` or `FAILED` per service.

The end of a successful run looks like this:

```text
waiting for services
  ok      auth liveness (/healthz)
  ok      auth readiness (/readyz)
  …
  ok      gameplay-proxy readiness (/readyz)
```

!!! tip "If one line says FAILED"
    Services depend on each other and some take a moment to start. Run `up.sh` again: it's
    safe to repeat, and it skips work that's already done. If the same service fails
    twice, look at its log: `cd deploy && docker compose logs --tail 50 <service>`.

At this point otomo is running, but only reachable from the VM itself. The next steps
make it reachable by players.

## Step 5: A public HTTPS address

Players' games must reach otomo over **HTTPS**, which needs a **certificate** proving the
server owns its domain name ([How games go online](../concepts/how-games-go-online.md)).
Otomo gets a free one from **Let's Encrypt**, an organisation that issues certificates
automatically.

!!! danger "Let's Encrypt limits how many certificates you can get"
    Let's Encrypt allows only a small number of new certificates per domain per week. If
    your host name sits under a domain you share with others (a company's or a school's),
    everyone on it shares that limit. Otomo's script therefore makes you **rehearse**
    first, which costs nothing, and refuses to request a real certificate twice. Follow the
    steps in order and don't skip the rehearsal.

In `deploy/`, open `.env` in a text editor (`nano deploy/.env`) and set:

```ini
OTOMO_PUBLIC_HOST=play.example.com
COMPOSE_PROFILES=edge
```

`OTOMO_PUBLIC_HOST` is your domain name. `COMPOSE_PROFILES=edge` switches on the **edge**,
the HTTPS front door, which is off by default. Save and exit (<kbd>Ctrl</kbd>+<kbd>X</kbd>,
<kbd>Y</kbd>, <kbd>Enter</kbd>), then run, from the `otomo` folder, one at a time:

```sh
# 1. Open ports 80 and 443 in the VM's firewall.
sudo ufw allow 80/tcp comment 'otomo edge'
sudo ufw allow 443/tcp comment 'otomo edge'

# 2. Install certbot, the program that talks to Let's Encrypt.
sudo deploy/edge/certbot.sh install

# 3. Start the edge (it waits for a certificate).
deploy/scripts/up.sh

# 4. Rehearse. This checks that Let's Encrypt can reach your VM, and costs nothing.
sudo OTOMO_PUBLIC_HOST=play.example.com deploy/edge/certbot.sh dry-run

# 5. ONLY if step 4 succeeded: request the real certificate, once.
sudo OTOMO_PUBLIC_HOST=play.example.com deploy/edge/certbot.sh issue --confirm-single-issuance
```

Then set `OTOMO_PUBLIC_BASE_URL=https://play.example.com` in `deploy/.env`, and run
`deploy/scripts/up.sh` one more time.

Check it from your own PC:

```sh
curl -i https://play.example.com/patch/v1/live/manifest
```

`HTTP/1.1 200 OK` with some JSON means players can reach otomo. The certificate renews
itself from now on; you never run `issue` again.

## Step 6: The Gameplay Proxy port

Open the proxy's UDP port in the firewall:

```sh
sudo ufw allow 27000/udp comment 'otomo gameplay proxy'
```

If another program on the VM already uses UDP 27000 (you'd see `address already in use`
when the proxy starts), pick another free port, open that instead (in `ufw` **and** your
cloud provider's firewall), and set it in `deploy/.env`:

```ini
GAMEPLAY_PROXY_PORT=27015
```

Otomo tells games the port automatically, so nothing changes on the game's side.

## Step 7: Game servers

Follow [Run a new game server build](../guide/game-servers.md) to put your game server in
`deploy/gameserver/build/`. Then add `gameservers` to the profiles in `deploy/.env`:

```ini
COMPOSE_PROFILES=edge,gameservers
```

and run `deploy/scripts/up.sh`.

You can skip this step for now and come back to it: patching and logins work without game
servers.

## Step 8: Sign in to the admin website

The admin website isn't on the internet (see
[Use the admin website](../guide/admin-website.md)). Read the root password on the VM:

```sh
sudo cat ~/otomo/deploy/secrets/admin_auth/root_password
```

Then, **from your PC**, open a tunnel and sign in as `root@otomo.internal` at
`http://localhost:8090/admin/`:

```sh
ssh -N -L 8090:127.0.0.1:8090 you@play.example.com
```

Create a personal account for each person on your team under **Users**, and keep root for
emergencies.

## Step 9: Point your game at it

In your game, set **Project Settings › Otomo › Config › Base Url** to
`https://play.example.com`. Run the SDK's test scene
([Install the SDK](../guide/install-the-sdk.md), Step 5) to check.

## Keeping it running

### Updating otomo

New otomo releases land on `main`. Read the release notes on GitHub first, then on the VM:

```sh
cd ~/otomo
git pull
deploy/scripts/up.sh
```

`up.sh` rebuilds only what changed and restarts only the services whose images changed.

### Checking health

- The admin website's **Overview** page shows each service's state and any firing alerts.
- On the VM, `cd ~/otomo/deploy && docker compose ps` lists every container and whether
  it's running.

### Where the important files are

| Path (under `~/otomo/deploy/`) | What | Back it up? |
|---|---|---|
| `.env` | Settings and passwords | **Yes**, privately |
| `secrets/` | Signing and service keys | **Yes**, privately. Losing the login signing key logs every player out. |
| Docker volumes (`docker volume ls`) | The databases and uploaded content | **Yes**: this is all your game's data |

Never commit `.env` or `secrets/` to git: they're secrets, and otomo's `.gitignore` keeps
them out.

## Next

- [Install the SDK](../guide/install-the-sdk.md) in your game.
- [Use the admin website](../guide/admin-website.md).
- [Troubleshooting](../troubleshooting.md).
