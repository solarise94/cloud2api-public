#!/usr/bin/env bash
# One-shot local deployment: OpenList + cloud2api in rootless Podman.
#
# The containers share a slirp4netns network namespace to avoid requiring
# host bridge and packet-filter configuration. OpenList owns the namespace
# and published ports; cloud2api joins with --network container:openlist and
# reaches OpenList at http://127.0.0.1:5244.
#
# Idempotent redeploy (v0.3.1): an existing ./config.yaml keeps its API key,
# cloud2api-data/admin_password keeps the portal password, and the OpenList
# admin password is persisted in cloud2api-data/openlist_admin_password. On
# redeploys nothing is rotated and no secrets are printed.
#
# Environment:
#   PUBLISH_ADDR             where cloud2api's :8080 is published
#                            (default 0.0.0.0 = LAN access)
#   OPENLIST_PUBLISH_ADDR    where OpenList's :5244 is published; must be
#                            declared on the openlist container because it
#                            owns the netns. Default 127.0.0.1 (admin UI
#                            reachable from this host / SSH tunnel only).
#                            Set to "none" or "" to not publish at all.
#                            A comma-separated list publishes on several
#                            addresses, e.g. "127.0.0.1,192.0.2.10" so
#                            LAN browsers can open the OpenList admin UI.
#   OPENLIST_PUBLIC_URL      the OpenList URL browsers use (portal links to
#                            its admin UI). Default: derived from the first
#                            non-loopback publish address; empty when OpenList
#                            is loopback-only/unpublished (the portal then
#                            asks for it in its settings page).
#   ADMIN_PASS               OpenList admin password for a FIRST install
#                            (or when cloud2api-data/openlist_admin_password
#                            is absent); it is then stored and reused.
#   OPENLIST_IMAGE           OpenList image to run. Default: the patched build
#                            from openlist-patch/ (OpenList engine + the
#                            connect login module), built here when the image
#                            for the pinned version is missing. Set it to
#                            docker.io/openlistteam/openlist:v4.2.6 to run
#                            upstream without the login module.
#   OPENLIST_REBUILD=1       rebuild the patched image even if it exists.
#   SEED_STORAGES=0          do not create the Local test storages /baidu,
#                            /123pan and /aliyun (scripts/acceptance.sh needs
#                            them); use it on a deployment that holds real
#                            drives and no longer wants the test fixtures.
set -euo pipefail
cd "$(dirname "$0")/.."

PUBLISH_ADDR="${PUBLISH_ADDR:-0.0.0.0}"
OPENLIST_PUBLISH_ADDR="${OPENLIST_PUBLISH_ADDR-127.0.0.1}"

if [ -z "${OPENLIST_IMAGE:-}" ]; then
  # shellcheck source=../openlist-patch/UPSTREAM
  . openlist-patch/UPSTREAM
  OPENLIST_IMAGE="localhost/openlist-connect:$OPENLIST_VERSION-connect.$CONNECT_VERSION"
  if [ "${OPENLIST_REBUILD:-0}" = 1 ] || ! podman image exists "$OPENLIST_IMAGE"; then
    echo "==> building patched OpenList ($OPENLIST_IMAGE)"
    openlist-patch/scripts/build-image.sh
  fi
fi

mkdir -p testdata/baidu/research testdata/123pan testdata/aliyun testdata/portal openlist-data cloud2api-data

# --- idempotent secrets -----------------------------------------------------
# Portal password: reuse when the deployment already has one.
if [ -f cloud2api-data/admin_password ]; then
  PORTAL_PASSWORD=$(cat cloud2api-data/admin_password)
  FRESH_PORTAL=0
else
  PORTAL_PASSWORD="${PORTAL_PASSWORD:-$(head -c 24 /dev/urandom | base64 | tr -d '/+=')}"
  FRESH_PORTAL=1
fi

# OpenList admin password: reuse the persisted one; first install accepts
# ADMIN_PASS or generates one, then persists it (chmod 600).
if [ -f cloud2api-data/openlist_admin_password ]; then
  ADMIN_PASS=$(cat cloud2api-data/openlist_admin_password)
  FRESH_OLPASS=0
else
  ADMIN_PASS="${ADMIN_PASS:-$(head -c 24 /dev/urandom | base64 | tr -d '/+=')}"
  FRESH_OLPASS=1
fi

echo "==> building cloud2api image"
podman build -f Containerfile -t cloud2api:dev .

