# Site deploys

The optional **site** (compose service `wiki-site`, served at `/` by the edge) is built from
a content repository that belongs to your game, not to otomo (`design/16-wiki-service.md`).
`deploy-site.sh` redeploys it from that repository's branch tip:

```
fetch + reset the content checkout ─► build (mkdocs --strict) ─► push <sha> ─► recreate ─► /healthz
```

- The VM fetches from GitHub itself, with a **read-only deploy key** if the content
  repository is private. Whatever triggers a deploy carries no content, so a forged trigger
  can at most redeploy what's already on the branch.
- A build that fails (a broken link, bad `_site.yml`) stops before anything is replaced, and
  the running site keeps serving.
- Each deploy is tagged with the content commit (`OTOMO_WIKI_SITE_TAG` in `deploy/.env`).
  Rollback is a re-point: set the previous SHA and run `docker compose … up -d wiki-site`.

## One-time setup on the VM

```sh
# 1. A checkout of the content repo. For a private repo, first make a read-only
#    deploy key and add its .pub to the repo under Settings → Deploy keys:
#      ssh-keygen -t ed25519 -N '' -f ~/.ssh/site_deploy -C 'read-only site deploy'
#    and in ~/.ssh/config:  Host github-site / HostName github.com / User git /
#                           IdentityFile ~/.ssh/site_deploy / IdentitiesOnly yes
git clone git@github-site:<owner>/<content-repo>.git ~/sites/<content-repo>

# 2. deploy/.env
#      SITE_CONTENT_DIR=/home/<you>/sites/<content-repo>
#      SITE_CONTENT_BRANCH=main
#      COMPOSE_PROFILES=edge,site
#      EDGE_SITE_URL=http://wiki-site:8080
deploy/site/deploy-site.sh          # first deploy by hand
docker compose -f deploy/compose.yaml up -d edge   # pick up EDGE_SITE_URL
```

## Redeploying on every push

Run `deploy/site/deploy-site.sh` whenever the content repository's branch changes. Any of
these works:

- **By hand**, after merging.
- **A cron job** every few minutes. A commit that's already live is a no-op.
- **A webhook into your CI.** `jenkins-job.xml` is a Jenkins job for this (Generic Webhook
  Trigger plugin): fill in `__TOKEN__` (a fresh random token), `__DEPLOY_USER__` and
  `__DEPLOY_DIR__`, install it as `jobs/otomo-site-deploy/config.xml`, let the Jenkins user
  run exactly that script as the deploy user via sudoers, and point a GitHub push webhook at
  `http://<host>:<port>/generic-webhook-trigger/invoke?token=<token>`.
