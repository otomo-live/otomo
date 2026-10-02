# Public edge: runbook

The edge container (`services/edge`) terminates TLS on 443 for the player gateway and the
wiki sites. This directory is the host side: the Let's Encrypt certificate.

## Read this first: the certificate quota

Let's Encrypt issues **at most 50 new certificates per week per registered domain**. If
your host name sits under a domain you share (a school's, a company's, a hosting
provider's subdomain), everyone on it draws from the same 50. So:

- **Rehearse with `dry-run` every time.** It uses the staging CA and costs no quota.
- **Request the real certificate exactly once** per host name. `certbot.timer` renews it
  (renewals don't count against the new-certificate limit), and the deploy hook reloads the
  edge. Nobody runs `issue` again.
- **The edge container never requests certificates itself**, so a restart, a rebuild or a
  crash loop can't spend quota. Only `certbot.sh issue` and certbot's renewal timer talk
  to Let's Encrypt.

`certbot.sh issue` enforces this. It refuses when a certificate for the name already exists
on the host, when there's no successful dry-run from the last 2 hours, or when crt.sh shows
a certificate issued for the name in the last 7 days. It also requires
`--confirm-single-issuance`.

## Cookies

Assume the parent domain may be shared with sites you don't control. **No otomo cookie ever sets `Domain`**, login-session cookies use the `__Host-`
prefix (design/06 §13.2), and the edge strips `Cookie` and `Set-Cookie` on every public route.
HSTS is sent for this host only, never with `includeSubDomains` or `preload`.

## First-time setup (once per host)

```sh
# 1. Firewall: the edge publishes 80 and 443. Docker publishes past ufw, so these
#    rules record intent rather than enforce it; keep them in step with compose.yaml.
sudo ufw allow 80/tcp comment 'otomo edge: ACME + redirect'
sudo ufw allow 443/tcp comment 'otomo edge: TLS'

# 2. certbot, the ACME webroot and the deploy hook.
sudo deploy/edge/certbot.sh install

# 3. In deploy/.env:
#      OTOMO_PUBLIC_HOST=play.example.com
#      COMPOSE_PROFILES=edge
#    then start the edge. With no certificate yet it runs in bootstrap mode
#    (ACME on :80, 503 elsewhere).
deploy/scripts/up.sh

# 4. Rehearse. This checks the challenge path from the internet first.
sudo OTOMO_PUBLIC_HOST=play.example.com deploy/edge/certbot.sh dry-run

# 5. Only after step 4 succeeds, and only ever once:
sudo OTOMO_PUBLIC_HOST=play.example.com deploy/edge/certbot.sh issue --confirm-single-issuance
#    The deploy hook switches the edge to TLS mode without a restart.

# 6. Point clients at it: in deploy/.env set
#      OTOMO_PUBLIC_BASE_URL=https://play.example.com
#    and re-run deploy/scripts/up.sh (Auth's login response carries the URL).
```

## Day to day

- `sudo deploy/edge/certbot.sh status` shows the certificate, the timer, and a renewal
  dry-run.
- Renewal is automatic (`certbot.timer`, twice a day; certbot renews within 30 days of
  expiry). The hook logs to syslog under `otomo-edge`: `journalctl -t otomo-edge`.
- After changing the edge's environment: `docker compose -f deploy/compose.yaml up -d edge`.
  To reload the certificate by hand: `docker compose -f deploy/compose.yaml exec edge edge-reload`.