echo "==> preparing testdata"
[ -f testdata/baidu/hello.txt ] || echo "hello from baidu" > testdata/baidu/hello.txt
[ -f testdata/baidu/research/data.csv ] || echo "research dataset v1" > testdata/baidu/research/data.csv
[ -f testdata/123pan/hello.txt ] || echo "hello from 123pan" > testdata/123pan/hello.txt
[ -f testdata/aliyun/secret.txt ] || echo "aliyun secret file" > testdata/aliyun/secret.txt
echo "portal test root" > testdata/portal/portal.txt
# openlist runs as uid 1001 in-container; make volumes writable by it and
# still writable by the host user for test fixtures.
podman unshare chown -R 1001:1001 openlist-data testdata
podman unshare chmod -R 0777 testdata

# OpenList's port must be declared on the openlist container (it owns the
# shared netns). Default: loopback only, so the OpenList admin UI is
# reachable from this machine (or an SSH tunnel) but not from the LAN.
OL_PUBLISH=()
OL_PUBLIC_DERIVED=""
if [ -n "$OPENLIST_PUBLISH_ADDR" ] && [ "$OPENLIST_PUBLISH_ADDR" != "none" ]; then
  IFS=',' read -r -a OL_ADDRS <<< "$OPENLIST_PUBLISH_ADDR"
  for a in "${OL_ADDRS[@]}"; do
    OL_PUBLISH+=(-p "$a:5244:5244")
    case "$a" in
      127.*|localhost|::1) ;;
      0.0.0.0) [ -n "$OL_PUBLIC_DERIVED" ] || OL_PUBLIC_DERIVED="http://$(hostname -I | awk '{print $1}'):5244" ;;
      *) [ -n "$OL_PUBLIC_DERIVED" ] || OL_PUBLIC_DERIVED="http://$a:5244" ;;
    esac
  done
fi
OPENLIST_PUBLIC_URL="${OPENLIST_PUBLIC_URL-$OL_PUBLIC_DERIVED}"
C2A_ENV=()
if [ -n "$OPENLIST_PUBLIC_URL" ]; then
  C2A_ENV=(-e C2A_OPENLIST_PUBLIC_URL="$OPENLIST_PUBLIC_URL")
fi

echo "==> starting openlist ($OPENLIST_IMAGE; publish: ${OPENLIST_PUBLISH_ADDR:-none})"
# Removing a running container can race its shutdown; retry once.
podman rm -f openlist cloud2api 2>/dev/null || true
podman rm -f openlist cloud2api 2>/dev/null || true
podman run -d --name openlist --network slirp4netns --memory=512m \
  -p "$PUBLISH_ADDR:8080:8080" \
  ${OL_PUBLISH[@]+"${OL_PUBLISH[@]}"} \
  -v "$PWD/openlist-data:/opt/openlist/data:Z" \
  -v "$PWD/testdata/baidu:/mnt/baidu:Z" \
  -v "$PWD/testdata/123pan:/mnt/123pan:Z" \
  -v "$PWD/testdata/aliyun:/mnt/aliyun:Z" \
  -v "$PWD/testdata/portal:/mnt/portal-test:Z" \
  "$OPENLIST_IMAGE" >/dev/null
sleep 10
podman exec openlist ./openlist admin set "$ADMIN_PASS" >/dev/null

ol() { # ol <method> <path> <json-body-or-empty>
  podman run --rm --network container:openlist -e TOKEN="$OPENLIST_TOKEN" \
    -e BODY="${3:-}" docker.io/library/alpine:3.20 sh -c '
      if [ -n "$BODY" ]; then
        wget -q -O- --post-data="$BODY" --header="Content-Type: application/json" --header="Authorization: $TOKEN" "http://127.0.0.1:5244$0"
      else
        wget -q -O- --header="Authorization: $TOKEN" "http://127.0.0.1:5244$0"
      fi' "$2"
}

