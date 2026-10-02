# Technical Specification — Auth

**Tasks covered:** AUTH-1, AUTH-2, AUTH-3, AUTH-4, AUTH-5, AUTH-8
**Not covered:** AUTH-6, AUTH-7 — undefined at time of writing, not guessed at here
**Source of truth for claim shape, JWKS format, token TTLs:** `06-auth-identity-contract.md` — this document implements that contract, it does not redefine it
**Repo location:** `services/auth/`

---

## 1. Scope

Auth owns: player account identity, Ed25519 signing keys and access/refresh token issuance. It tells a client nothing about where to go next (AUTH-8, §7). It does not own routing (Gateway), does not know Gateway's internal route table, and does not maintain any topology map (see §6).

---

## 2. Data model (AUTH-1)

```sql
account (
  account_id   uuid primary key,
  created_at   timestamptz not null default now()
)

identity_binding (
  method       text not null,           -- 'device' in M1; extensible without a schema change
  external_id  text not null,
  account_id   uuid not null references account(account_id),
  created_at   timestamptz not null default now(),
  primary key (method, external_id)
)

signing_key (
  kid          text primary key,
  public_key   bytea not null,
  active       boolean not null default true,
  created_at   timestamptz not null default now()
)

refresh_token (
  token_id     uuid primary key,
  account_id   uuid not null references account(account_id),
  family_id    uuid not null,
  token_hash   bytea not null,
  revoked      boolean not null default false,
  expires_at   timestamptz not null,
  created_at   timestamptz not null default now()
)
```

`identity_binding` is generic (`method` + `external_id`) rather than device-only, so a future email/platform login provider is an `INSERT`, not a migration.

Migrations via `goose`, embedded, run through the `migrate` subcommand per `00-common-stack.md` §3.1.

---

## 3. Key management (AUTH-2)

- Keys are generated **out of band**, once per environment, via a `genkey` CLI subcommand — never at service startup. Regenerating on every boot invalidates every outstanding token.
- Private key: `ed25519.GenerateKey(rand.Reader)`, private half written to a file (`AUTH_SIGNING_KEY_PATH`), mounted read-only into the container as a Jenkins-managed secret (COM-9).
- Public half + `kid`: inserted into `signing_key`.
- JWKS response is hand-built (no third-party JWKS-serving library needed — the shape is fixed and small):

```go
type jwk struct {
    Kty, Crv, Kid, X, Use, Alg string `json:"..."`
}
type jwkSet struct{ Keys []jwk }
```

- Built once at startup from `signing_key` rows where `active = true`, pre-serialized, held behind `atomic.Pointer[[]byte]`, swapped on rotation. Same pattern as Patch's manifest caching in `03-patch-minimal.md`.
- Rotation: insert a new `active` row, keep the previous `kid` active for at least one access-token TTL past its last use as a signing key (15 min + margin — see `06-auth-identity-contract.md` §6), then flip `active = false` on the old row and rebuild the cached JWKS.

---

## 4. Claim schema and issuance (AUTH-3)

Use the `Claims` type from `06-auth-identity-contract.md` §9.1 verbatim — same struct, signing side instead of verifying side.

```go
tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, authn.Claims{
    RegisteredClaims: jwt.RegisteredClaims{
        Issuer:    cfg.Issuer,     // must equal GATEWAY_PLAYER_ISSUER exactly
        Audience:  jwt.ClaimStrings{cfg.Audience},
        Subject:   accountID.String(),
        ExpiresAt: jwt.NewNumericDate(now.Add(cfg.AccessTokenTTL)), // 15m default
        NotBefore: jwt.NewNumericDate(now),
        IssuedAt:  jwt.NewNumericDate(now),
    },
})
tok.Header["kid"] = currentSigningKid
signed, err := tok.SignedString(privateKey)
```

No `roles` field is set — absent on player tokens per the contract.

---

## 5. Anonymous / device provider (AUTH-4)

