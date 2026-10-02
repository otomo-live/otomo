# Contributing to otomo

Thanks for your interest. Bug reports, documentation fixes and pull requests are welcome.

## Before you start

- For anything bigger than a small fix, open an issue first so we can agree on the approach.
- Read the design document for the area you're changing in `design/`. Code comments cite
  them (for example `design/14-launch-handoff.md §4.3`), and requirement IDs such as `COM-5`
  (the error envelope) or `SE-8` (release enforcement) are defined there.

## Development

Each Go service is its own module under `services/`, tied together by `services/go.work`.
From a service's folder:

```sh
gofmt -l .        # must print nothing
go vet ./...
go test ./...
```

Tests that need Postgres or Valkey skip themselves unless their `*_TEST_DATABASE_URL` /
`*_TEST_VALKEY_URL` variable is set; `ci/compose.yaml` starts both for local runs, and
`ci/services/<service>/unit_test.sh <level>` runs the same selection CI does.

- Admin website: `services/adminui` (`npm ci`, `npm run lint`, `npm test`).
- Godot SDK: `godot-plugin/tests` (`dotnet test`).
- Documentation: `services/wiki/preview.sh ../docs` from `services/wiki/` serves the docs
  with live reload. The build is strict: a broken link fails it.
- The full stack: `deploy/scripts/generate-secrets.sh && deploy/scripts/up.sh`.

## Pull requests

- Branch from `staging` and open the pull request against `staging`.
- Keep each pull request to one change, with tests for behaviour changes.
- Write the commit message and the pull request description in plain prose: what changed
  and why. Pull requests are squash-merged, so the description becomes the commit message.
- Don't add tool-generated footers or co-author trailers for code assistants to commits or
  pull request descriptions. A check rejects them.
- User-facing changes update `docs/`; contract changes update the matching `design/` doc.

## Code of conduct

Be respectful and constructive. Harassment of any kind isn't tolerated.
