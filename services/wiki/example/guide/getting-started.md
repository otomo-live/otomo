# Getting started

This directory is a content directory: everything under it becomes the site,
except `_site.yml` and `README.md` (see
[Writing pages](writing-pages.md) for the full contract).

## Preview it

From an otomo checkout, preview this directory with live reload:

```sh
services/wiki/preview.sh services/wiki/example
```

The build runs `mkdocs build --strict`, so any broken link or bad `_site.yml`
fails before a broken site can replace a working one.

## Layout

```
_site.yml      site settings (required)
index.md       the home page
guide/…        any folder layout you like
assets/…       images and other static files, linked relatively
```

## Add an asset

Static files live in `assets/` and are linked relatively, so the links keep
working no matter which base path the site is served under:

![The content directory flows into a built site](../assets/diagram.svg)

Next: [Writing pages](writing-pages.md) explains the content contract.
