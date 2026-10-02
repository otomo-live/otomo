#!/bin/sh
# Assert the properties of compose.yaml that the file's comments
# promise, from the same rendering Compose itself uses. `docker compose config`
# resolves anchors, interpolation and defaults, so the JSON checked here is what a
# deploy would actually run rather than the YAML's text.
#
# The properties are the ones a reviewer cannot see from a diff:
#
#   1. Staff trust is one pinned pair everywhere. Every *_STAFF_JWKS_URL and
#      *_STAFF_ISSUER, and admin-auth's own ADMIN_AUTH_ISSUER, is the service
#      name and the contract issuer - not a host-local override.
#   2. Config mounts the blobs volume read-write and Patch mounts it read-only.
#   3. Only registry, gateway, gateway_dev, the edge and the gameplay proxy publish a host port, and each on
#      the interface and port it did before.
#   4. Every one-shot *-migrate service is restart: "no", so a completed
#      migration does not get looped by the daemon.
#   5. Each service's database user reaches exactly the database that role owns.
#   6. The observability stack's limits (see below).
#   7. Player trust is one pinned pair everywhere: auth's
#      AUTH_ISSUER/AUTH_AUDIENCE, every *_PLAYER_ISSUER/_AUDIENCE/_JWKS_URL in
#      compose, and the same keys in every services/*/.env.example. A mismatch
#      fails closed at runtime (every player request 401s), which is safe but
#      costs a debugging session; this makes it a failed check instead.
#   8. Session's internal listener (:8081, the Allocator's callback) is named by
#      the Allocator and by no other service.
#
# Usage: deploy/scripts/check-compose.sh
#   ENV_FILE     read this deploy env file instead of a generated throwaway
#   COMPOSE_BIN  the compose command to use (e.g. "$HOME/bin/docker-compose");
#                otherwise docker-compose or the docker compose plugin is found.

set -eu

DEPLOY_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$DEPLOY_DIR"

command -v python3 >/dev/null 2>&1 || {
    echo "check-compose: python3 is not on PATH" >&2
    exit 2
}

# The composition of the command is the caller's business: a bare name uses
# PATH, while COMPOSE_BIN may carry a subcommand ("docker compose"). Word
# splitting is deliberate for that reason.
compose() {
    if [ -n "${COMPOSE_BIN:-}" ]; then
        # shellcheck disable=SC2086
        $COMPOSE_BIN "$@"
    elif command -v docker-compose >/dev/null 2>&1; then
        docker-compose "$@"
    elif command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
        docker compose "$@"
    else
        echo "check-compose: no compose binary found; set COMPOSE_BIN" >&2
        exit 2
    fi
}

generated_env=
json=$(mktemp)
cleanup() {
    rm -f "$json"
    [ -n "$generated_env" ] && rm -f "$generated_env"
    return 0
}
trap cleanup EXIT INT TERM

# A throwaway env with every password filled in, so `config` can render without a
# real deploy/.env. An operator can point ENV_FILE at one instead.
if [ -z "${ENV_FILE:-}" ]; then
    [ -f .env.example ] || {
        echo "check-compose: deploy/.env.example is missing" >&2
        exit 1
    }
    generated_env=$(mktemp)
    sed 's/^\([A-Z_]*PASSWORD\)=$/\1=x/' .env.example > "$generated_env"
    ENV_FILE=$generated_env
fi

# Every profile, so the opt-in edge is checked like everything else.
compose --env-file "$ENV_FILE" -f compose.yaml --profile '*' config --format json > "$json"

python3 - "$json" "$DEPLOY_DIR/.." <<'PY'
import json
import sys
import urllib.parse

cfg = json.load(open(sys.argv[1]))
services = cfg["services"]
problems = []


def fail(msg):
    problems.append(msg)


def env_of(name):
    return {
        k: "" if v is None else str(v)
        for k, v in (services[name].get("environment") or {}).items()
    }


PINNED_JWKS = "http://admin-auth:8080/.well-known/jwks.json"
PINNED_ISSUER = "https://admin-auth.otomo.internal"
STAFF_AUDIENCE = "otomo:staff"

