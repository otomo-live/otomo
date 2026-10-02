# Otomo Auth Service — Executive Brief

**Date:** 2026-09-18
**Status:** Skeleton complete and verified on laptop, WSL, and the team VM. Not yet committed or deployed.
**Audience:** Anyone on the team, no prior knowledge of the tools assumed.

---

## 1. What was built, in plain terms

Otomo is the backend for a multiplayer game. **Auth** is the service that issues players their "ID badges" (login tokens). Every other service — Gateway, Session, and so on — checks that badge before it does anything for a player.

What exists today is the **skeleton** of Auth: the building is up, the plumbing works, the lights are on, and the badge printer is installed and tested — but the front counter isn't staffed yet.

| Done | Not yet (deliberately) |
|---|---|
| Starts up, reads its settings, refuses to start if anything essential is missing | Actual player login (`/auth/anonymous`) |
| Connects to its database and creates its four tables | Token refresh and logout (`/auth/refresh`, `/auth/logout`) |
| Generates and safely stores its signing key; publishes the matching public key so other services can verify badges | The "here's where to find the other services" payload |
| Can mint a real, verifiable badge (token) | Automatic key rotation |
| Health checks, metrics, structured logs, graceful shutdown | Wiring into the deploy pipeline (needs secrets management first) |

The three unfinished endpoints exist but politely answer "not implemented yet" (HTTP 501), so nothing downstream breaks.

---

## 2. How it's implemented — the tools, explained

- **Go** — the programming language. Compiles to a single self-contained file; no runtime to install on the server.
- **PostgreSQL** — the database. Holds player accounts, device bindings, signing keys, and refresh tokens. The table layout is versioned with **goose**, which applies schema changes in order and remembers which ones it already ran, so running it twice is harmless.
- **Ed25519 / JWT / JWKS** — the badge system. Auth holds a *private* key (secret, on disk, never in the database or the code) and signs each token with it. It publishes the *public* half at `/.well-known/jwks.json`; anyone can fetch that and verify a token is genuine without ever contacting Auth. Tokens expire after 15 minutes.
- **Docker** — packages the program plus exactly what it needs into an "image" that runs identically on a laptop, in WSL, or on the VM. The image is 26 MB and runs as a non-root user, so a compromised process can't do much.
- **Prometheus metrics + JSON logs** — the service reports request counts, latencies, and one structured log line per request, each tagged with a request ID that Gateway generates, so one player's request can be traced across services.
- **Two network ports** — one public (what Gateway talks to) and one internal (`/healthz`, `/readyz`, `/metrics`, profiling). The internal port is never exposed, so diagnostics can't leak.

Three commands, one binary:

| Command | What it does |
|---|---|
| `auth serve` | Run the service (the default) |
| `auth migrate` | Create or upgrade the database tables |
| `auth genkey` | Create a signing key — done once per environment, never on startup, because a new key would invalidate every player's existing badge |

Source lives in `services/auth/`. Its `README.md` has the full configuration table and the reasoning behind each design decision.

---

## 3. How the work was produced

1. **Plan** — `design/08-auth-skeleton-plan.md`, a detailed spec derived from `00-common-stack.md`, `06-auth-identity-contract.md` and `07-auth-techspec.md`, resolving gaps those documents left (key file format, how the service learns which key is its own, etc.).
2. **Implementation by DeepSeek** — a cheaper AI model executed the plan headlessly via the reusable `/deepseek` skill.
3. **Independent vetting** — every file it produced was read line by line. Round 1 had two real bugs (a mislabelled metric that would have failed its own tests, and a middleware ordering that lost log lines on crashes). Round 2 fixed both.
4. **Verification in three environments** — Windows laptop (Go 1.27.1 installed for this), WSL (Docker build + live Postgres), and the team VM (everything, including the Go race detector, which needs Linux).

Result: 28/28 tests pass, no data races, no known vulnerabilities (`govulncheck`), the Docker image builds and runs correctly, and an end-to-end smoke test exercises the full lifecycle.

