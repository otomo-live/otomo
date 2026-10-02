# run-player.ps1 — Start the player gateway against fake upstreams.
# Prerequisites: fake upstreams running (go run fake_upstream.go)
#
# The admin and dev gateway is a separate service: services/gateway_dev.

$env:GATEWAY_LOG_PRETTY     = "1"
$env:GATEWAY_LISTEN_ADDR    = "127.0.0.1:8080"
$env:GATEWAY_METRICS_ADDR   = "127.0.0.1:8081"

$env:GATEWAY_UPSTREAM_AUTH_URL    = "http://127.0.0.1:9001"
$env:GATEWAY_UPSTREAM_PATCH_URL   = "http://127.0.0.1:9002"
$env:GATEWAY_UPSTREAM_SESSION_URL = "http://127.0.0.1:9003"

$env:GATEWAY_PLAYER_JWKS_URL  = "http://localhost:9999/.well-known/jwks.json"
$env:GATEWAY_PLAYER_ISSUER    = "https://auth.example.com"
$env:GATEWAY_PLAYER_AUDIENCE  = "player-api"

# Required, not optional: /patch/v1/dev/manifest and /patch/v1/staging/manifest
# take a staff token, so this gateway loads a second issuer. The admin routes do
# not exist here; that is services/gateway_dev.
$env:GATEWAY_STAFF_JWKS_URL   = "http://localhost:9999/.well-known/staff-jwks.json"
$env:GATEWAY_STAFF_ISSUER     = "https://admin.example.com"
$env:GATEWAY_STAFF_AUDIENCE   = "staff-api"

Write-Host "Starting gateway (players) on http://127.0.0.1:8080 ..." -ForegroundColor Cyan
Write-Host "Metrics on http://127.0.0.1:8081/healthz" -ForegroundColor Cyan
Write-Host "Admin paths 404 here; see services/gateway_dev." -ForegroundColor Yellow
Write-Host ""

Set-Location "$PSScriptRoot\..\.."
go run .