# 1. One staff trust pair everywhere, with no Compose indirection left.
for name, svc in services.items():
    for key, value in (svc.get("environment") or {}).items():
        if key.endswith("_STAFF_JWKS_URL") and value != PINNED_JWKS:
            fail(f"{name}: {key} is {value!r}, want {PINNED_JWKS!r}")
        if key.endswith("_STAFF_ISSUER") and value != PINNED_ISSUER:
            fail(f"{name}: {key} is {value!r}, want {PINNED_ISSUER!r}")
        if key.endswith("_STAFF_AUDIENCE") and str(value) != STAFF_AUDIENCE:
            fail(f"{name}: {key} is {value!r}, want {STAFF_AUDIENCE!r}")
        if "${" in str(value):
            fail(f"{name}: {key} still contains a substitution: {value!r}")

# Every staff verifier must actually carry the pair, not merely not contradict it.
for name, prefix in {
    "gateway": "GATEWAY",
    "gateway_dev": "GATEWAY",
    "config": "CONFIG",
    "patch": "PATCH",
    "dashboard": "DASHBOARD",
    "session": "SESSION",
}.items():
    e = env_of(name)
    if e.get(f"{prefix}_STAFF_JWKS_URL") != PINNED_JWKS:
        fail(f"{name}: {prefix}_STAFF_JWKS_URL is missing or wrong")
    if e.get(f"{prefix}_STAFF_ISSUER") != PINNED_ISSUER:
        fail(f"{name}: {prefix}_STAFF_ISSUER is missing or wrong")

admin_auth = env_of("admin-auth")
if admin_auth.get("ADMIN_AUTH_ISSUER") != PINNED_ISSUER:
    fail("admin-auth: ADMIN_AUTH_ISSUER must equal the pinned issuer")
if admin_auth.get("ADMIN_AUTH_AUDIENCE") != STAFF_AUDIENCE:
    fail("admin-auth: ADMIN_AUTH_AUDIENCE must equal the pinned audience")

# 2. blobs is read-write for Config and read-only for Patch.
def blobs_mounts(name):
    return [
        v
        for v in services[name].get("volumes", [])
        if v.get("source") == "blobs"
    ]


for name, want_ro in (("config", False), ("patch", True)):
    mounts = blobs_mounts(name)
    if len(mounts) != 1:
        fail(f"{name}: expected exactly one blobs mount, found {len(mounts)}")
        continue
    mount = mounts[0]
    if bool(mount.get("read_only")) != want_ro:
        state = "read-only" if want_ro else "read-write"
        fail(f"{name}: blobs must be mounted {state}")
    if mount.get("target") != "/var/lib/otomo/blobs":
        fail(f"{name}: blobs must be mounted at /var/lib/otomo/blobs")

# Config must create the blobs volume: whoever mounts an empty named volume first
# decides its ownership, and only Config's image carries a nonroot-owned directory.
if "config" not in (services["patch"].get("depends_on") or {}):
    fail("patch: must depend on config so Config initialises the blobs volume")

# 3. The published set: registry, gateway and gateway_dev on loopback, and the
# edge on 80/443 on every interface - the only public TCP ports.
expected_ports = {
    "registry": [("127.0.0.1", "5000", 5000)],
    "gateway": [("127.0.0.1", "8080", 8080)],
    "gateway_dev": [("127.0.0.1", "8090", 8080)],
    "edge": [(None, "80", 80), (None, "443", 443)],
    # The Gameplay Proxy is the one public UDP port (checked for udp below).
    "gameplay-proxy": [(None, "27000", 27000)],
}
for name, want in expected_ports.items():
    ports = services[name].get("ports") or []
    got = sorted(((p.get("host_ip") or None), str(p.get("published")), p.get("target")) for p in ports)
    if got != sorted(want, key=str):
        fail(f"{name}: publishes {got}, want {want}")
for name, svc in services.items():
    if name not in expected_ports and svc.get("ports"):
        fail(f"{name}: publishes a host port; only registry, gateway, gateway_dev, edge and gameplay-proxy may")
for p in services["gameplay-proxy"].get("ports") or []:
    if p.get("protocol") != "udp":
        fail(f"gameplay-proxy: port {p.get('published')} must be udp, got {p.get('protocol')!r}")

# The gateway believes X-Forwarded-For only from the edge's fixed address.
edge_ip = ((services["edge"].get("networks") or {}).get("otomo-edge") or {}).get("ipv4_address")
if env_of("gateway").get("GATEWAY_TRUSTED_PROXIES") != edge_ip:
    fail(f"gateway: GATEWAY_TRUSTED_PROXIES must be exactly the edge's address {edge_ip!r}")
