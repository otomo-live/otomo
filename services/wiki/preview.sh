#!/bin/sh
# Preview an otomo wiki content directory with live reload.
#
# Builds the "build" stage of this image and runs it as a MkDocs dev server,
# publishing http://127.0.0.1:8000/. The content directory is mounted
# read-only at /content, exactly where the image expects it.
#
# Usage: preview.sh <content_dir>

set -eu

usage() {
    echo "usage: $0 <content_dir>" >&2
    echo "  <content_dir>  directory containing _site.yml (e.g. example/)" >&2
    exit 2
}

if [ "$#" -ne 1 ]; then
    usage
fi

content_dir=$1
if [ ! -d "$content_dir" ]; then
    echo "preview: not a directory: $content_dir" >&2
    exit 2
fi

# Docker needs an absolute path for the named build context.
case "$content_dir" in
    /*) ;;
    *) content_dir="$(pwd)/$content_dir" ;;
esac

here=$(CDPATH= cd "$(dirname "$0")" && pwd)

echo "preview: building $here (target build, content=$content_dir)"
docker build \
    -f "$here/dockerfile" \
    --target build \
    --build-context "content=$content_dir" \
    -t otomo-wiki-preview \
    "$here"

echo "preview: serving on http://127.0.0.1:8000/ (Ctrl-C to stop)"
exec docker run --rm -it \
    -p 127.0.0.1:8000:8000 \
    -v "$content_dir":/content:ro \
    otomo-wiki-preview \
    python /wiki/build.py /content /tmp/out --serve