---

## 4. How to test on the VM

**Prerequisites** (all already in place as of 2026-09-18): an SSH key for `you@203.0.113.10` in WSL, password-less `sudo` for `you`, Docker + Go 1.27.1 + gcc on the VM, and a Postgres 17 container named `otomo-pg`.

### Step 1 — get the latest code onto the VM

From a WSL terminal:

```sh
scp -r /mnt/c/otomo/services/auth you@203.0.113.10:~/otomo-auth-test/
ssh you@203.0.113.10
```

### Step 2 — unit and integration tests (about 10 seconds)

```sh
cd ~/otomo-auth-test/auth
sudo docker start otomo-pg                                      # in case the VM rebooted
AUTH_TEST_DATABASE_URL=postgres://auth_rw:pw@127.0.0.1:5432/auth_test go test -race ./...
```

**Good looks like:** every package line says `ok`. `-race` catches concurrency bugs; the environment variable makes the database tests run for real instead of skipping.

### Step 3 — build the container image (about a minute the first time)

```sh
sudo docker build -t otomo-auth:skeleton .
```

**Good looks like:** the output ends with `naming to docker.io/library/otomo-auth:skeleton` and contains no `ERROR`.

### Step 4 — full lifecycle smoke test (about 15 seconds)

```sh
sudo sh smoke.sh
```

It runs the migrations twice, generates a key, tries to overwrite it (must refuse), starts the service, hits every endpoint, stops it with a shutdown signal, then disables the key in the database and confirms the service refuses to start.

**Good looks like:**

```
healthz: 200
readyz: ok 200
jwks: {"keys":[{"kty":"OKP","crv":"Ed25519",...}]} 200
metrics on public: 404
...
container exit=0
...
"signing key at AUTH_SIGNING_KEY_PATH has no active row in signing_key; run `auth genkey`"
exit=1
```

**If something fails:** the output names the step. The most common cause after a reboot is `otomo-pg` not running — Step 2's `docker start` fixes it.

### Step 5 — optional cleanup

```sh
sudo docker rm -f otomo-pg          # the throwaway database (see section 5)
sudo docker rmi otomo-auth:skeleton
rm -rf ~/otomo-auth-test
```

---

## 5. Things to know before this goes further

- **`otomo-pg` is a throwaway.** Password `pw`, no restart policy, no named volume, port bound to localhost only. Before real data touches it, it should become a proper Compose-managed database (task COM-7 in the plan) with per-service users (COM-6).
- **Jenkins will "fail" the Auth deploy** once this code merges — the container exits immediately because no database URL is provided. That is correct fail-fast behaviour; the fix is the secrets/env work (COM-9), not a change to Auth.
- **Jenkins' test stage runs `go test` without `-race`**; the plan (COM-12) calls for `-race`. gcc is now on the VM, so adding the flag is a one-line Jenkinsfile change.
- **Two values are still "proposed, not confirmed"** in the identity contract: the exact `iss` and `aud` strings. They are environment variables, so confirming them later costs nothing, but Auth and Gateway must use identical strings or every player request will be rejected.
- **On a Windows laptop's WSL**, Docker builds need `--network=host` because of a DNS quirk inside build containers. The VM does not have this problem.
- **Nothing is committed yet.** The changes sit in the working tree under `services/auth/` plus the `eol=lf` lines in `.gitattributes`.

---

## 6. What comes next

In the order the plan suggests:

1. Commit the skeleton.
2. Implement the stubbed endpoints — device login (AUTH-4), refresh rotation with family revocation (AUTH-5), the `services` hand-off payload (AUTH-8). The schema for all three already exists.
3. Provision Postgres properly (COM-6/7) and wire secrets into the deploy (COM-9) so the Jenkins staging container stays up.
4. Confirm `iss`/`aud` with whoever owns Gateway and admin-auth.