import ipaddress
edge_ipam = ((cfg.get("networks") or {}).get("otomo-edge") or {}).get("ipam", {}).get("config") or [{}]
ip_range = edge_ipam[0].get("ip_range")
if not ip_range or ipaddress.ip_address(edge_ip) in ipaddress.ip_network(ip_range):
    fail(f"otomo-edge: the edge's {edge_ip} must lie outside the dynamic ip_range ({ip_range!r}), or another container can take it")
# The wiki sites are public-tier only, and only the public tier is
# on otomo-edge.
for name in ("wiki-docs", "wiki-site"):
    if set(services[name].get("networks") or {}) != {"otomo-edge"}:
        fail(f"{name}: must be on otomo-edge only")
on_edge = {n for n, s in services.items() if "otomo-edge" in (s.get("networks") or {})}
if on_edge != {"edge", "gateway", "wiki-docs", "wiki-site"}:
    fail(f"otomo-edge must hold only the edge, the gateway and the wiki sites, found {sorted(on_edge)}")
if set(services["edge"].get("networks") or {}) != {"otomo-edge"}:
    fail("edge: must be on otomo-edge only (never otomo-net)")

# 4. Every one-shot migration is restart: "no".
for name, svc in services.items():
    if name.endswith("-migrate") and svc.get("restart") != "no":
        fail(f"{name}: restart is {svc.get('restart')!r}, want 'no'")
for name in ("auth-migrate", "admin-auth-migrate", "config-migrate", "session-migrate", "allocator-migrate"):
    if name not in services:
        fail(f"{name} is missing from compose.yaml")

# 5. Each DSN's user owns (or, for patch_ro, may read) exactly its database.
expected_dsn = {
    "auth": ("auth_rw", "auth"),
    "auth-migrate": ("auth_rw", "auth"),
    "admin-auth": ("admin_auth_rw", "admin_auth"),
    "admin-auth-migrate": ("admin_auth_rw", "admin_auth"),
    "config": ("config_rw", "config"),
    "config-migrate": ("config_rw", "config"),
    "patch": ("patch_ro", "config"),
    "session": ("session_rw", "session"),
    "session-migrate": ("session_rw", "session"),
    "allocator": ("allocator_rw", "allocator"),
    "allocator-migrate": ("allocator_rw", "allocator"),
}
found = set()
for name, svc in services.items():
    for key, value in (svc.get("environment") or {}).items():
        if not key.endswith("_DATABASE_URL"):
            continue
        found.add(name)
        parsed = urllib.parse.urlparse(str(value))
        user = urllib.parse.unquote(parsed.username or "")
        database = (parsed.path or "").lstrip("/")
        if name not in expected_dsn:
            fail(f"{name}: unexpected {key}")
            continue
        want = expected_dsn[name]
        if (user, database) != want:
            fail(f"{name}: {key} is {user!r}@{database!r}, want {want[0]!r}@{want[1]!r}")
for name in expected_dsn:
    if name not in found:
        fail(f"{name}: no *_DATABASE_URL found")

# 6. The observability stack publishes nothing, every container has a
# memory ceiling, the ceilings total under the 650 MB budget, and Prometheus and
# Loki keep their data on named volumes. The published-port rule in section 3
# already rejects a port on any of these; this repeats it because it is the
# property that keeps the stores off the internet.
OBSERVABILITY = {
    "prometheus": ("prometheus-data", "/prometheus"),
    "loki": ("loki-data", "/loki"),
    "alloy": (None, None),
    "node_exporter": (None, None),
    "cadvisor": (None, None),
    "postgres_exporter": (None, None),
}
declared_volumes = cfg.get("volumes") or {}
total_limit_bytes = 0
for name, (volume, target) in OBSERVABILITY.items():
    svc = services.get(name)
    if svc is None:
        fail(f"{name} is missing from compose.yaml")
        continue
    if svc.get("ports"):
        fail(f"{name}: publishes a host port; the observability stack must not")
    limits = ((svc.get("deploy") or {}).get("resources") or {}).get("limits") or {}
    raw = limits.get("memory")
    if not raw:
        fail(f"{name}: has no memory limit")
    else:
        try:
            total_limit_bytes += int(raw)
        except (TypeError, ValueError):
            fail(f"{name}: memory limit {raw!r} is not an integer number of bytes")
    if volume is None:
        continue
    if volume not in declared_volumes:
        fail(f"{volume}: is not a top-level named volume")
    mounts = [v for v in svc.get("volumes", []) if v.get("source") == volume]
    if len(mounts) != 1 or mounts[0].get("target") != target:
        fail(f"{name}: {volume} must be mounted at {target}")

