# Service CI

Every `otomo-<service>` Jenkins job runs the same file, `ci/services/Jenkinsfile`,
with its service name as the `SERVICE` parameter. Nothing per service is
configured in Jenkins beyond that; what differs per service lives here, found by
convention from `SERVICE`.

```
ci/services/
  Jenkinsfile                   the one pipeline every service job runs
  lib/unit_test.sh              the pre-deploy levels and their contract
  otomo-<service>/unit_test.sh  that service's pre-deploy tests
```

## Pipeline

| Stage | What runs | Where it is defined |
| --- | --- | --- |
| Observability configs | validates `deploy/observability/{prometheus.yml,loki.yml,config.alloy}` against the images `deploy/compose.yaml` pins; dashboard job only | `ci/scripts/observability-checks.sh` |
| Test | gofmt, vet, `go test -race` (DB tests included), govulncheck, drift, golangci-lint (non-blocking); npm chain for adminui | `ci/scripts/go-checks.sh` |
| Smoke | end-to-end smoke test (gateway, gateway_dev, session) | `ci/scripts/smoke.sh` |
| Image | `docker build` of the service, then removed | Jenkinsfile |
| Pre-deploy: sanity → functional → integration → security → scaling | `ci/services/otomo-<service>/unit_test.sh <level>` | this directory |

Everything runs in the per-build Compose stack in `ci/compose.yaml`. A stage
runs only if every stage before it passed.

The observability stage is the exception to the stack: it runs on the Jenkins
agent, because the configs are validated by the `promtool`, `alloy` and `loki`
images themselves and the compose `go` container has no docker CLI. It runs only
in the dashboard job: `deploy/observability/` belongs to the Dashboard's data
plane, and `deploy/` has no job of its own, so `ci/dispatcher/Jenkinsfile`
dispatches a change there to `otomo-dashboard`. See
`ci/scripts/observability-checks.sh`.

## Writing pre-deploy tests

Open `otomo-<service>/unit_test.sh` and replace a level's `not_implemented` with
the real checks. A level function runs from the repository root inside the
service's CI container, with the toolchain, Postgres, Valkey and the
`*_TEST_DATABASE_URL` variables available; it passes by returning 0. See
`lib/unit_test.sh` for what each level is for.

Run one level locally the way Jenkins does:

```sh
docker compose -f ci/compose.yaml run --rm --workdir /src go \
  sh ci/services/otomo-<service>/unit_test.sh sanity
docker compose -f ci/compose.yaml down -v
```

(`node` instead of `go` for adminui; add `--no-deps` for a service that needs no
database.) Pushing a change to `otomo-<service>/` re-runs only that service's
job; a change anywhere else under `ci/` re-runs every service.

## Adding a service

Create its `otomo-<service>` job pointing at this Jenkinsfile, add it to
`SERVICES` in `ci/dispatcher/Jenkinsfile`, and copy any
`otomo-<service>/unit_test.sh`, changing `SERVICE=`. A job whose
`unit_test.sh` is missing fails rather than skipping the pre-deploy gate.
