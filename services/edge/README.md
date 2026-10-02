# edge

The public edge: nginx terminating TLS on 443 for the player gateway and
the wiki sites, and answering Let's Encrypt HTTP-01 challenges on 80. It is the only
container that publishes 80/443.

| Path | Goes to |
|---|---|
| `/auth/`, `/patch/`, `/api/player/` | the player gateway (`EDGE_GATEWAY_URL`) |
| `/docs/` | the technical wiki (`EDGE_DOCS_URL`), which serves itself under `/docs/` |
| everything else | the optional site (`EDGE_SITE_URL`); without one, `/` redirects to `/docs/` |
| `http://…/.well-known/acme-challenge/` | the ACME webroot certbot writes into |
| any other `http://` | 301 to `https://` |

**Certificates are never requested by this container.** The host's certbot issues and
renews them (`deploy/edge/`), and its deploy hook runs `edge-reload` inside the container.
Until a certificate exists, the edge runs in **bootstrap mode**: port 80 answers ACME
challenges and every other request gets 503. Player traffic is never served over plain
HTTP.

**No cookies cross the edge**, in either direction. `Cookie` is removed from every
proxied request and `Set-Cookie` from every response. HSTS covers this host only;
there's no `includeSubDomains` or `preload`, because the parent domain is shared.

| Env | Default | |
|---|---|---|
| `EDGE_SERVER_NAME` | required | the certificate name, e.g. `play.example.com` |
| `EDGE_GATEWAY_URL` | `http://gateway:8080` | player gateway |
| `EDGE_DOCS_URL` | `http://wiki-docs:8080` | technical wiki |
| `EDGE_SITE_URL` | empty | optional site served at `/` (e.g. a game's wiki) |

Mounts: `/etc/letsencrypt` (read-only) and the ACME webroot at `/var/www/acme` (read-only).
