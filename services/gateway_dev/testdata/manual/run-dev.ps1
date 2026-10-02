# run-dev.ps1 — Start gateway_dev against fake upstreams.
# Prerequisites: fake upstreams running (go run fake_upstream.go)
#
# This service uses upstreams on 9003-9007. Port 9001/9002 (auth, patch) belong
# to services/gateway's harness and are never reached from here.

$env:GATEWAY_DEV_LOG_PRETTY  = "1"
$env:GATEWAY_DEV_LISTEN_ADDR = "127.0.0.1:8090"
$env:GATEWAY_DEV_METRICS_ADDR = "127.0.0.1:8091"

$env:GATEWAY_DEV_UPSTREAM_ADMINAUTH_URL = "http://127.0.0.1:9004"
$env:GATEWAY_DEV_UPSTREAM_ADMINUI_URL   = "http://127.0.0.1:9005"
$env:GATEWAY_DEV_UPSTREAM_CONFIG_URL    = "http://127.0.0.1:9006"
$env:GATEWAY_DEV_UPSTREAM_DASHBOARD_URL = "http://127.0.0.1:9007"
$env:GATEWAY_DEV_UPSTREAM_SESSION_URL   = "http://127.0.0.1:9003"

$env:GATEWAY_STAFF_JWKS_URL = "http://localhost:9999/.well-known/staff-jwks.json"
$env:GATEWAY_STAFF_ISSUER   = "https://admin-auth.otomo.internal"
$env:GATEWAY_STAFF_AUDIENCE = "otomo:staff"

Write-Host "Starting gateway_dev on http://127.0.0.1:8090 ..." -ForegroundColor Cyan
Write-Host "Metrics on http://127.0.0.1:8091/healthz" -ForegroundColor Cyan
Write-Host "Protected routes need a staff token; see TESTING.md." -ForegroundColor Yellow
Write-Host ""

Set-Location "$PSScriptRoot\..\.."
go run .
