# Use the admin website

The admin website is where your team publishes content, watches otomo's health, and
manages staff accounts. This guide gets you signed in for the first time and tours each
page. It takes about 20 minutes, most of it one-time setup.

**You'll need:**

- a login on your VM (your SSH key added to it), and WSL on
  Windows. See [Before you begin](../start/before-you-begin.md);
- an invite link from an administrator, or the root password if you're setting otomo up.

## Why it needs a tunnel

The admin website can publish content to every player, so it's deliberately **not on the
internet**. It only listens on the server's own address, `127.0.0.1:8090`. To reach it,
your computer makes an **SSH tunnel**: an encrypted pipe from port 8090 on your computer to
port 8090 on the server. [Command-line basics](../concepts/command-line-basics.md) explains
tunnels in detail.

## Step 1: Open the tunnel

1. Open **WSL** (Start menu → **Ubuntu**).
2. Run:

    ```sh
    ssh -N -L 8090:127.0.0.1:8090 you@play.example.com
    ```

3. The first time, SSH asks `Are you sure you want to continue connecting (yes/no)?`.
   Type `yes` and press <kbd>Enter</kbd>.

The command then seems to do nothing, with no new prompt. **That's correct**: the tunnel
is open while this command runs. Leave the window open for as long as you use the admin
website.

!!! warning "Type `127.0.0.1` in the command, not `localhost`"
    On the server, `localhost` means the IPv6 address `::1`, where the admin website
    doesn't listen. The tunnel would open, but every page would fail with
    `channel 3: open failed: connect failed: Connection refused` in this window.

## Step 2: Open the website

In your browser, go to:

```text
http://localhost:8090/admin/
```

Use exactly `localhost` here, not `127.0.0.1`. The sign-in cookie is set for `localhost`,
and with the other address the browser won't send it back, so you'd be signed out on every
page.

You should see the **Sign in** page.

## Step 3: Your account

=== "I have an invite link"

    1. Keep the tunnel open and open the invite link in the same browser. It starts with
       `http://localhost:8090/`.
    2. On **Set up your account**, choose a password.
    3. If your team requires two-factor sign-in, the next page shows a QR code. Scan it
       with an authenticator app on your phone (Google Authenticator, Microsoft
       Authenticator, 1Password and others all work), then type the 6-digit code the app
       shows.
    4. You're signed in.

    Invite links work once and expire. If yours doesn't work, ask for a new one.

=== "I'm setting otomo up (root)"

    Otomo starts with one emergency account, `root@otomo.internal`. Its password is on
    the server:

    ```sh
    ssh you@play.example.com 'sudo cat ~/otomo/deploy/secrets/admin_auth/root_password'
    ```

    Sign in with it, then **immediately create a personal account for each person**
    (Step 5) and use those. Keep root for emergencies.

## Step 4: A tour of the pages

The menu on the left groups the pages. Which ones you see depends on your **role**
(below).

**Operate**

| Page | What it's for |
|---|---|
| **Overview** | Health at a glance: which services are up, players online, free game servers, and any **firing alerts** (things that need attention now, such as "no free game servers"). |
| **Logs** | Search every service's log messages. Paste a `request_id` from an error to find exactly what happened to that request. |
| **Audit** | Who changed what, and when: every publish, rollback, account change and staff action. |

**Content**

| Page | What it's for |
|---|---|
| **Namespaces** | The settings documents: create, edit drafts, create versions. |
| **Packs** | Upload `.pck` content packs. |
| **Releases** | What each channel serves; compose, roll back and promote releases. |

**Admin**

| Page | What it's for |
|---|---|
| **Users** | Invite staff, change roles, send password-reset links. |

The words on the Content pages (namespace, version, pack, release, channel) are explained
in [Content and patching](../concepts/content-and-patching.md).

## Step 5: Staff roles and invites

Every staff account has one **role**. Each role can do everything the one above it can:

| Role | Can |
|---|---|
| **viewer** | Look at everything, change nothing. |
| **live_ops** | Also edit namespace drafts, create versions, upload packs, and publish releases to `dev` and `staging`. |
| **admin** | Also create namespaces and edit their schemas, publish to **`live`** (what players get), roll back or promote any channel, and manage staff accounts. |

To invite someone (admins only):

1. **Users → Invite user**.
2. Fill in their **Email**, **Name** and **Role**, then press **Send invite**.
3. Copy the **invite link** shown and send it to them privately (a direct message, not a
   public channel: whoever opens it first gets the account). Otomo doesn't send email
   itself, and the link is shown only once, so copy it before closing the dialog.

The person opens the link with their own tunnel open, as in Step 3.

## Closing up

When you're done, sign out (top right), then stop the tunnel: in the WSL window, press
<kbd>Ctrl</kbd>+<kbd>C</kbd>.

## Next

[Publish your first content update](first-patch.md) walks through the Content pages
end to end.
