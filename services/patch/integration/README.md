# Patch integration tests

This directory holds the end-to-end test that proves a Config publish or rollback
reaches Patch's read-only manifest, and that the manifest's blob is fetchable. It is
kept out of `go test ./...` by the `integration` build tag so Unit runs need no
database and no second service.

## What it covers

`TestPublishRollback` builds the real `config` and `patch` binaries into a temp
directory and runs them as child processes against one Postgres database and one
shared blob directory:

1. `config migrate` (idempotent), then both `serve` commands, waiting for `/readyz`.
2. Staff key material in-process: an Ed25519 key, a JWKS served by `httptest`, and
   `admin`/`live_ops` tokens signed for `PATCH_STAFF_ISSUER`/`PATCH_STAFF_AUDIENCE`.
3. A unique namespace, schema, draft, version 1, draft, version 2 — all through
   Config's HTTP API.
4. Publish version 1 and wait for Patch's dev manifest ETag to change and name
   version 1; `If-None-Match` then revalidates with an empty `304`.
5. Publish version 2 and wait for the second manifest.
6. Roll back to the version 1 release and assert Patch serves bytes (and the
   content-derived ETag) byte-identical to the first publish.
7. `GET /patch/v1/blob/{sha256}` for the namespace's blob and re-hash it.

The test takes the same Postgres advisory lock Config's tests use
(`hashtext('config_test:dev')`) for its whole run, and restores the original `dev`
head in cleanup, so it does not race the rest of the suite.

## Running it locally

Point `PATCH_TEST_DATABASE_URL` at the same database Config's tests use (CI uses
`config_test`), then:

```sh
PATCH_TEST_DATABASE_URL='postgres://auth_rw:pw@127.0.0.1:5433/config_test?sslmode=disable' \
  go test -tags integration -race -count=1 ./integration/...
```

`integration/run.sh` is the same command from the module directory, for CI:

```sh
PATCH_TEST_DATABASE_URL='…' ./integration/run.sh
```

The test skips when `PATCH_TEST_DATABASE_URL` is unset.