echo "==> openlist login"
OPENLIST_TOKEN=$(podman run --rm --network container:openlist docker.io/library/alpine:3.20 sh -c \
  "wget -q -O- --post-data='{\"username\":\"admin\",\"password\":\"$ADMIN_PASS\"}' --header='Content-Type: application/json' http://127.0.0.1:5244/api/auth/login" \
  | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$OPENLIST_TOKEN" ] || { echo "login failed"; exit 1; }

# v0.3.1: use OpenList's PERMANENT settings token ("其他设置 → 令牌"), not the
# 48h login JWT, as OPENLIST_TOKEN. The JWT is only used to read the setting.
PERM_TOKEN=$(podman run --rm --network container:openlist -e T="$OPENLIST_TOKEN" docker.io/library/alpine:3.20 sh -c \
  'wget -q -O- --header="Authorization: $T" "http://127.0.0.1:5244/api/admin/setting/get?key=token"' \
  | sed -n 's/.*"value":"\([^"]*\)".*/\1/p')
if [ -n "$PERM_TOKEN" ]; then
  OPENLIST_TOKEN="$PERM_TOKEN"
  echo "    using permanent settings token"
else
  echo "    WARNING: could not read the permanent settings token; falling back to the login JWT (expires in 48h)" >&2
fi

if [ "${SEED_STORAGES:-1}" = 0 ]; then
  echo "==> SEED_STORAGES=0: not creating the Local test storages"
else
  echo "==> creating Local storages (simulated netdisks)"
  for spec in baidu 123pan aliyun; do
    ol POST /api/admin/storage/create "{\"mount_path\":\"/$spec\",\"order\":0,\"driver\":\"Local\",\"cache_expiration\":30,\"status\":\"work\",\"addition\":\"{\\\"root_folder_path\\\":\\\"/mnt/$spec\\\",\\\"thumbnail\\\":false}\"}" | grep -q '"code":200' \
      && echo "    /$spec ok" || echo "    /$spec already exists or failed"
  done
fi

# --- config.yaml: keep an existing one (never rotate a key in use) ----------
if [ -f config.yaml ]; then
  echo "==> reusing existing config.yaml (API key unchanged)"
else
  echo "==> generating API key and config.yaml"
  podman run --rm localhost/cloud2api:dev genkey > /tmp/cloud2api_key.txt
  PLAIN=$(sed -n 1p /tmp/cloud2api_key.txt)
  HASH=$(sed -n 2p /tmp/cloud2api_key.txt)
  # Shared netns: cloud2api reaches OpenList on the loopback of the common
  # network namespace.
  sed -e "s|key_hash: .*|key_hash: \"$HASH\"|" \
      -e "s|endpoint: .*|endpoint: http://127.0.0.1:5244|" \
      config.example.yaml > config.yaml
fi

echo "==> starting cloud2api (tokens via env, state volume for the portal)"
# OPENLIST_USERNAME/OPENLIST_PASSWORD enable the in-process token self-heal
# (re-login + permanent token swap + retry when OpenList answers 401).
podman run -d --name cloud2api --network container:openlist \
  -e OPENLIST_TOKEN="$OPENLIST_TOKEN" \
  -e OPENLIST_USERNAME="admin" \
  -e OPENLIST_PASSWORD="$ADMIN_PASS" \
  -e ADMIN_PASSWORD="$PORTAL_PASSWORD" \
  "${C2A_ENV[@]}" \
  -v "$PWD/config.yaml:/etc/cloud2api/config.yaml:ro,Z" \
  -v "$PWD/cloud2api-data:/var/lib/cloud2api:Z" \
  cloud2api:dev serve -config /etc/cloud2api/config.yaml >/dev/null
sleep 2

# Persist secrets for later runs and acceptance.sh (never printed on reuse).
umask 077
if [ "$FRESH_PORTAL" = 1 ]; then printf '%s\n' "$PORTAL_PASSWORD" > cloud2api-data/admin_password; fi
if [ "$FRESH_OLPASS" = 1 ]; then printf '%s\n' "$ADMIN_PASS" > cloud2api-data/openlist_admin_password; fi
chmod 600 cloud2api-data/admin_password cloud2api-data/openlist_admin_password 2>/dev/null || true

echo
if [ -n "${PLAIN:-}" ]; then
  echo "deployed. plaintext API key (shown once): $PLAIN"
fi
if [ "$FRESH_PORTAL" = 1 ]; then
  echo "portal admin password (shown once): $PORTAL_PASSWORD  (saved to cloud2api-data/admin_password)"
fi
if [ "$FRESH_OLPASS" = 1 ]; then
  echo "openlist admin password (shown once): $ADMIN_PASS  (saved to cloud2api-data/openlist_admin_password)"
fi
echo "portal: http://${PUBLISH_ADDR}:8080/admin/"
if [ ${#OL_PUBLISH[@]} -gt 0 ]; then
  echo "openlist admin ui: published on ${OPENLIST_PUBLISH_ADDR} (port 5244)"
  if [ -n "$OPENLIST_PUBLIC_URL" ]; then
    echo "openlist public url (portal links): ${OPENLIST_PUBLIC_URL}/@manage"
  fi
else
  echo "openlist admin ui: not published (OPENLIST_PUBLISH_ADDR=none)"
fi
if [ -n "${PLAIN:-}" ]; then
  echo "run acceptance with:"
  echo "  CLOUD2API_KEY='$PLAIN' bash scripts/acceptance.sh"
else
  echo "redeploy kept the existing API key; run acceptance with:"
  echo "  CLOUD2API_KEY='<your existing key>' bash scripts/acceptance.sh"
fi
