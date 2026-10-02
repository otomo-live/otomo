#!/bin/sh
# Validates the Dashboard's observability configs, run on the Jenkins agent
# (which has the docker CLI; the `go` container of ci/compose.yaml does not):
#
#   sh ci/scripts/observability-checks.sh
#
# The configs live in deploy/observability/ but their data plane is the
# Dashboard, so a change there dispatches otomo-dashboard and the
# "Observability configs" stage in ci/services/Jenkinsfile runs this. There is
# no Jenkins job for deploy/.
#
# Each config is checked by the image deploy/compose.yaml actually deploys, read
# from that file below, so the validator can never drift from the deployment:
#
#   promtool check config          prometheus
#   alloy fmt (diffed)             alloy: syntax plus formatting
#   -verify-config                 loki
#
# The three run on the host, against a read-only bind of deploy/observability/,
# and the script stops at the first failure with a one-line FAIL. Set DOCKER to
# exercise it without docker (ci/scripts/test-observability-checks.sh does).
set -eu

DOCKER=${DOCKER:-docker}

root=$(cd "$(dirname "$0")/../.." && pwd)
compose="$root/deploy/compose.yaml"
cfg="$root/deploy/observability"

if [ ! -f "$compose" ]; then
  echo "FAIL: $compose not found (run from the repository root)" >&2
  exit 1
fi

# image_of <service>: the `image:` of that service in deploy/compose.yaml, or
# empty. A service block runs from its two-space key to the next two-space key;
# the first four-space `image:` inside it is the one.
image_of() {
  awk -v svc="$1" '
    $0 ~ "^  " svc ":" { inblock = 1; next }
    inblock && /^  [^ ]/ { exit }
    inblock && /^    image:/ {
      sub(/^[[:space:]]*image:[[:space:]]*/, "")
      print
      exit
    }
  ' "$compose"
}

PROM=$(image_of prometheus)
LOKI=$(image_of loki)
ALLOY=$(image_of alloy)
for pair in "prometheus:$PROM" "loki:$LOKI" "alloy:$ALLOY"; do
  if [ -z "${pair#*:}" ]; then
    echo "FAIL: no image: found for ${pair%%:*} in $compose" >&2
    exit 1
  fi
done

# promtool: config syntax and semantic checks. Its own error is on stderr.
echo "== prometheus: promtool check config (${PROM}) =="
if "$DOCKER" run --rm -v "$cfg:/cfg:ro" --entrypoint promtool "$PROM" check config /cfg/prometheus.yml; then
  echo "OK: prometheus config"
else
  echo "FAIL: prometheus config (promtool check config)" >&2
  exit 1
fi

# alloy fmt prints the formatted config. Diffing catches both syntax errors
# (fmt fails) and a config that is valid but not formatted (diff fails), which
# keeps the committed file canonical.
echo "== alloy: alloy fmt (${ALLOY}) =="
formatted=$(mktemp)
trap 'rm -f "$formatted"' EXIT
if ! "$DOCKER" run --rm -v "$cfg:/cfg:ro" "$ALLOY" fmt /cfg/config.alloy >"$formatted"; then
  echo "FAIL: alloy config (alloy fmt)" >&2
  exit 1
fi
if ! diff -u "$cfg/config.alloy" "$formatted"; then
  echo "FAIL: alloy config is not alloy-formatted (see diff above); run: alloy fmt deploy/observability/config.alloy" >&2
  exit 1
fi
echo "OK: alloy config"

# loki: parse and validate the config without starting the server.
echo "== loki: -verify-config (${LOKI}) =="
if "$DOCKER" run --rm -v "$cfg:/cfg:ro" "$LOKI" -config.file=/cfg/loki.yml -verify-config; then
  echo "OK: loki config"
else
  echo "FAIL: loki config (-verify-config)" >&2
  exit 1
fi

echo "OK: observability configs"
