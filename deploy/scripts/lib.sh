#!/bin/sh
# Helpers shared by deploy/scripts/up.sh and deploy/scripts/release.sh.
#
# Sourced, never executed:
#   . "$(dirname -- "$0")/lib.sh"
#
# Everything here exists because the Go services are distroless. There is no
# shell, no wget and no curl in those images, so a Compose healthcheck has nothing
# to run and `docker compose exec gateway ...` has nothing to exec. Readiness is
# therefore polled from outside, by starting a throwaway container on the same
# network, which is also exactly what COM-8's deploy step has to do.

# Already pulled by the time any probe runs: the stack's own Valkey image, which
# is Alpine. Using it rather than curlimages/curl means a probe needs no image
# that the deployment does not already have, and adds no tag to keep current.
# `--entrypoint sh` is not optional - the Valkey entrypoint rewrites any command
# that is not `valkey-server` into one, so `docker run ... wget` would start a
# server instead of a probe.
PROBE_IMAGE=valkey/valkey:8-alpine

# The one way either script invokes Compose. `--env-file` is explicit rather than
# relying on Compose finding deploy/.env on its own, and the file is named rather
# than discovered so that running from anywhere still reads the same stack. Both
# scripts cd to the deploy directory before this is used, which is what makes the
# relative paths here correct.
COMPOSE="docker compose --env-file .env -f compose.yaml"

# Compose sets this name explicitly in deploy/compose.yaml. A generated
# <project>_<network> name would depend on the directory the stack was started
# from, which is not something a probe should have to be right about.
NETWORK=otomo-net

die() {
    echo "$1" >&2
    exit 1
}

# http_get <host> <port> <path>: the raw HTTP response, over a hand-written
# request. `nc` and `printf` are in every Alpine base; `wget` is not guaranteed to
# be on PATH the same way in every one.
http_get() {
    docker run --rm --network "$NETWORK" --entrypoint sh "$PROBE_IMAGE" -c \
        "printf 'GET $3 HTTP/1.0\r\nHost: $1\r\n\r\n' | nc -w 5 $1 $2"
}

# http_status <host> <port> <path>: just the status line, e.g. `HTTP/1.0 200 OK`.
http_status() {
    http_get "$1" "$2" "$3" 2>/dev/null | head -n 1
}

# wait_http <label> <host> <port> <path> [tries]: 0 once the response is 2xx.
wait_http() {
    label=$1
    host=$2
    port=$3
    path=$4
    tries=${5:-30}

    i=0
    line=""
    while [ "$i" -lt "$tries" ]; do
        line=$(http_status "$host" "$port" "$path" || true)
        case $line in
            *" 2"*)
                echo "  ok      $label ($path)"
                return 0
                ;;
        esac
        i=$((i + 1))
        sleep 2
    done

    echo "  FAILED  $label ($path) -> ${line:-no response after $((tries * 2))s}" >&2
    return 1
}

# report_http <label> <host> <port> <path>: prints a status without judging it.
# Used for /readyz on the gateways, which is 503 by design until the staff
# identity provider exists.
#
# The arguments are named rather than positional for a reason: the first version
# of this passed "$1 $2 $3" straight through, which made the *label* the host and
# the host the port, so every call reported "no response" while the service was
# answering 503 in under a second. A single-shot probe has no retry to hide that,
# so it silently turned a correct 503 into a false alarm in the deploy log.
report_http() {
    label=$1
    host=$2
    port=$3
    path=$4
    line=$(http_status "$host" "$port" "$path" || true)
    echo "  info    $label ($path) -> ${line:-no response}"
}

# container_health <service>: the Compose container's Docker health status, or
# empty when it has none or is not running.
container_health() {
    cid=$($COMPOSE ps -q "$1" 2>/dev/null || true)
    [ -n "$cid" ] || return 1
    docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$cid" 2>/dev/null
}

# wait_healthy <service> [tries]: 0 once Docker reports the healthcheck green.
# A service with no healthcheck is not waited for, and reports `none` so the
# caller can see that rather than assuming something was checked.
wait_healthy() {
    service=$1
    tries=${2:-30}

    i=0
    status=""
    while [ "$i" -lt "$tries" ]; do
        status=$(container_health "$service" || true)
        case $status in
            healthy)
                echo "  ok      $service is healthy"
                return 0
                ;;
            unhealthy)
                echo "  FAILED  $service reported unhealthy" >&2
                return 1
                ;;
            none)
                echo "  info    $service has no healthcheck; not waited for"
                return 0
                ;;
        esac
        i=$((i + 1))
        sleep 2
    done

    echo "  FAILED  $service did not become healthy in $((tries * 2))s (last: ${status:-no container})" >&2
    return 1
}
