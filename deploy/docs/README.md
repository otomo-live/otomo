# Serving the documentation on your VM (optional)

The compose service `wiki-docs` serves this repository's `docs/` at
`https://<your domain>/docs/`, built with the framework in `services/wiki/`. It's optional:
the same pages can be read on GitHub.

`deploy-docs.sh` builds and deploys it:

```
fetch + reset the docs checkout to DOCS_BRANCH ─► build (mkdocs --strict)
  ─► push otomo-wiki-docs:<sha> ─► recreate wiki-docs ─► /healthz
```

- **Separate checkout.** The build uses its own sparse clone (`DOCS_CONTENT_DIR`: only
  `docs/` and `services/wiki/`), never the deploy checkout, so publishing docs can't move
  the deploy files every other service runs from.
- **Always the branch tip.** It deploys the tip of `DOCS_BRANCH`, whatever you ran it for,
  so runs that finish out of order can't roll the site back. A commit that's already live
  is a no-op, so it's safe to run on a schedule.
- **A failed build changes nothing.** A broken link, a missing nav page or a bad
  `docs/_site.yml` fails the run, and the running site keeps serving.
- Rollback is a re-point: set `OTOMO_WIKI_DOCS_TAG` in `deploy/.env` to the previous SHA
  and run `docker compose … up -d --no-build wiki-docs`.

## One-time setup

There are **two checkouts** of this repository on the VM. Don't mix them up:

| Checkout | Example path | What it's for |
|---|---|---|
| **Deploy checkout** | `~/otomo` | Runs the stack. Its `deploy/.env` holds the settings, and its `deploy/docs/deploy-docs.sh` is the script you run |
| **Docs checkout** | `~/sites/otomo-docs` | Only the source the site is built from: a sparse clone with `docs/` and `services/wiki/`. No `.env`, and never run anything from it |

```sh
# 1. The docs checkout: exactly docs/ and services/wiki/.
git clone --filter=blob:none --no-checkout https://github.com/otomo-live/otomo.git ~/sites/otomo-docs
git -C ~/sites/otomo-docs sparse-checkout set docs services/wiki
git -C ~/sites/otomo-docs checkout main

# 2. The settings, in the DEPLOY checkout's .env (use absolute paths).
cat >> ~/otomo/deploy/.env <<ENV
DOCS_CONTENT_DIR=$HOME/sites/otomo-docs
DOCS_BRANCH=main
ENV

# 3. Deploy. "already live" is a success.
~/otomo/deploy/docs/deploy-docs.sh
```

The script refuses to run if `DOCS_CONTENT_DIR` still contains a `<placeholder>`, or if it's
started from inside the docs checkout.

To keep the docs in step with new releases, run step 3 after each `git pull` of the deploy
checkout, or from a daily cron job (`crontab -e`):

```cron
0 4 * * * $HOME/otomo/deploy/docs/deploy-docs.sh >> $HOME/deploy-docs.log 2>&1
```
