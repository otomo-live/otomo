#!/usr/bin/env python3
"""Build or serve an otomo wiki from a content directory.

Usage:
    build.py <content_dir> <out_dir> [--site-url URL] [--serve]

The framework owns base.yml: the Material theme (without web fonts), search,
Markdown extensions and MkDocs' strict validation. A content directory owns
_site.yml, a small allow-listed subset of MkDocs configuration. This script
validates _site.yml, deep-merges it into base.yml, writes the merged config to a
temporary mkdocs.yml and runs MkDocs against it.

The framework is game-agnostic: it knows about content directories, never about
any particular game.
"""

from __future__ import annotations

import argparse
import os
import subprocess
import sys
import tempfile

import yaml

HERE = os.path.dirname(os.path.abspath(__file__))
BASE_CONFIG = os.path.join(HERE, "base.yml")
CONTENT_FILE = "_site.yml"
PREVIEW_ADDRESS = "0.0.0.0:8000"

# The whole content contract: _site.yml may set these top-level keys and these
# theme keys, and nothing else.
ALLOWED_KEYS = ("site_name", "site_description", "nav", "copyright", "extra", "theme")
ALLOWED_THEME_KEYS = ("palette", "logo", "favicon")


def fail(message: str) -> int:
    """Print a prefixed error to stderr and return the config-error exit code."""
    print(f"wiki: {message}", file=sys.stderr)
    return 2


def report(messages) -> None:
    """Print each validation message on its own prefixed line."""
    for message in messages:
        print(f"wiki: {message}", file=sys.stderr)


def load_yaml(path: str):
    with open(path, "r", encoding="utf-8") as handle:
        return yaml.safe_load(handle)


def deep_merge(base, override):
    """Merge override into base; override wins for scalars and lists."""
    if isinstance(base, dict) and isinstance(override, dict):
        merged = dict(base)
        for key, value in override.items():
            merged[key] = deep_merge(merged[key], value) if key in merged else value
        return merged
    return override


def validate_content(content):
    """Return a list of problems with _site.yml; empty means it is valid."""
    if not isinstance(content, dict):
        return ["_site.yml: top level must be a mapping"]

    errors = [
        f"_site.yml: unknown key '{key}' (allowed: {', '.join(ALLOWED_KEYS)})"
        for key in content
        if key not in ALLOWED_KEYS
    ]

    site_name = content.get("site_name")
    if not isinstance(site_name, str) or not site_name.strip():
        errors.append("_site.yml: 'site_name' is required and must be a non-empty string")

    theme = content.get("theme")
    if theme is not None:
        if not isinstance(theme, dict):
            errors.append("_site.yml: 'theme' must be a mapping")
        else:
            errors.extend(
                f"_site.yml: unknown key 'theme.{key}' "
                f"(allowed: {', '.join(ALLOWED_THEME_KEYS)})"
                for key in theme
                if key not in ALLOWED_THEME_KEYS
            )

    return errors


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(
        prog="build.py",
        description="Build or serve an otomo wiki from a content directory.",
    )
    parser.add_argument("content_dir", help="content directory containing _site.yml")
    parser.add_argument("out_dir", help="directory for the built site")
    parser.add_argument("--site-url", default=None, help="public base URL of the site")
    parser.add_argument(
        "--serve",
        action="store_true",
        help="run a live-reload server on 0.0.0.0:8000 instead of building",
    )
    args = parser.parse_args(argv)

    content_dir = os.path.abspath(args.content_dir)
    out_dir = os.path.abspath(args.out_dir)
    site_file = os.path.join(content_dir, CONTENT_FILE)

    if not os.path.isdir(content_dir):
        return fail(f"content directory not found: {content_dir}")
    if not os.path.isfile(site_file):
        return fail(f"_site.yml: not found in {content_dir}")
    try:
        content = load_yaml(site_file)
    except yaml.YAMLError as exc:
        return fail(f"_site.yml: invalid YAML: {exc}")

    errors = validate_content(content)
    if errors:
        report(errors)
        return 2

    try:
        base = load_yaml(BASE_CONFIG)
    except (OSError, yaml.YAMLError) as exc:
        return fail(f"framework config unreadable ({BASE_CONFIG}): {exc}")
    if not isinstance(base, dict):
        return fail(f"framework config is not a mapping: {BASE_CONFIG}")

    # The content already passed the allow-list, so this merge can only touch
    # the keys the contract allows. theme.name, theme.features, plugins,
    # markdown_extensions and validation therefore always come from base.yml.
    config = deep_merge(base, content)
    config["docs_dir"] = content_dir
    config["site_dir"] = out_dir
    if args.site_url:
        config["site_url"] = args.site_url

    with tempfile.TemporaryDirectory(prefix="otomo-wiki-") as tmp:
        config_path = os.path.join(tmp, "mkdocs.yml")
        with open(config_path, "w", encoding="utf-8") as handle:
            yaml.safe_dump(config, handle, sort_keys=False, allow_unicode=True)

        if args.serve:
            print(f"wiki: serving {content_dir} at http://{PREVIEW_ADDRESS} (not strict)")
            command = [
                sys.executable, "-m", "mkdocs", "serve",
                "-a", PREVIEW_ADDRESS,
                "--config-file", config_path,
            ]
        else:
            print(f"wiki: building {content_dir} -> {out_dir} (strict)")
            command = [
                sys.executable, "-m", "mkdocs", "build",
                "--strict",
                "--config-file", config_path,
            ]

        try:
            result = subprocess.run(command)
        except OSError as exc:
            return fail(f"could not run MkDocs: {exc}")

    if result.returncode < 0:
        return 128 - result.returncode
    return result.returncode


if __name__ == "__main__":
    sys.exit(main())