```
lookup := SELECT account_id FROM identity_binding WHERE method='device' AND external_id=$1
if found:
    sub = lookup.account_id
else:
    tx: INSERT account; INSERT identity_binding(method='device', external_id=$1, account_id=new)
    sub = new account_id
```

**Hard requirement, tested explicitly:** two logins with the same `device_id` return the same `sub`. This is not optional — Session's `player_profile` table is keyed by `sub` (`04-session-minimal.md` §3.1), and a fresh `sub` per login produces duplicate ghost profiles with no error raised anywhere.

---

## 6. Refresh tokens (AUTH-5)

Opaque, not a JWT. Generated with `crypto/rand`, stored as a SHA-256 hash (not a slow password hash — this is 256 bits of CSPRNG output, not a guessable secret, so a slow hash buys nothing and costs CPU).

**Issue:**
```go
raw := make([]byte, 32); rand.Read(raw)
tokenStr := base64.RawURLEncoding.EncodeToString(raw)
hash := sha256.Sum256(raw) // stored; tokenStr is not
```

**Rotate on use, atomically:**
```sql
UPDATE refresh_token SET revoked = true
WHERE token_id = $1 AND revoked = false
```
`RowsAffected == 1` → proceed: insert new row (same `family_id`), issue new access token.
`RowsAffected == 0` → reuse of an already-rotated token. Revoke the entire family:
```sql
UPDATE refresh_token SET revoked = true WHERE family_id = $1
```
Force re-login. Same pattern as the PHP staff-auth refresh design from the original planning conversation, applied here to player tokens.

**Access token TTL:** 15 min (`06-auth-identity-contract.md` §6). **Refresh token TTL:** 30 days, sliding.

---

## 7. Login response (AUTH-8, no hand-off payload)

The login and refresh responses carry tokens only (D2, settled 2026-09-29;
`06-auth-identity-contract.md` §12). `schema_version` sits at the top level and versions the
whole body.

```go
type loginResponse struct {
    SchemaVersion int    `json:"schema_version"` // 1
    AccessToken   string `json:"access_token"`
    ExpiresIn     int64  `json:"expires_in"`
    RefreshToken  string `json:"refresh_token"`
}
```

```json
{
  "schema_version": 1,
  "access_token": "eyJhbGciOiJFZERTQSIs…",
  "expires_in": 900,
  "refresh_token": "n3Jq…"
}
```

The `services` object Auth first shipped, and the `AUTH_PUBLIC_SESSION_URL` setting that
fed it, are removed. A client reaches every service at a fixed path under the gateway base URL
it is configured with, so Auth has no address to hand out, and no copy of one that could
drift. `schema_version` stays `1` because clients already treated `services` as optional. Auth
never provides a game-server address (AUTH-8.5): a client learns the Gameplay Proxy's address
and its join ticket from Session's `party.launching` event when its lobby launches
(`14-launch-handoff.md` §3, §7).

---

## 8. Testing

| Case | Assertion |
|---|---|
| Same `device_id`, two logins | Identical `sub` both times |
| Refresh token used twice | Second use fails, entire `family_id` revoked, subsequent refreshes with any token from that family fail |
| Concurrent refresh with the same token | Exactly one succeeds (`RowsAffected` race resolved correctly), the other triggers family revocation |
| JWKS response | Valid RFC 7517 JWK Set, `kty: OKP`, `crv: Ed25519`, parseable by `MicahParks/keyfunc/v3` (Gateway's actual consumer) |
| Issued token | Verifies successfully against Gateway's middleware using the fixture pattern in `06-auth-identity-contract.md` §10, substituting Auth's real signature |
| Login and refresh body | Exactly `schema_version`, `access_token`, `expires_in`, `refresh_token`; no `services` key (D2) |

---

## 9. Definition of done

- A device can log in anonymously, receive a stable `sub`, and get the same `sub` on every subsequent login from that device.
- Refresh rotation and family revocation both verified under concurrent access.
- JWKS served, matches the contract shape, and a token signed by Auth is accepted by Gateway's real middleware (not just the test fixture).
- The login/refresh response carries tokens only; a client reaches Session at `{gateway base URL}/api/player/session`.
