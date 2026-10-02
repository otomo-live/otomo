# otomo wiki service

The framework that builds and serves every otomo wiki: the technical docs under
`/docs/` and a game site under `/`. It is **game-agnostic** — it consumes a
content directory and contains no game content and no game names.

The design is [`design/16-wiki-service.md`](../../design/16-wiki-service.md).

## What it is

| File | Purpose |
|---|---|
| `base.yml` | The MkDocs configuration the framework owns: Material theme without web fonts, search, Markdown extensions, strict validation. |
| `build.py` | Validates and merges a content directory's `_site.yml` into `base.yml`, then runs `mkdocs build --strict` (or `mkdocs serve`). |
| `nginx.conf` | A full, unprivileged nginx config serving static output under `BASE_PATH` on port 8080. |
| `dockerfile` | Two stages: a pinned Python/MkDocs build and an unprivileged nginx runtime. |
| `preview.sh` | Builds and live-previews a content directory on `127.0.0.1:8000`. |
| `example/` | A minimal, game-neutral content directory used as a template. |

## The content contract

A content directory contains `_site.yml` (required), `index.md`, any number of
Markdown pages in any folder layout, and `assets/`. `README.md` is optional and
excluded from the site.

`_site.yml` may set only `site_name` (required, non-empty), `site_description`,
`nav`, `copyright`, `extra`, and the `theme` keys `palette`, `logo` and
`favicon`. Any other key fails the build with the key named. The full contract
is in
[design/16 §1](../../design/16-wiki-service.md#1-the-content-contract);
[`example/guide/writing-pages.md`](example/guide/writing-pages.md) is a working
walkthrough.

## Build contexts and arguments

The content is a Docker **named build context**, so the same recipe builds any
site:

```sh
docker build \
    --build-context content=../docs \
    --build-arg BASE_PATH=/docs/ \
    --build-arg SITE_URL=https://example.com/docs/ \
    -t otomo-wiki .
```

| Argument | Default | Meaning |
|---|---|---|
| `BASE_PATH` | `/` | Path the site is served under. Must be `/` or start and end with `/`; the build fails otherwise. |
| `SITE_URL` | empty | Public URL passed to MkDocs. Omitted entirely when empty. |

`COPY --from=content . /content` is what makes the named context work.

## Preview

From an otomo checkout:

```sh
services/wiki/preview.sh services/wiki/example
```

It builds the `build` stage, mounts the content read-only at `/content`, and
serves it with live reload on <http://127.0.0.1:8000/>. Run it with `sh
preview.sh <content_dir>` if the executable bit is not set.

## Security properties

- Runs as the `nginx-unprivileged` image's non-root user, with pid and temp
  paths under `/tmp`; listens on 8080.
- Never sets a cookie.
- Every response carries a strict `Content-Security-Policy`, plus
  `X-Content-Type-Options: nosniff` and
  `Referrer-Policy: strict-origin-when-cross-origin`.
- No external requests: `font: false` disables Google Fonts, and there are no
  analytics, CDN scripts or CDN styles. Search runs in the browser.
- `server_tokens off` hides the `Server` version.
- `assets/` is cached for seven days; everything else is `Cache-Control:
  no-cache`.
- Broken internal links, missing nav pages and bad `_site.yml` configuration
  fail the strict build, so a broken site never replaces a working one.
