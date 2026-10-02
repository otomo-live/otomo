# Wikis and sites

Otomo hosts two websites for your game, built the same way:

| Site | Its pages live in | Address | Who edits it |
|---|---|---|---|
| **This documentation** (optional) | otomo's repository, folder `docs/` | `https://<your domain>/docs/` | Otomo's maintainers, through pull requests on GitHub |
| **Your game's website** | **your game's own repository**, never otomo's | `https://<your domain>/` | Your game team, by pushing to that repository |

Both are written in **Markdown** and turned into web pages by a tool called **MkDocs**
(with the **Material** theme, which gives the look you see here).

## Markdown in two minutes

Markdown is plain text with a few symbols for formatting. You write it in any text editor,
and it reads well even before it becomes a web page:

```markdown
# A page title

## A section heading

Normal text. **Bold**, *italic*, and `code`.

- A bullet point
- Another one

1. A numbered step
2. The next step

[A link to another page](../guide/install-the-sdk.md)

| A | Table |
|---|---|
| with | rows |
```

Two extras this wiki uses a lot:

```markdown
!!! warning "A title for the box"
    Indented text inside a coloured box. Also: note, tip, danger.

=== "Tab one"
    Content shown when the first tab is selected.

=== "Tab two"
    Content of the second tab.
```

## How a site is organised

A site is a folder like this:

```text
_site.yml        the site's name, menu and colours (required)
index.md         the home page
guide/…md        pages, in any folders you like
assets/…         images and other files, linked from pages
README.md        notes for editors; never shown on the site
```

`_site.yml` is short:

```yaml
site_name: Example Wiki          # required
site_description: What this is   # optional
nav:                             # optional: the menu. Without it, the folders are used.
  - Home: index.md
  - Guides:
      - First steps: guide/first-steps.md
theme:                           # optional: only these three settings
  palette: {primary: indigo, accent: amber}
  logo: assets/logo.png
  favicon: assets/favicon.png
```

Any other setting makes the build fail with a message naming it, so a typo can't silently
do nothing. `services/wiki/example/` in otomo's repository is a small working site to copy
as a starting point.

## Previewing your changes

With otomo's repository and Docker on your computer, you can see your site exactly as it
will look, updating as you save:

```sh
services/wiki/preview.sh <path to the site folder>
```

Then open `http://localhost:8000` in your browser. To preview this wiki, use
`services/wiki/preview.sh ../docs`.

## Publishing

**This documentation:** on your VM, `deploy/docs/deploy-docs.sh` builds it from otomo's
repository and serves it at `/docs/` (`deploy/docs/README.md` sets it up). To suggest a
change to the pages themselves, open a pull request on GitHub.

**Your game's website:** push to your game repository's `main` branch, then run
`deploy/site/deploy-site.sh` on the VM, or set it up to run on every push (by cron or a
webhook into your CI; `deploy/site/README.md` shows how). It fetches the new content,
builds it, and swaps it in.

The swap only happens when the build succeeds, so a broken page never replaces a working
site.

## What makes a build fail

The build is **strict**: anything that would give readers a broken page stops it.

- A link to a page that doesn't exist (check the path and spelling; links are relative to
  the page they're in).
- A link to a heading that doesn't exist.
- A page listed in the `nav` menu that doesn't exist.
- An unknown setting in `_site.yml`.

When a build fails, the old version of the site stays up, and the deploy script's output
names the problem.

## Going back to an earlier version

Each published version is kept, tagged with the commit it came from. To return to one, point the
site at the older tag (`deploy/site/README.md`). The design document
`design/16-wiki-service.md` in otomo's repository has the details.
