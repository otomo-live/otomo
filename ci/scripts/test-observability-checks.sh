#!/bin/sh
# Tests ci/scripts/observability-checks.sh without Docker. A stub `docker` on
# PATH records the image each validator was invoked with and can fail one tool
# or make `alloy fmt` differ from the committed file; the checks themselves run
# against the real deploy/ tree.
#
#   sh ci/scripts/test-observability-checks.sh
set -eu

root=$(cd "$(dirname "$0")/../.." && pwd)
check="$root/ci/scripts/observability-checks.sh"

if [ ! -f "$check" ]; then
  echo "FAIL: $check not found" >&2
  exit 1
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
stub_dir="$tmp/bin"
mkdir -p "$stub_dir"
stub="$stub_dir/docker"
# The script under test resolves the default DOCKER=docker through PATH.
PATH="$stub_dir:$PATH"
export PATH

cat >"$stub" <<'STUB'
#!/bin/sh
# Stub docker. TOOL/IMAGE are derived from the arguments the real script passes.
tool=
image=
for a in "$@"; do
  case $a in
    prom/* | grafana/*) image=$a ;;
  esac
done
case " $* " in
  *" promtool "*) tool=promtool ;;
  *" fmt "*) tool=alloy ;;
  *" -verify-config "*) tool=loki ;;
esac
printf 'TOOL=%s IMAGE=%s\n' "$tool" "$image" >>"$STUB_LOG"

if [ "${STUB_FAIL_TOOL:-}" = "$tool" ]; then
  echo "stub docker: $tool failed on purpose" >&2
  exit 1
fi

# `alloy fmt` prints the formatted config; the committed file is the clean one
# unless the test asks for a difference.
if [ "$tool" = alloy ]; then
  if [ "${STUB_ALLOY_DIFF:-}" = 1 ]; then
    echo "// the stub's formatted output, deliberately different from the file"
  else
    cat "$STUB_ROOT/deploy/observability/config.alloy"
  fi
fi
exit 0
STUB
chmod +x "$stub"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# image_of <service>: the `image:` deploy/compose.yaml pins, read independently
# of the script under test.
image_of() {
  awk -v svc="$1" '
    $0 ~ "^  " svc ":" { inblock = 1; next }
    inblock && /^  [^ ]/ { exit }
    inblock && /^    image:/ {
      sub(/^[[:space:]]*image:[[:space:]]*/, "")
      print
      exit
    }
  ' "$root/deploy/compose.yaml"
}

cd "$root"

# 1. The happy path: every config validates, and the images docker was called
#    with are exactly the ones deploy/compose.yaml pins.
log="$tmp/ok.log"
: >"$log"
if ! STUB_LOG="$log" STUB_ROOT="$root" "$check" >"$tmp/ok.out" 2>&1; then
  cat "$tmp/ok.out" >&2
  fail "happy path exited non-zero"
fi

[ "$(wc -l <"$log")" -eq 3 ] || fail "expected 3 docker calls, got $(wc -l <"$log")"
for svc in prometheus loki alloy; do
  case $svc in
    prometheus) tool=promtool ;;
    loki) tool=loki ;;
    alloy) tool=alloy ;;
  esac
  got=$(sed -n "s/^TOOL=$tool IMAGE=//p" "$log")
  want=$(image_of "$svc")
  [ -n "$got" ] || fail "$tool was not invoked"
  [ "$got" = "$want" ] || fail "$svc image: docker saw '$got', compose pins '$want'"
done

# 2. A failing promtool fails the script, with a clear message and the tool's
#    own error.
log="$tmp/prom.log"
: >"$log"
if STUB_LOG="$log" STUB_ROOT="$root" STUB_FAIL_TOOL=promtool "$check" >"$tmp/prom.out" 2>&1; then
  fail "a failing promtool did not fail the script"
fi
grep -q "FAIL: prometheus config" "$tmp/prom.out" || fail "no clear prometheus FAIL: $(cat "$tmp/prom.out")"
grep -q "failed on purpose" "$tmp/prom.out" || fail "promtool's own error was not shown"

# 3. `alloy fmt` output that differs from the committed file fails the script.
log="$tmp/alloy.log"
: >"$log"
if STUB_LOG="$log" STUB_ROOT="$root" STUB_ALLOY_DIFF=1 "$check" >"$tmp/alloy.out" 2>&1; then
  fail "a differing alloy fmt output did not fail the script"
fi
grep -q "FAIL: alloy config is not alloy-formatted" "$tmp/alloy.out" ||
  fail "no clear alloy FAIL: $(cat "$tmp/alloy.out")"

echo "PASS: observability-checks.sh"
