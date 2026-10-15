#!/usr/bin/env bash
# Acceptance check for the connect page build: it must consist of exactly the
# three allow-listed files, stay small, load nothing from other origins and
# contain none of the preview / gallery / media / Office components the
# upstream web UI bundles.
#
#   scripts/check-bundle.sh                 checks src/server/connect/web
#   scripts/check-bundle.sh http://host:5244   checks what a running OpenList serves
set -euo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
allowed="index.html app.js app.css"
dir="$(mktemp -d)"
trap 'rm -rf "$dir"' EXIT

if [ $# -ge 1 ]; then
  base="${1%/}"
  # The page itself is served at /@connect/ only.
  curl -fsS -o "$dir/index.html" "$base/@connect/"
  for f in app.js app.css; do
    curl -fsS -o "$dir/$f" "$base/@connect/$f"
  done
  # Anything else under /@connect/ must not exist.
  for probe in index.html mock.js main.js assets/index.js static/js/main.js; do
    code="$(curl -s -o /dev/null -w '%{http_code}' "$base/@connect/$probe")"
    if [ "$code" != 404 ]; then
      echo "FAIL: $base/@connect/$probe answered $code (expected 404)"; exit 1
    fi
  done
else
  src="$here/src/server/connect/web"
  extra="$(cd "$src" && find . -type f | sed 's|^\./||' | sort | tr '\n' ' ')"
  if [ "$extra" != "app.css app.js index.html " ]; then
    echo "FAIL: page directory holds [$extra], expected exactly: $allowed"; exit 1
  fi
  cp "$src"/* "$dir/"
fi

fail=0
size=$(cat "$dir"/* | wc -c)
if [ "$size" -gt 102400 ]; then
  echo "FAIL: page is $size bytes (limit 100 KiB)"; fail=1
fi
# Libraries the upstream UI uses for previews, galleries, players and Office,
# and the elements a preview or player would need.
forbidden='pdfjs|pdf\.worker|marked\.(parse|js)|markdown-it|highlight\.js|hljs|monaco|codemirror|prismjs|lightgallery|viewerjs|photoswipe|artplayer|aplayer|dplayer|hls\.js|flv\.js|mpegts|plyr|video\.js|libass|docx-preview|mammoth|sheetjs|katex|mermaid'
if grep -niE "$forbidden" "$dir"/* ; then
  echo "FAIL: preview/media component references found above"; fail=1
fi
if grep -niE "<(video|audio|iframe|embed|object)[ >]|createElement\(['\"](video|audio|iframe|embed|object)['\"]" "$dir"/* ; then
  echo "FAIL: media/embedding elements found above"; fail=1
fi
if grep -nE '<script[^>]+src="(https?:)?//|<link[^>]+href="(https?:)?//|@import|https?://(cdn|fonts|unpkg|jsdelivr)' "$dir"/* ; then
  echo "FAIL: external script/style references found above"; fail=1
fi
if grep -nE '<script>[^<]|on(click|load|error|submit)=' "$dir/index.html" ; then
  echo "FAIL: inline script or handler in index.html"; fail=1
fi
[ "$fail" = 0 ] && echo "connect page bundle OK: $(ls "$dir" | tr '\n' ' ')($size bytes)"
exit "$fail"
