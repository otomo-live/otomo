# AUTH-0 — Identity Contract (Player + Staff)

**Status:** Proposed defaults. Everything marked *(ratified)* is settled and safe to build against now. Everything marked *(needs Auth/admin-auth owner confirmation)* should be confirmed before those services diverge from it — but nothing here blocks Gateway from being built today.
**Consumed by:** `05-gateway-techspec.md` (resolves its §6.3 and §7), `04-session-minimal.md` (SES-B1's `player_id` = `sub`), `02-config.md` and `01-dashboard.md` (`roles` checks).

This document exists because Gateway (and Session, and Config, and Dashboard) were being specified against guesses about what a token looks like. This freezes the guess into a contract. Auth and admin-auth's *implementations* can now proceed independently and in parallel with Gateway, as long as both produce exactly this.

---

## 1. Two separate things Gateway does with Auth — don't conflate them

This is the actual answer to "how does Gateway talk to Auth," and it's simpler than it looks once split in two:

| | Pass-through proxy | Token verification |
|---|---|---|
| What it is | `/auth/*` on `gateway` (the player edge) forwards the raw request to Auth unmodified (GATE-2) | Gateway independently fetches Auth's public keys and checks a token's signature itself (GATE-3) |
| When it happens | On every request under `/auth/*` — login, refresh, whatever Auth exposes | Once at startup, then in the background on a refresh interval — **never per incoming request** |
| What Gateway needs to know about Auth's API | Nothing beyond the base URL. It's a wildcard prefix (`Pattern: "/auth/"`); Auth can add or change endpoints under it without Gateway's route table changing | Everything in this document |
| Network path | Whatever traffic the client sends, proxied straight through | A single outbound `GET` to `/.well-known/jwks.json`, east-west, not through either Gateway instance |

**The reason this matters:** your Network Engineer does not need Auth's finished API surface to build Gateway. GATE-2's routing work is already fully unblocked — it's a prefix forward. What was actually blocking GATE-3/GATE-4 is only the second column, and that's what the rest of this document pins down.

**Also explicit, because it's easy to assume otherwise:** Gateway never calls Auth synchronously to ask "is this token still valid." Verification is entirely local, against cached public keys. This is the whole point of JWT + JWKS — it's what makes Gateway fast and lets it keep working if Auth is briefly down, as long as the cached keys haven't gone stale. The trade-off is in §6.

---

## 2. Signing algorithm: EdDSA (Ed25519), both domains *(ratified)*

Both Auth and admin-auth sign with Ed25519. One algorithm, one code path in Gateway, one `Claims` struct shape, one test-fixture generator for both domains.

- **Auth** already commits to this via AUTH-2.
- **admin-auth** signs EdDSA too. The staff service was originally planned in PHP, where `firebase/php-jwt` supports EdDSA signing directly (alongside RSA/ECDSA); the implemented Go service uses `crypto/ed25519`. Either way it removes a class of bug where Gateway's validator has to special-case one domain's algorithm differently from the other. This was an unstated assumption in `05-gateway-techspec.md` §6.3 (it hardcoded `WithValidMethods(["EdDSA"])` for both groups without flagging that the staff algorithm choice was still open) — it's now a deliberate decision, not an accident two services made independently.

**Requirement on the key format:** both JWKS endpoints publish keys as JWK `OKP` type per RFC 8037 — `"kty": "OKP", "crv": "Ed25519"`. This is what `MicahParks/keyfunc/v3` parses; nothing else is needed on the Gateway side for this to work.

---

## 3. JWKS endpoint contract — identical shape for Auth and admin-auth *(ratified)*

| | |
|---|---|
| Path | `GET /.well-known/jwks.json` |
| Auth required | None. Public key material has no confidentiality requirement. Serve it on the same internal network as everything else for now; nothing more is needed at M1 scale |
| Response | `Content-Type: application/json`, a standard JWK Set (RFC 7517): `{"keys": [ {...}, {...} ]}` |
| Each key | `kty: "OKP"`, `crv: "Ed25519"`, `x: "<base64url public key>"`, `kid: "<opaque string>"`, `use: "sig"`, `alg: "EdDSA"` |
| Multiple keys | The array **must** be able to hold 2+ entries during a rotation window (§6) — this is why Gateway's validator selects a key by `kid` from the JWT header, not by "the one key" |
| Caching | No special headers required. Gateway controls its own refresh cadence (`keyfunc`'s default background refresh, plus automatic re-fetch on an unrecognized `kid`) — it does not take instructions from response headers |

**Example response shape** (placeholder key material — illustrative formatting only, not real bytes):

```json
{
  "keys": [
    {
      "kty": "OKP",
      "crv": "Ed25519",
      "kid": "auth-2026-09-01",
      "x": "MCowBQYDK2VwAyEAGb9ECWmEzf6FQbrBZ9w7lshQhqowtrbLDFw4rXAxZuE",
      "use": "sig",
      "alg": "EdDSA"
    }
  ]
}
```

**Startup ordering, explicit because Docker Compose's `depends_on` does not guarantee this:** `depends_on` waits for a container to *start*, not for its app to be *ready*. Auth's process can be running with its HTTP server not yet listening, or its key not yet loaded. Gateway must not crash-loop if the first JWKS fetch fails — retry with backoff, and let `/readyz` (already specified in `05-gateway-techspec.md` §4) stay `503` until the fetch succeeds. This is already how the spec is written; this note just confirms it's required, not defensive over-engineering.

---

## 4. Claim schema — player domain (Auth) *(ratified shape, values below need confirmation)*

```json
{
  "iss": "https://auth.otomo.internal",
  "aud": ["otomo:player"],
  "sub": "3f1a2b4c-...-uuid",
  "exp": 1789001800,
  "nbf": 1789000900,
  "iat": 1789000900
}
```

| Claim | Required | Notes |
|---|---|---|
| `iss` | yes | Exact string, matches Gateway's `GATEWAY_PLAYER_ISSUER` *(needs confirmation — value above is the proposed default, already used as the example in `05-gateway-techspec.md` §3)* |
| `aud` | yes | Emit as a **single-element array**, not a bare string. `jwt/v5`'s `ClaimStrings` type accepts both forms on parse, but pin the array form on the emitting side so nothing downstream has to special-case it |
| `sub` | yes | The player's stable identifier. See the hard requirement below — this is the one item in this whole document most likely to cause a real bug if got wrong |
| `roles` | **absent** | Player tokens carry no `roles` claim. Role-based authorization is a staff-only concept (per the original Auth/admin-auth domain split) — Gateway's player routes are all configured with `MinRole: 0` in `05-gateway-techspec.md` §5.2, so there's nothing to check |
| `exp`, `nbf`, `iat` | yes | Standard Unix timestamps |

### Hard requirement: `sub` stability

This is a requirement on AUTH-4's design, not an open question to leave dangling — Session's entire data model depends on it (`04-session-minimal.md` §3.1, `player_profile.player_id` is `sub`, primary key):

1. **`sub` must be stable across sessions for the same identity.** An anonymous/device-based player logging in twice from the same device must get the same `sub` both times. If AUTH-4 mints a fresh `sub` per login, Session accumulates a duplicate ghost profile — no friends, no party, a new discriminator — every time that player opens the game.
2. **`sub` must survive account linking.** If an anonymous player later links a persistent account (email, platform ID, whatever AUTH-6/7 turn out to be), the `sub` issued afterward must be the *same* `sub` as before linking, not a new one. If it changes, every friendship and party row in Session keyed to the old `sub` silently orphans, with no error raised anywhere — this is the kind of bug that only shows up as a support ticket ("my friends disappeared") weeks later, not as a test failure.

Whoever builds AUTH-4 needs to see this before writing the anonymous-identity code, not after.

---

## 5. Claim schema — staff domain (admin-auth) *(ratified shape, values below need confirmation)*

```json
{
  "iss": "https://admin-auth.otomo.internal",
  "aud": ["otomo:staff"],
  "sub": "8b2e9f10-...-uuid",
  "roles": ["live_ops"],
  "exp": 1789001500,
  "iat": 1789000600
}
```

| Claim | Required | Notes |
|---|---|---|
| `iss` / `aud` | yes | Same rules as §4, staff values *(needs confirmation, proposed default above)* |
| `sub` | yes | Staff account ID, admin-auth's own table — never a value from Auth's player table |
| `roles` | yes, **array**, at least one element | Resolves `05-gateway-techspec.md` §7's open question #1: array, not a single string. Ordinal hierarchy for `MinRole` checks: `viewer < live_ops < admin`. A token may carry more than one role; the check takes the highest ordinal present |
| `nbf` | optional | Staff sessions are short and interactive; `nbf` adds little here and can be omitted |

---

## 6. Token lifetimes and key rotation *(proposed — tune once real usage patterns exist)*

| | Player (Auth) | Staff (admin-auth) |
|---|---|---|
| Access token TTL | 15 minutes | 15 minutes |
| Refresh token TTL | 30 days, sliding (extends on use) | 7 days, sliding — shorter, since staff access is higher-blast-radius per compromised session |
| Refresh rotation | AUTH-5's concern; opaque server-side, not a JWT | A3/A4 in the originally planned PHP service; now implemented by `services/admin_auth` as rotation with family revocation (§13.2) |

**Revocation is not instant, by design — state this explicitly so nobody is surprised later.** Gateway verifies access tokens locally against cached public keys; it has no way to know a refresh token was just revoked. Revoking a session takes effect the next time that client tries to *refresh*, not the moment revocation happens. A stolen access token remains usable until it naturally expires. This is the standard trade-off for short-lived JWTs, and it's exactly why the TTL above is 15 minutes and not something longer — that's the practical upper bound on "how long can a revoked session still act."

**Key rotation:** keep at least two keys active in the JWKS at all times — the current signing key and the previous one. Since access tokens are short-lived (15 min), the previous key only needs to stay listed for roughly one TTL-plus-clock-skew past the moment it stops signing new tokens, but there's no cost to leaving it longer for operational safety during a manual rotation. Gateway needs no special code for this: `keyfunc` selects by `kid` from whatever's currently in the JWKS array, so multiple simultaneously-valid keys are already handled by `05-gateway-techspec.md` §6.2 as written.

**Clock skew:** `05-gateway-techspec.md`'s `GATEWAY_JWT_CLOCK_SKEW` default of 30s is fine relative to a 15-minute TTL — it's a small fraction, not a meaningful fraction. Revisit only if TTLs shrink dramatically later.

---

## 7. What's still genuinely open

1. **Exact `iss`/`aud` string values** — proposed above, but these are the actual strings that go in the deployed env vars. A typo here fails closed (safe — everything 401s) but wastes debugging time. Confirm once, before Auth's AUTH-3 issuance code is written, not after.
2. **admin-auth's EdDSA adoption** — settled: `services/admin_auth` signs EdDSA, so the staff and player validators share one algorithm (`10-communication-schema.md` §8).
3. **admin-auth's staff HTTP surface** (§13): implemented by `services/admin_auth` in this repository; MFA (`/admin-auth/mfa/verify`) and the audit endpoint remain in review.

AUTH-8 is now specced (see §12) and is **not** on this list — it doesn't touch Gateway's routing or validation code at all. It touches Gateway's *public address*, which is a coordination item, not an open question.

---

## 8. What Auth's HTTP surface probably looks like (illustrative, not a spec)

Gateway doesn't need this list to build GATE-2 — it's a wildcard proxy, per §1. It's included only so the Network Engineer isn't surprised by what's forwarded:

- `POST /auth/anonymous` (or similar — AUTH-4)
- `POST /auth/refresh` (AUTH-5)
- `POST /auth/logout` (AUTH-5)
- `GET /.well-known/jwks.json` (AUTH-2 — **not** proxied through Gateway at all; fetched directly, per §1)

Whatever the real paths turn out to be, they need no changes on Gateway's side as long as they stay under `/auth/`.

---

## 9. Gateway build checklist — resolves `05-gateway-techspec.md` §6.3 and §7

This is what to actually write, now that §4–§6 above are pinned.

### 9.1 Finalized `Claims` struct

```go
package authn

import "github.com/golang-jwt/jwt/v5"

type Claims struct {
    jwt.RegisteredClaims          // iss, sub, aud, exp, nbf, iat — aud parses either array or string form
    Roles []string `json:"roles,omitempty"` // present on staff tokens only; nil/empty on player tokens
}

var roleRank = map[string]int{
    "viewer":   1,
    "live_ops": 2,
    "admin":    3,
}

// HasRoleAtLeast returns true if any role on the token meets or exceeds min.
func (c *Claims) HasRoleAtLeast(min router.Role) bool {
    best := 0
    for _, r := range c.Roles {
        if rank := roleRank[r]; rank > best {
            best = rank
        }
    }
    return best >= int(min)
}
```

This replaces the placeholder referenced in `05-gateway-techspec.md` §6.3 — that section's `writeError`/`ParseWithClaims` code is unchanged, it now has a concrete struct to parse into.

### 9.2 Finalized env values

```
GATEWAY_PLAYER_JWKS_URL=http://auth:8080/.well-known/jwks.json
GATEWAY_PLAYER_ISSUER=https://auth.otomo.internal
GATEWAY_PLAYER_AUDIENCE=otomo:player

GATEWAY_STAFF_JWKS_URL=http://admin-auth:8080/.well-known/jwks.json
GATEWAY_STAFF_ISSUER=https://admin-auth.otomo.internal
GATEWAY_STAFF_AUDIENCE=otomo:staff

GATEWAY_JWT_CLOCK_SKEW=30s
```

Both `WithValidMethods([]string{"EdDSA"})` calls in `05-gateway-techspec.md` §6.3 are now justified for both `Group`s — §2 above is why admin-auth uses the same list, not a separate one.

### 9.3 Build order

1. Wire the env vars above into `internal/config` (`05-gateway-techspec.md` §3) — this can happen before Auth or admin-auth exist, since these are just strings.
2. Implement `internal/authn/jwks.go` and `middleware.go` exactly as specified in `05-gateway-techspec.md` §6.2/§6.3, using the `Claims` struct in §9.1 above.
3. **Do not wait for a running Auth service to test this.** Use the fixture generator in §10 below — it stands up a fake JWKS endpoint and signs real Ed25519 tokens locally, which is everything the middleware needs to be fully tested.
4. Once Auth is actually deployed, run one integration smoke test: point `GATEWAY_PLAYER_JWKS_URL` at the real service, confirm `/readyz` flips to `200`, and confirm one real player token round-trips through a proxied request successfully. That's the entire integration surface — everything else was already covered by the fixture tests.

---

## 10. Test fixture generator (drop into `internal/authn` as a test helper)

Generates an ephemeral Ed25519 keypair, serves it as a JWKS from an `httptest.Server`, and signs tokens against it — so Gateway's validator can be fully unit-tested without Auth or admin-auth running.

```go
package authn_test

import (
    "crypto/ed25519"
    "encoding/base64"
    "encoding/json"
    "net/http"
    "net/http/httptest"
    "testing"
    "time"

    "github.com/golang-jwt/jwt/v5"
)

// newTestJWKS starts an httptest.Server serving a single Ed25519 key
// and returns the server plus a signer for minting test tokens against it.
func newTestJWKS(t *testing.T, kid string) (*httptest.Server, ed25519.PrivateKey) {
    t.Helper()
    pub, priv, err := ed25519.GenerateKey(nil)
    if err != nil {
        t.Fatal(err)
    }
    jwks := map[string]any{
        "keys": []map[string]any{{
            "kty": "OKP",
            "crv": "Ed25519",
            "kid": kid,
            "x":   base64.RawURLEncoding.EncodeToString(pub),
            "use": "sig",
            "alg": "EdDSA",
        }},
    }
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        _ = json.NewEncoder(w).Encode(jwks)
    }))
    return srv, priv
}

// signTestToken mints a token with the given claims, signed with priv,
// and sets kid in the header to match the JWKS entry above.
func signTestToken(t *testing.T, priv ed25519.PrivateKey, kid string, claims Claims) string {
    t.Helper()
    tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
    tok.Header["kid"] = kid
    s, err := tok.SignedString(priv)
    if err != nil {
        t.Fatal(err)
    }
    return s
}

func TestPlayerTokenRejectedOnStaffRoute(t *testing.T) {
    srv, priv := newTestJWKS(t, "test-key-1")
    defer srv.Close()

    playerToken := signTestToken(t, priv, "test-key-1", Claims{
        RegisteredClaims: jwt.RegisteredClaims{
            Issuer:    "https://auth.otomo.internal",
            Audience:  jwt.ClaimStrings{"otomo:player"},
            Subject:   "player-123",
            ExpiresAt: jwt.NewNumericDate(time.Now().Add(15 * time.Minute)),
        },
    })

    // Build the middleware against srv.URL as the staff JWKS source,
    // send a request to a GroupStaff route with playerToken, assert 401.
    // (Wire-up omitted — this is the fixture; the assertion is the
    // acceptance criteria already listed in 05-gateway-techspec.md §6.3.)
    _ = playerToken
}
```

This single pattern covers every acceptance case listed in `05-gateway-techspec.md` §6.3 and §8 — wrong issuer, wrong audience, insufficient role, expired token — by varying the claims passed to `signTestToken` and which `Group`'s middleware the request is sent through.

---

## 11. Summary for the Network Engineer

- You do not need Auth or admin-auth finished, or even running, to build or fully test Gateway. §10 gives you everything.
- Gateway talks to Auth two ways: dumb proxy forwarding (needs nothing but a base URL) and direct JWKS fetch (needs §3–§6 above, all of which are now pinned).
- Gateway never calls Auth per-request to check a token — verification is local, always.
- Copy the `Claims` struct in §9.1 and the env values in §9.2 directly into the Gateway codebase; they're final pending only the two small confirmations in §7.
- AUTH-8 is resolved (§12): the login response carries tokens only, and a client reaches every service at a fixed path under the gateway's base URL.


---

## 12. AUTH-8: public endpoint map (resolved; no hand-off payload)

**Decided (D2, 2026-09-29): Auth does not tell a client where to go next.** The login and
refresh responses carry tokens only:

```json
{"schema_version":1,"access_token":"eyJ…","expires_in":900,"refresh_token":"n3Jq…"}
```

The `services` object Auth first shipped (`services.session`, `services.match`) is
removed, with the `AUTH_PUBLIC_SESSION_URL` setting that fed it. It duplicated what every
client already knows: the client only ever holds the gateway's base URL, and every service
behind the gateway sits at a fixed path under it. A second copy of that address in Auth's
config could only drift from the real one, and a wrong copy would send the player's token to
the wrong place. `schema_version` stays `1`: every client already treated `services` as
optional and fell back to the fixed path, so none misreads a body without it.

### The public base URL

TLS terminates at the public edge (`services/edge`, nginx with a Let's Encrypt certificate
from the host's certbot). The base URL is the deployment's
`OTOMO_PUBLIC_BASE_URL` (`deploy/.env`): for example `https://play.example.com`,
and `http://localhost:8080` (the gateway through an SSH tunnel) without the edge.
The client is configured with it (doc 12 §3.4). Auth does not need it.

### The path convention

| Service | Public path | Status |
|---|---|---|
| Patch | `{base}/patch/v1/…` | Active |
| Auth | `{base}/auth/…` | Active |
| Session | `{base}/api/player/session` | Active: matches the live route in `05-gateway-techspec.md` §5.2 |
| Match | `{base}/api/player/match` | Reserved. No route exists yet, because the Matchmaker is next milestone. Do not add a route for it now |

If a public path ever changes, the change is the gateway's route table and the client SDK's
path constant, in the same release. Auth is not involved.

### Where a game-server address comes from

Never from Auth (AUTH-8.5). A client learns where to play only when its lobby launches:
Session calls the Allocator and sends each member a `party.launching` event carrying the
Gameplay Proxy's public address, port and that member's join ticket
(`14-launch-handoff.md` §3, §7). The Allocator has no gateway route at all
(`05-gateway-techspec.md` §5.2).

### What Gateway does

Nothing new: `/auth/*` on `gateway` (the player edge; `gateway_dev` does not carry player
routes) is a wildcard reverse-proxy route (`05-gateway-techspec.md` §5.2), and the gateway
forwards Auth's body byte for byte.


---

## 13. Staff HTTP surface (`/admin-auth/*`) (implemented by `services/admin_auth`; MFA in review)

§5 pins the staff **token**. This section pins the **calls** that obtain one: §6 deferred the
refresh side to "the original PHP plan (A3/A4)", which was not in this repository, and §8's
endpoint list is player-only by design.

That gap became load-bearing: the admin WebUI (`services/adminui`, WEB-3 and WEB-4) calls these
endpoints. The shapes below are the contract the WebUI is coded against, and `services/admin_auth`
now implements them (login, refresh, logout, me, onboarding and `/api/admin/users*`); MFA is still
in review. The handler-level details are in `10-communication-schema.md` §8. The
WebUI's `src/api/auth.ts` is the only frontend file that changes if a shape shifts.

The paths were not invented. `gateway_dev` routes `/admin-auth/*` to admin-auth as a
public group with no minimum role (`00-common-stack.md` §1, and the route table in
`05-gateway-techspec.md`, which also states that this prefix is legitimately public).

### 13.1 Endpoints

| Call | Request | Success response |
|---|---|---|
| `POST /admin-auth/login` | `{email, password}` | `{access_token, expires_in, user: {id, name, roles[]}}`, **or** `{mfa_required: true, mfa_ticket}` |
| `POST /admin-auth/mfa/verify` | `{mfa_ticket, code}` (a 6-digit TOTP or a recovery code) | the same success body as `login` |
| `POST /admin-auth/mfa/enroll` | `{mfa_ticket}` from a login answering `{mfa_enrollment_required: true, mfa_ticket}`, **or** `Authorization: Bearer` | `{secret, otpauth_url}` |
| `POST /admin-auth/mfa/confirm` | `{mfa_ticket?, code}` (same two auth modes) | `{recovery_codes[10]}`; with a ticket also the `login` success body fields, so enrollment ends signed in |
| `POST /admin-auth/refresh` | no body; the refresh token is a cookie (§13.2) | `{access_token, expires_in, user: {id, name, roles[]}}` |
| `POST /admin-auth/logout` | no body | `204`, and the cookie is cleared |
| `GET /admin-auth/me` | `Authorization: Bearer <access_token>` | `{id, name, roles[], is_root, mfa_enabled}` |

`login` answers `{mfa_enrollment_required: true, mfa_ticket}` for an `admin` account with
no confirmed TOTP factor (D5: TOTP is required for admins, optional otherwise, and root is
exempt). Tickets are single-use, live 5 minutes and allow 5 code attempts. The onboarding
redeem (`POST /admin-auth/onboard`, invite or password-reset link) applies the same
policy after setting the password: it may answer `mfa_required` or
`mfa_enrollment_required` instead of tokens, so a link never bypasses the second factor.

Four deliberate choices in that table:

- **Both `login` and `refresh` return `user`, `roles` included.** The staff token already carries
  `roles` (§5), so the WebUI could decode the JWT and read them from there. It must not. Reading a
  token the client has not verified, to make a display decision, is one refactor away from reading
  it to make an authorization decision, and the hierarchy in §9.1 is the gateway's to enforce. Roles
  arrive as data from the server, and WEB-5 makes the WebUI's use of them UX only regardless.
- **`expires_in` is seconds**, following the OAuth 2 convention. It is advisory: the WebUI reacts to
  a `401`, it does not schedule refreshes from this number.
- **MFA is a second call, not a redirect.** Whatever the final mechanism, `login` has to be able to
  answer "not yet, prove the second factor", and the WebUI has to render that state without a
  session. `mfa_ticket` is opaque to the client, only the service interprets it (in review), and it is what keeps the
  password out of the second request.
- **`GET /admin-auth/me` exists even though `login` and `refresh` both return `user`.** It is the
  only way to answer "is this token still good, and who is it for" after a page reload, when the
  cookie is present but no other call has been made yet. It also carries `is_root` and
  `mfa_enabled` (a confirmed TOTP factor), which the account page needs and `login` does not
  report.

### 13.2 The refresh token rides in an httpOnly cookie

**An httpOnly, `Secure`, `SameSite=Strict` cookie named `__Host-otomo_refresh`, `Path=/`, with
no `Domain` attribute, set by admin-auth and passed through the gateway by its proxy**.

- Everything is same-origin. The WebUI is served at `/admin/` and admin-auth at `/admin-auth/`, both behind
  `gateway_dev`, so there is no CORS credential juggling and the cookie is simply sent.
- An httpOnly cookie is unreadable from JavaScript, so an XSS in the WebUI cannot exfiltrate a
  seven-day refresh token (§6). The access token stays in memory (WEB-4), so neither token is
  reachable from `localStorage` or from a script.
- **The `__Host-` prefix.** A deployment's host may sit under a parent domain shared with other
  sites (a school's or company's). A cookie set with `Domain=<parent>` goes to every sibling site, and a sibling site can
  plant one on ours ("cookie tossing"). Browsers accept a `__Host-` cookie only if it's
  `Secure`, has no `Domain` and has `Path=/`, and only from the exact host, so nobody else can set
  or overwrite it. admin-auth reads only the prefixed name. **No otomo cookie ever sets
  `Domain`.**
- Because the prefix forces `Path=/`, the browser sends the cookie on every staff request.
  `gateway_dev` forwards the `Cookie` header only on the admin-auth routes (`ForwardCookies` in
  the route table) and drops it everywhere else, so Config, Dashboard and Session never see it. The public player gateway drops the `Cookie` header
  entirely; the player plane is bearer-only.
- A page reload silently restores the session. A memory-only refresh token cannot, and would sign a
  developer out on every reload during an editing session, which is the case this app exists for.

CSRF exposure is bounded: `refresh` and `logout` are POST-only, they rotate or clear a token and do
nothing else, and `SameSite=Strict` stops a cross-site form from sending the cookie at all.

It needs no gateway change. `internal/proxy/proxy.go` builds a
`httputil.NewSingleHostReverseProxy` and overrides only `ErrorHandler` and `FlushInterval`;
`Set-Cookie` is not a hop-by-hop header, so it is copied like any other response header, and the
browser attributes the cookie to `gateway_dev`'s origin, which is where the WebUI lives.

**Verify this before building on it, because the design rests on that one hop:** a `curl -i` against
`gateway_dev` calling admin-auth's login and grepping for the `Set-Cookie` line. If the gateway drops it,
the fallback is a memory-only session, which means signing in again after every page reload.

Consequence for CORS: none. `GATEWAY_DEV_CORS_ALLOWED_ORIGINS` needs no change, because local
development proxies through Vite rather than calling the gateway cross-origin. Two documents
disagree about that variable's value (`services/gateway_dev/.env.example` gives
`https://admin.otomo.internal`, `05-gateway-techspec.md` §3 gives `https://admin.otomo.example`);
flagged here rather than silently resolved, since nothing reads it yet.

### 13.3 What the WebUI does with a rejection

The two failure families are different, and a staff client must not treat them alike
(`internal/authn/middleware.go`):

| Response | Codes | Meaning | WebUI behaviour |
|---|---|---|---|
| `401` | `missing_token`, `invalid_signature`, `expired`, `aud_mismatch`, `iss_mismatch`, `invalid_token` | The token itself is unusable | Try `refresh` once; if that fails, clear the session and go to `/login` |
| `403` | `insufficient_role` | The token is fine, the role is not | Do not refresh, it cannot help. Show the refusal |

A `403` that triggers a refresh loop is the specific bug this table exists to prevent.

Refreshing is **single-flight** (WEB-4): any number of concurrent `401`s produce exactly one
`POST /admin-auth/refresh`, and the rest wait on that one promise and then retry their own request
once. The WebUI implements this in `src/api/client.ts` and asserts it with a unit test that fires
five parallel requests into an expiry.

### 13.4 Status

The five routes in §13.1 are implemented by `services/admin_auth`: `login`, `refresh`, `logout`
and `me`, plus `/.well-known/jwks.json`, the public onboarding pair and `/api/admin/users*`;
`10-communication-schema.md` §8 records the handler-level request/response shapes. MFA
(`/admin-auth/mfa/verify`) is still in review. The confirmation that the `Set-Cookie`
hop survives `gateway_dev` remains the one `curl -i` through login, refresh and logout; until it is
recorded, the WebUI's sign-in path is also exercised against the fixture API in
`services/adminui/src/mocks/`.