if total_limit_bytes > 650 * 1024 * 1024:
    fail(f"observability memory limits total {total_limit_bytes} bytes, over the 650 MB cap")

# 7. One player trust pair everywhere, in compose and in every
# service's .env.example.
PLAYER = {
    "_PLAYER_JWKS_URL": "http://auth:8080/.well-known/jwks.json",
    "_PLAYER_ISSUER": "https://auth.otomo.internal",
    "_PLAYER_AUDIENCE": "otomo:player",
}
ISSUER_KEYS = {"AUTH_ISSUER": PLAYER["_PLAYER_ISSUER"], "AUTH_AUDIENCE": PLAYER["_PLAYER_AUDIENCE"]}


def player_pin(key):
    """The pinned value for key, or None when key is not part of the player pair."""
    if key in ISSUER_KEYS:
        return ISSUER_KEYS[key]
    for suffix, want in PLAYER.items():
        if key.endswith(suffix):
            return want
    return None


for name, svc in services.items():
    for key, value in (svc.get("environment") or {}).items():
        want = player_pin(key)
        if want is not None and str(value) != want:
            fail(f"{name}: {key} is {value!r}, want {want!r}")

# The carriers must actually set the pair, not merely not contradict it: a missing
# variable would fall back to a code default that nothing here checks.
for name, keys in {
    "auth": ("AUTH_ISSUER", "AUTH_AUDIENCE"),
    "gateway": ("GATEWAY_PLAYER_JWKS_URL", "GATEWAY_PLAYER_ISSUER", "GATEWAY_PLAYER_AUDIENCE"),
    "session": ("SESSION_PLAYER_JWKS_URL", "SESSION_PLAYER_ISSUER", "SESSION_PLAYER_AUDIENCE"),
}.items():
    if name not in services:
        fail(f"{name} is missing from compose.yaml")
        continue
    e = env_of(name)
    for key in keys:
        if key not in e:
            fail(f"{name}: {key} is not set")

# Auth's login response carries no services hand-off any more (D2), so
# nothing may set the variable that used to feed it.
if "AUTH_PUBLIC_SESSION_URL" in env_of("auth"):
    fail("auth: AUTH_PUBLIC_SESSION_URL is set, but the services hand-off was removed (D2)")

# The same pair in every service's .env.example, which is what a developer copies
# to run a service outside compose. Staff keys are not checked here.
import glob
import os

repo_root = sys.argv[2]
examples = sorted(glob.glob(os.path.join(repo_root, "services", "*", ".env.example")))
if not examples:
    fail(f"no services/*/.env.example found under {repo_root}")
carriers = set()
for path in examples:
    rel = os.path.relpath(path, repo_root)
    with open(path) as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            key, value = line.split("=", 1)
            want = player_pin(key)
            if want is None:
                continue
            carriers.add(rel)
            if value != want:
                fail(f"{rel}: {key}={value!r}, want {want!r}")
for rel in ("services/auth/.env.example", "services/gateway/.env.example", "services/session/.env.example"):
    if rel not in carriers:
        fail(f"{rel}: does not pin the player issuer/audience")

# 8. LB-4: Session's internal listener is where the Allocator's callback
# lands, and nothing else points at it. Its only access control is the service key and
# not being routed, so a gateway upstream naming it would expose the callback.
if env_of("session").get("SESSION_INTERNAL_ADDR") != ":8081":
    fail("session: SESSION_INTERNAL_ADDR must be \":8081\", where ALLOCATOR_SESSION_URL points")
if "allocator" in services and env_of("allocator").get("ALLOCATOR_SESSION_URL") != "http://session:8081":
    fail("allocator: ALLOCATOR_SESSION_URL must be http://session:8081, Session's internal listener")
for name, svc in services.items():
    if name == "allocator":
        continue
    for key, value in (svc.get("environment") or {}).items():
        if "session:8081" in str(value):
            fail(f"{name}: {key} points at Session's internal listener; only the Allocator may")

if problems:
    print("check-compose: FAILED", file=sys.stderr)
    for problem in problems:
        print(f"  {problem}", file=sys.stderr)
    sys.exit(1)

print("check-compose: ok")
PY
