# Otomo: wiki service

**Scope:** wiki hosting on the VM with Docker Compose.
**Builds on:** `15-allocator-and-edge-plan.md` §3 (the public edge this is served through).

Otomo hosts two kinds of wiki with one framework:

| Site | Content lives in | Served at | Who edits |
|---|---|---|---|
| **Technical wiki**: onboarding, technical design, setup docs for anyone adopting otomo | this repository, `docs/` | `https://<public host>/docs/` | the otomo team, by PR |
| **Game site**: the wiki for the game built on otomo | **the game's own repository**, never this one | `https://<public host>/` | the game team, by pushing to that repo |

Otomo doesn't depend on any particular game. It provides the framework (build, image,
serving, deployment) and reads a **content directory**. It contains no game content and no
game names.

---

## 1. The content contract

A content directory is:

```
_site.yml        site settings (required)
index.md         the home page
*.md, */*.md     pages; any folder layout
assets/…         images and other static files, linked relatively
README.md        optional; excluded from the site (use it for editing instructions)
```

`_site.yml` is a small subset of MkDocs' configuration. The framework owns everything else
(theme, extensions, search, strictness):

```yaml
site_name: Example Wiki           # required
site_description: …               # optional
nav: [...]                        # optional; defaults to the folder layout
copyright: …                      # optional
theme:                            # optional, only these keys
  palette: {primary: indigo, accent: amber}
  logo: assets/logo.png
  favicon: assets/favicon.png
extra: {...}                      # optional, passed through
```

Unknown keys fail the build with a message naming them, so a typo can't silently do nothing.

## 2. The framework (`services/wiki`)

- **Build stage:** Python with pinned `mkdocs` and `mkdocs-material`. `build.py` merges
  the framework's `base.yml` with the content's `_site.yml` and runs `mkdocs build
  --strict`. A broken link, a missing nav page or a bad config **fails the build**, so the
  running site is never replaced by a broken one.
- **Runtime stage:** unprivileged nginx on 8080 serving the static files **under the site's
  base path** (`BASE_PATH`, e.g. `/docs/`). Redirects and relative links then stay correct
  behind the edge, which forwards the path unchanged.
- **Privacy and safety:** no cookies, no external requests (no web fonts, no analytics), a
  strict CSP, `X-Content-Type-Options`, and no `Server` version. Search runs in the browser.
- The content is a **named build context** (`content`), so the same image recipe builds
  any site: compose passes `../docs` for the technical wiki and `SITE_CONTENT_DIR` for the
  game site.
- `services/wiki/example/` is a minimal, game-neutral content directory used by tests and as
  a template for adopters.

## 3. Deployment

| Compose service | Profile | Content | Image tag |
|---|---|---|---|
| `wiki-docs` | `edge` | `../docs` (deployed from its own sparse checkout, `DOCS_CONTENT_DIR`) | commit SHA of this repo |
| `wiki-site` | `site` | `SITE_CONTENT_DIR` (a checkout of the game's repo) | commit SHA of the content repo |

Both are on `otomo-edge` only. The edge routes `/docs/` to `wiki-docs` and, when
`EDGE_SITE_URL` is set, everything that isn't the player API or `/docs/` to `wiki-site`.

### Automatic deploys of the technical wiki

A push to `staging` that touches `docs/` or `services/wiki/` makes the dispatcher start the
Jenkins job `otomo-wiki-docs` (`ci/wiki-docs/Jenkinsfile`). That job runs
`deploy/docs/deploy-docs.sh` as the deploy user, which rebuilds from the tip of `staging`.
Setup and the reasons for the separate checkout: `deploy/docs/README.md`.

### Automatic deploys of the game site

A push to the content repository's `main` branch redeploys the site:

```
content repo ──push──► GitHub webhook ──► :5009 webhook proxy ──► Jenkins job `otomo-site-deploy`
                                                   (Generic Webhook Trigger, own token, main only)
      └── sudo -u <deploy user> deploy/site/deploy-site.sh   (the only command Jenkins may run as that user)
            1. git fetch + reset the content checkout to origin/main (read-only deploy key)
            2. build wiki-site (mkdocs --strict); on failure stop, and the old site keeps serving
            3. push to the local registry as otomo-wiki-site:<content sha>, record the tag in deploy/.env
            4. recreate wiki-site and wait for it to answer
```

- The webhook carries **no content**. It only says "main moved", and the host fetches from
  GitHub itself with a read-only deploy key. A forged webhook can at most redeploy what's
  already on `main`.
- Rollback is a re-point, like every other service: set `OTOMO_WIKI_SITE_TAG` to the previous
  SHA and `up -d wiki-site`.

### Why the game repo doesn't include otomo (no submodule)

Otomo consumes the content directory; the content repo doesn't consume otomo.

- The game team edits Markdown and never manages a submodule pointer.
- The content repo needs no access to the private otomo repo.
- The framework can be upgraded for every site from one place.

The dependency is one-way: `_site.yml` is the whole interface. To preview locally, run
`services/wiki/preview.sh <content dir>` from an otomo checkout. It runs the same pinned
builder with live reload.

## 4. Tasks

| # | Task | Done when |
|---|---|---|
| W-1 | `services/wiki`: builder, runtime image, `_site.yml` validation, example content, preview script | Example builds `--strict`; a broken link fails the build; the image serves under `/docs/` and `/` |
| W-2 | Technical wiki content: `docs/_site.yml`, landing page, onboarding and adopter guides, nav over docs 00–16 | `wiki-docs` builds strict; `https://<host>/docs/` serves it |
| W-3 | Compose (`wiki-docs`, `wiki-site`), edge routing under the base path, check-compose | check-compose passes; the edge serves both |
| W-4 | `deploy/site/deploy-site.sh`, the Jenkins job, deploy key and webhook | A push to the content repo's `main` is live within a minute; a broken push leaves the old site up |
