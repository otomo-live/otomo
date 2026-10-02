# Admin access

How to reach the staff plane (`/admin/` and `/admin-auth/*`) and sign in. Read
this with `README.md`'s "Root account" section; the tunnel it uses is
`deploy/scripts/tunnel.sh`.

The staff plane is served by `gateway_dev`, which `deploy/compose.yaml`
publishes on the host's **loopback interface only**
(`127.0.0.1:8090`). You cannot open `http://<host>:8090/` from your own machine.
Instead you forward that port over SSH and work at `http://localhost:8090/`.
This is deliberate (decision 2 in `compose.yaml`): exposing `gateway_dev` would
also expose the unauthenticated `/admin-auth/*` and `/admin/` prefixes.

## 1. Prerequisites

1. **SSH access to the host.** If you do not have it, ask whoever runs the
   deployment to add your SSH public key; nothing in this guide works without
   it.
2. **`ssh` on your machine**, and `nc` (or `ss`/`netstat`) for the port check.
3. **Chrome or Firefox.** **Safari is unsupported for the tunnel.** The refresh
   cookie is `Secure; SameSite=Strict; Path=/admin-auth/`
   (`design/06-auth-identity-contract.md` §13). Safari refuses to set a `Secure`
   cookie on `http://localhost`, so sign-in appears to succeed and then drops
   you straight back to the login page. Chrome and Firefox treat `localhost` as
   a secure context and accept it.
4. **An out-of-band channel** for sending invite links: a direct message, your
   password manager's sharing feature, or similar. Never a public channel.

## 2. Start the tunnel

1. From a checkout of this repository, with your SSH login on the host:

   ```sh
   OTOMO_SSH=you@play.example.com deploy/scripts/tunnel.sh
   ```

2. It prints `open http://localhost:8090/admin/ — Ctrl+C to close` and stays in
   the foreground. Keep that terminal open for as long as you are working.
3. If `8090` is already taken on your machine, choose another local port:

   ```sh
   OTOMO_SSH=you@play.example.com LOCAL_PORT=8091 deploy/scripts/tunnel.sh
   ```

   Then use `http://localhost:8091/` everywhere below. **Invite and reset links
   always say `localhost:8090`** (admin-auth builds them from
   `ADMIN_AUTH_PUBLIC_URL`): with another local port, change the port in the link
   before opening it. The part after `#token=` is what matters.
`deploy/scripts/tunnel.sh --help` shows both variables.

## 3. First sign-in ever (root)

Do this once, with the operator who has SSH to the host. Root is the
break-glass account; it is the only account that can grant the `admin` role.

1. On the host, over SSH, read the root password:

   ```sh
   sudo cat ~/otomo/deploy/secrets/admin_auth/root_password
   ```

   The file is `0600` and owned by the admin-auth container's uid (`65532`),
   which is why `sudo` is needed. Adjust the path to wherever the checkout is.
   The value never leaves the host except into the browser over the tunnel.
2. With the tunnel running, open <http://localhost:8090/admin/> and sign in as
   `root@otomo.internal` with that password.
3. Root has **no MFA**, by design (decision D5). Its only job is creating
   personal admin accounts (decision D3: only root may grant `admin`). Do not
   use it day to day and do not share its password.

## 4. Inviting someone

An invite creates the account at acceptance time; it is one-time and expires in
72 hours.

1. Sign in as root (or as an `admin`) and open the **Users** page.
2. Choose **Invite**, enter the person's email and name, and pick a role:
   `viewer`, `live_ops`, or `admin`. Only root can grant `admin`.
3. Copy the one-time link. It is shown **once**; if you lose it, revoke the
   invite and create a new one.
4. Send the link out-of-band. It looks like
   `http://localhost:8090/admin/onboard#token=…` and only works through a
   running tunnel. The token is in the URL fragment, so it is never sent to the
   server in a request path or logged by the gateway.

> Not built yet: see §0. Until the Users page lands, create and list invites
> with the API calls in "Doing it from the API".

## 5. Accepting an invite

1. Start the tunnel (§2) — the link points at `localhost`, so it does nothing
   without it.
2. Open the invite link in Chrome or Firefox.
3. Set a password. The policy is at least 12 characters, not a common password,
   and not your email address or its local part.
4. You are signed in immediately after redeeming the link.
5. An `admin` account is required to enroll TOTP (authenticator app: scan the QR
   code or type the secret), and you save the 10 recovery codes that are shown.

> Not built yet: see §0. The SPA has no `/admin/onboard` route yet, and TOTP
> enrollment/recovery codes have no endpoint, so as of this build a redeemed
> invite signs the person in with a password only.

## 6. Everyday sign-in

1. Start the tunnel and open <http://localhost:8090/admin/>.
2. Enter your email and password. If you have a confirmed authenticator, enter
   the current 6-digit code when asked.
3. **Recovery code:** if you have lost the authenticator, choose the recovery
   option at the MFA prompt and enter one of your saved codes. Each code works
   once.
4. **Password reset:** ask an admin to issue one from the Users page (**Reset**).
   A reset link has the same shape, the same 72-hour expiry, and the same
   tunnel requirement as an invite; opening it lets you set a new password and
   signs you in.

## 7. Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `tunnel: localhost:8090 is already in use.` | Another process (often a previous tunnel) holds the port. Close it, or run with `LOCAL_PORT=8091`. |
| Browser shows "connection refused" or cannot reach `localhost:8090`. | The tunnel is not running, or `gateway_dev` is down on the host. Check the host with `docker compose ps` and make sure `gateway_dev` is up. |
| Signed out immediately after signing in, or the session does not survive a reload. | You are using Safari, or you opened `http://<host>:8090` instead of `http://localhost:8090`. Use Chrome/Firefox and `localhost` (see §1). |
| Invite or reset link says the link is invalid/expired. | The 72-hour window passed, it was already used, or it was revoked. Ask an admin to issue a new one. |
| Lost the authenticator, and no recovery code left. | Use a recovery code if you still have one. There is **no** self-service or admin MFA-reset endpoint in this build, so with no code the account cannot recover its second factor through the UI. This is a known gap (§0). |
| `Permission denied (publickey)` from the tunnel script. | Your SSH key is not installed for the target account, or `OTOMO_SSH` names the wrong user/host. Ask the host operator. |

## 8. Rotating the root password

The root password is read from a file; rotation writes a new value and updates
the stored hash in one step. Run it on the host, as root:

```sh
sudo ~/otomo/deploy/scripts/rotate-root-password.sh
```

It keeps the old value in `root_password.previous` while
`admin-auth bootstrap-root -rotate` runs, and restores it if that fails.
`-rotate` also revokes every live root session, so anyone signed in as root is
signed out. The new password is read the same way as before (§3).

## Doing it from the API (until the UI exists)

With the tunnel running and a bearer token from signing in, the user-management
API is reachable at `http://localhost:8090/api/admin/users*`. For example, to
invite an admin (root only) and get the one-time link:

```sh
curl -sS -X POST http://localhost:8090/api/admin/users \
  -H "Authorization: Bearer $STAFF_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"email":"new.admin@example.com","name":"New Admin","role":"admin"}'
```

The response carries `invite_url`, which is the `…/admin/onboard#token=…` link
to send. To redeem an invite or reset without the SPA, POST the token and new
password to the public endpoint:

```sh
curl -sS -X POST http://localhost:8090/admin-auth/onboard \
  -H 'Content-Type: application/json' \
  -d '{"token":"<token from the fragment>","password":"<your new password>"}'
```
