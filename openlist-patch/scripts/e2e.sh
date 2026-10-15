#!/usr/bin/env bash
# End-to-end test of the connect module against a throwaway patched OpenList
# built with the example adapter (EXAMPLE=1 build-image.sh). The example
# adapter connects the instance to itself through the upstream "OpenList"
# driver, so login → create → list/upload/download/delete → re-authorization
# and every failure path run without an external account.
#
#   openlist-patch/scripts/e2e.sh
#
# Environment: E2E_IMAGE (default the -example tag of this patch version,
# built when missing), E2E_PORT (default 15244), KEEP=1 keeps the container
# and its data directory for inspection.
set -euo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=../UPSTREAM
. "$here/UPSTREAM"

image="${E2E_IMAGE:-localhost/openlist-connect:$OPENLIST_VERSION-connect.$CONNECT_VERSION-example}"
port="${E2E_PORT:-15244}"
name=openlist-connect-e2e
base="http://127.0.0.1:$port"

if ! podman image exists "$image"; then
  echo "==> building $image"
  EXAMPLE=1 "$here/scripts/build-image.sh"
fi

cache="${XDG_CACHE_HOME:-$HOME/.cache}/openlist-connect"
mkdir -p "$cache"
data="$(mktemp -d "$cache/e2e.XXXXXX")"
cleanup() {
  if [ "${KEEP:-0}" = 1 ]; then
    echo "kept: container $name, data $data"
    return
  fi
  podman rm -f "$name" >/dev/null 2>&1 || true
  podman unshare rm -rf "$data" 2>/dev/null || rm -rf "$data"
}
trap cleanup EXIT

mkdir -p "$data/files/sub"
echo "hello from e2e" > "$data/files/hello.txt"
podman unshare chown -R 1001:1001 "$data"

pass="e2e-admin-$(head -c 12 /dev/urandom | base64 | tr -d '/+=')"
pass2="e2e-user-$(head -c 12 /dev/urandom | base64 | tr -d '/+=')"
# Rejected logins use a disabled account: with a wrong password the upstream
# OpenList driver retries /auth/login until OpenList's rate limiter locks the
# client IP (here the container's own 127.0.0.1) for five minutes.
pass3="e2e-off-$(head -c 12 /dev/urandom | base64 | tr -d '/+=')"

echo "==> starting $image on $base"
podman rm -f "$name" >/dev/null 2>&1 || true
podman run -d --name "$name" --network slirp4netns -p "127.0.0.1:$port:5244" \
  -v "$data:/opt/openlist/data:Z" "$image" >/dev/null
for _ in $(seq 1 60); do
  curl -fsS "$base/ping" >/dev/null 2>&1 && break
  sleep 1
done
podman exec "$name" ./openlist admin set "$pass" >/dev/null

fails=0
ok() { echo "  ok    $*"; }
bad() { echo "  FAIL  $*"; fails=$((fails + 1)); }
# check <description> <jq predicate> <json>
check() {
  if jq -e "$2" >/dev/null 2>&1 <<<"$3"; then
    ok "$1"
  else
    bad "$1"
    # A session view is long; show the parts that explain a failure.
    printf '        %s\n' "$(jq -c 'if (.data|type)=="object" and (.data.phase!=null)
      then {code, phase: .data.phase, error: .data.error, result: .data.result}
      else . end' 2>/dev/null <<<"$3" | head -c 1200 || head -c 800 <<<"$3")"
  fi
}
# hasnt <description> <needle> <haystack>
hasnt() {
  if grep -qF -- "$2" <<<"$3"; then bad "$1"; else ok "$1"; fi
}
tok=""
api() { # api METHOD PATH [JSON]
  if [ $# -ge 3 ]; then
    curl -sS -X "$1" "$base$2" -H "Authorization: $tok" -H 'Content-Type: application/json' -d "$3"
  else
    curl -sS -X "$1" "$base$2" -H "Authorization: $tok"
  fi
}
login() { # login USER PASSWORD → JWT
  local h
  h="$(printf '%s-https://github.com/alist-org/alist' "$2" | sha256sum | cut -d' ' -f1)"
  curl -sS -X POST "$base/api/auth/login/hash" -H 'Content-Type: application/json' \
    -d "$(jq -nc --arg u "$1" --arg p "$h" '{username:$u, password:$p}')" | jq -r '.data.token // empty'
}
start() { api POST /api/admin/connect/session/start "$1"; }
submit() { # submit SESSION USER PASSWORD [URL]
  local values
  if [ $# -ge 4 ]; then
    values="$(jq -nc --arg u "$2" --arg p "$3" --arg url "$4" '{url:$url, username:$u, password:$p}')"
  else
    values="$(jq -nc --arg u "$2" --arg p "$3" '{username:$u, password:$p}')"
  fi
  api POST /api/admin/connect/session/submit "$(jq -nc --arg id "$1" --argjson v "$values" '{id:$id, values:$v}')"
}
save() { # save SESSION MOUNT ROOT REMARK
  api POST /api/admin/connect/session/save \
    "$(jq -nc --arg id "$1" --arg m "$2" --arg r "$3" --arg k "$4" '{id:$id, mount_path:$m, root:$r, remark:$k}')"
}
storage() { api GET "/api/admin/storage/get?id=$1"; }
count_mount() {
  api GET "/api/admin/storage/list?page=1&per_page=500" | jq --arg m "$1" '[.data.content[] | select(.mount_path==$m)] | length'
}
self="http://127.0.0.1:5244"

tok="$(login admin "$pass")"
[ -n "$tok" ] || { echo "admin login failed"; exit 1; }

echo "== module and page"
plat="$(api GET /api/admin/connect/platforms)"
check "platforms: six bundled adapters in order, then the example" \
  '.code==200 and ([.data.platforms[].id]==["baidu","aliyun","123pan","onedrive","googledrive","goapi","example-openlist"])' "$plat"
check "module reports upstream $OPENLIST_VERSION and connect $CONNECT_VERSION" \
  ".data.module.upstream_version==\"$OPENLIST_VERSION\" and .data.module.version==\"$CONNECT_VERSION\"" "$plat"
check "without saved apps every platform uses its default mode" \
  '[.data.platforms[] | {id, m: .app.mode, d: (.app.default // false), c: .app.configured, a: .active_method}]
   == [{id:"baidu",m:"public_app",d:true,c:true,a:"qrcode"},
       {id:"aliyun",m:"online_service",d:true,c:true,a:"qrcode"},
       {id:"123pan",m:"none",d:false,c:true,a:"form"},
       {id:"onedrive",m:"online_service",d:true,c:true,a:"form"},
       {id:"googledrive",m:"online_service",d:true,c:true,a:"form"},
       {id:"goapi",m:"none",d:false,c:true,a:"form"},
       {id:"example-openlist",m:"none",d:false,c:true,a:"form"}]' "$plat"
check "OAuth stays available as the own-app mode" \
  '[.data.platforms[] | select(.id=="baidu" or .id=="onedrive" or .id=="googledrive") | .mode_methods.own_app] == ["oauth","oauth","oauth"]' "$plat"
hasnt "platforms response carries no public app secret" "SVT6xpMdLcx6v4aCR4wT8BBOTbzFO8LM" "$plat"
bad_app="$(api POST /api/admin/connect/app/save '{"platform":"baidu","mode":"own_app","client_id":"","client_secret":""}')"
check "own app without client_id is rejected" '.code==400 and .data.error_code=="app_invalid"' "$bad_app"
hdr="$(curl -sS -D - -o /dev/null "$base/@connect/")"
if grep -qi '^content-security-policy:' <<<"$hdr" && grep -qi '^cache-control: no-store' <<<"$hdr"; then
  ok "page served with CSP and no-store"
else
  bad "page headers: $hdr"
fi
redir="$(curl -sS -o /dev/null -w '%{http_code} %{redirect_url}' "$base/@connect")"
[[ "$redir" == 301\ */@connect/ ]] && ok "/@connect redirects to /@connect/" || bad "/@connect answered $redir"
if "$here/scripts/check-bundle.sh" "$base" >/dev/null; then ok "served bundle passes check-bundle.sh"; else bad "check-bundle.sh $base"; fi
for p in / /@manage; do
  code="$(curl -sS -o /dev/null -w '%{http_code}' "$base$p")"
  [ "$code" = 200 ] && ok "upstream UI $p still served" || bad "upstream UI $p answered $code"
done

echo "== native storage form still works (Local root for the example storage)"
loc="$(api POST /api/admin/storage/create '{"mount_path":"/local","driver":"Local","order":0,"cache_expiration":0,"addition":"{\"root_folder_path\":\"/opt/openlist/data/files\"}"}')"
check "native create of a Local storage" '.code==200' "$loc"

echo "== default mount path"
sg="$(api GET '/api/admin/connect/mount/suggest?platform=onedrive')"
check "suggests the platform name" '.code==200 and .data.mount_path=="/OneDrive"' "$sg"
occ="$(api POST /api/admin/storage/create '{"mount_path":"/OneDrive","driver":"Local","order":0,"cache_expiration":0,"addition":"{\"root_folder_path\":\"/opt/openlist/data/files\"}"}')"
check "occupy /OneDrive with a Local storage" '.code==200' "$occ"
sg="$(api GET '/api/admin/connect/mount/suggest?platform=onedrive')"
check "a taken name gets a numeric suffix" '.data.mount_path=="/OneDrive-2"' "$sg"
api POST "/api/admin/storage/delete?id=$(jq -r '.data.id' <<<"$occ")" >/dev/null
nf="$(api GET '/api/admin/connect/mount/suggest?platform=nope')"
check "unknown platform answers not_found" '.data.error_code=="not_found"' "$nf"

# The paste flows need no network until save: the driver renews the token on
# init, so the session is cancelled before that.
echo "== token paste (OneDrive and Google default: OpenList online service)"
s="$(start '{"platform":"onedrive"}')"
sid="$(jq -r '.data.id' <<<"$s")"
check "onedrive session is a paste form with a guide to the token page" \
  '.data.phase=="pending" and .data.method=="form" and .data.form_label=="粘贴令牌" and ([.data.form[].key]==["refresh_token"]) and (.data.guide.url|startswith("https://api.oplist.org/#"))' "$s"
fake="e2e-fake-refresh-token-$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')"
r="$(api POST /api/admin/connect/session/submit "$(jq -nc --arg id "$sid" --arg t "$fake" '{id:$id, values:{refresh_token:$t}}')")"
check "pasted token: authorized, renewed through the online service" \
  '.data.phase=="authorized" and .data.auth.app_mode=="online_service" and ((["refresh_token","use_online_api","api_url_address"] - .data.auth.fields) == [])' "$r"
hasnt "submit response carries no pasted token" "$fake" "$r"
r="$(api POST /api/admin/connect/session/cancel "$(jq -nc --arg id "$sid" '{id:$id}')")"
check "cancel the paste session" '.data.phase=="cancelled"' "$r"
g="$(start '{"platform":"googledrive"}')"
check "google guide opens the token page without preselection" \
  '.data.method=="form" and .data.guide.url=="https://api.oplist.org/"' "$g"
api POST /api/admin/connect/session/cancel "$(jq -nc --arg id "$(jq -r '.data.id' <<<"$g")" '{id:$id}')" >/dev/null

echo "== accounts used by the example storage"
u="$(api POST /api/admin/user/create "$(jq -nc --arg p "$pass2" '{username:"e2e-user", password:$p, base_path:"/", role:0, permission:0, disabled:false}')")"
check "create a second OpenList user" '.code==200' "$u"
u="$(api POST /api/admin/user/create "$(jq -nc --arg p "$pass3" '{username:"e2e-off", password:$p, base_path:"/", role:0, permission:0, disabled:true}')")"
check "create a disabled OpenList user" '.code==200' "$u"

echo "== create: login, storage step, save"
s="$(start '{"platform":"example-openlist"}')"
sid="$(jq -r '.data.id' <<<"$s")"
check "form session pending with url/username/password" \
  '.data.phase=="pending" and .data.mode=="create" and ([.data.form[].key]==["url","username","password"])' "$s"
r="$(submit "$sid" admin "$pass" "$self")"
check "submit: authorized, only field names in the summary" \
  '.data.phase=="authorized" and (.data.auth.fields|index("password")) and .data.auth.credential_type=="account_password"' "$r"
hasnt "submit response carries no password" "$pass" "$r"
r="$(save "$sid" /e2e /local "e2e remark")"
check "save: storage created and initialized (work)" \
  '.data.phase=="saved" and .data.result.ok and .data.result.mode=="create" and .data.result.status=="work"' "$r"
id1="$(jq -r '.data.result.storage_id' <<<"$r")"
r2="$(save "$sid" /e2e-other /local "")"
[ "$(jq -r '.data.result.storage_id' <<<"$r2")" = "$id1" ] && ok "repeat save returns the same result (#$id1)" || bad "repeat save: $r2"
[ "$(count_mount /e2e)" = 1 ] && [ "$(count_mount /e2e-other)" = 0 ] && ok "exactly one storage written" || bad "storage rows after repeat save"
hasnt "save response carries no password" "$pass" "$r"

echo "== file operations through the new storage"
ls="$(api POST /api/fs/list '{"path":"/e2e","refresh":true}')"
check "list /e2e shows the remote directory" '.code==200 and ([.data.content[].name]|index("hello.txt"))' "$ls"
up="$(curl -sS -X PUT "$base/api/fs/put" -H "Authorization: $tok" -H 'File-Path: /e2e/up.txt' \
  -H 'Content-Type: application/octet-stream' --data-binary 'uploaded via e2e')"
check "upload through /e2e" '.code==200' "$up"
ondisk="$(podman unshare cat "$data/files/up.txt" 2>/dev/null || true)"
[ "$ondisk" = "uploaded via e2e" ] && ok "upload landed in the Local root" || bad "file on disk: [$ondisk]"
g="$(api POST /api/fs/get '{"path":"/e2e/up.txt"}')"
sign="$(jq -r '.data.sign // empty' <<<"$g")"
body="$(curl -sS "$base/p/e2e/up.txt?sign=$sign")"
[ "$body" = "uploaded via e2e" ] && ok "download through /e2e" || bad "download body: [$body]"
rm_="$(api POST /api/fs/remove '{"dir":"/e2e","names":["up.txt"]}')"
check "delete through /e2e" '.code==200' "$rm_"
podman unshare test ! -e "$data/files/up.txt" && ok "delete reached the Local root" || bad "up.txt still on disk"

echo "== create failure: driver init fails, the attempt's storage is removed"
s="$(start '{"platform":"example-openlist"}')"
sid="$(jq -r '.data.id' <<<"$s")"
submit "$sid" e2e-off "$pass3" "$self" >/dev/null
r="$(save "$sid" /e2e-bad /local "")"
check "save_failed with cleanup of this attempt's storage" \
  '.data.phase=="save_failed" and (.data.result.ok|not) and .data.result.credentials_written and .data.result.cleanup.attempted and .data.result.cleanup.deleted' "$r"
[ "$(count_mount /e2e-bad)" = 0 ] && ok "no storage left at /e2e-bad" || bad "storage left at /e2e-bad"
hasnt "failure response carries no password" "$pass3" "$r"

echo "== retryable save: mount path taken, nothing written"
s="$(start '{"platform":"example-openlist"}')"
sid="$(jq -r '.data.id' <<<"$s")"
submit "$sid" admin "$pass" "$self" >/dev/null
r="$(save "$sid" /e2e /local "")"
check "taken mount: retryable, session stays authorized" \
  '.data.phase=="authorized" and .data.result.retryable and (.data.result.ok|not) and (.data.result.credentials_written|not)' "$r"
r="$(api POST /api/admin/connect/session/cancel "$(jq -nc --arg id "$sid" '{id:$id}')")"
check "cancel ends the session" '.data.phase=="cancelled" and .data.error_code=="cancelled"' "$r"
r="$(save "$sid" /e2e-late /local "")"
check "save after cancel answers cancelled (410)" '.code==410 and .data.error_code=="cancelled"' "$r"

echo "== re-authorization target"
t="$(api GET "/api/admin/connect/target?id=$id1")"
check "target pins driver and offers the example adapter" \
  ".data.storage_id==$id1 and .data.driver==\"OpenList\" and ([.data.platforms[].id]==[\"example-openlist\"]) and .data.platforms[0].problem==\"\"" "$t"
t="$(api GET "/api/admin/connect/target?id=$(jq -r '.data.content[]|select(.mount_path=="/local")|.id' <<<"$(api GET '/api/admin/storage/list?page=1&per_page=500')")")"
check "Local storage has no adapter (native form only)" '.data.platforms==[] and (.data.edit_path|test("/@manage/storages/edit/"))' "$t"

echo "== re-authorization failure: original config restored and verified"
before="$(storage "$id1")"
s="$(start "{\"platform\":\"example-openlist\",\"reauth_storage_id\":$id1}")"
sid="$(jq -r '.data.id' <<<"$s")"
check "reauth session hides the create-only url field" \
  '.data.mode=="reauth" and ([.data.form[].key]==["username","password"])' "$s"
submit "$sid" e2e-off "$pass3" >/dev/null
r="$(save "$sid" "" "" "")"
check "reauth save_failed, restore attempted and verified, status work" \
  '.data.phase=="save_failed" and .data.result.credentials_written and .data.result.restore.attempted and .data.result.restore.restored and .data.result.restore.status=="work"' "$r"
after="$(storage "$id1")"
check "same storage id, mount, remark and original password after restore" \
  ".data.id==$id1 and .data.mount_path==\"/e2e\" and .data.remark==\"e2e remark\" and .data.status==\"work\" and (.data.addition|fromjson|.password)==\"$pass\"" "$after"
hasnt "reauth failure response carries no password" "$pass3" "$r"

echo "== re-authorization refuses to overwrite a concurrent edit"
s="$(start "{\"platform\":\"example-openlist\",\"reauth_storage_id\":$id1}")"
sid="$(jq -r '.data.id' <<<"$s")"
submit "$sid" admin "$pass" >/dev/null
edited="$(storage "$id1" | jq -c '.data | .remark="edited meanwhile"')"
api POST /api/admin/storage/update "$edited" >/dev/null
r="$(save "$sid" "" "" "")"
check "save refused, nothing written" \
  '.data.phase=="save_failed" and (.data.result.ok|not) and (.data.result.credentials_written|not)' "$r"
check "the concurrent edit is kept" '.data.remark=="edited meanwhile"' "$(storage "$id1")"

echo "== re-authorization success with another account"
before="$(storage "$id1")"
s="$(start "{\"platform\":\"example-openlist\",\"reauth_storage_id\":$id1}")"
sid="$(jq -r '.data.id' <<<"$s")"
submit "$sid" e2e-user "$pass2" >/dev/null
r="$(save "$sid" "" "" "")"
check "reauth saved on the same storage" ".data.phase==\"saved\" and .data.result.ok and .data.result.storage_id==$id1 and .data.result.status==\"work\"" "$r"
after="$(storage "$id1")"
check "only credential fields changed" \
  "(.data.addition|fromjson|.username)==\"e2e-user\" and .data.mount_path==\"/e2e\" and .data.remark==\"edited meanwhile\"" "$after"
same="$(jq -n --argjson a "$before" --argjson b "$after" \
  '($a.data.addition|fromjson|del(.username,.password,.token)) == ($b.data.addition|fromjson|del(.username,.password,.token))
   and ($a.data|del(.addition,.modified,.status)) == ($b.data|del(.addition,.modified,.status))')"
[ "$same" = true ] && ok "every other setting preserved" || bad "settings changed by reauth"
ls="$(api POST /api/fs/list '{"path":"/e2e","refresh":true}')"
check "list /e2e works with the new account" '.code==200 and ([.data.content[].name]|index("hello.txt"))' "$ls"

echo "== OAuth callback, owner binding and limits"
cbfile="$(mktemp)"
code="$(curl -sS -D "$cbfile.h" -o "$cbfile" -w '%{http_code}' "$base/api/connect/oauth/callback?state=e2e-unknown-state&code=E2E-CODE-VALUE-123")"
[ "$code" = 400 ] && ok "unknown state answers 400" || bad "unknown state answered $code"
hasnt "callback page does not echo the code" "E2E-CODE-VALUE-123" "$(cat "$cbfile")"
grep -qi '^referrer-policy: no-referrer' "$cbfile.h" && ok "callback page sets Referrer-Policy: no-referrer" || bad "callback headers: $(cat "$cbfile.h")"
rm -f "$cbfile" "$cbfile.h"
perm="$(api GET '/api/admin/setting/get?key=token' | jq -r '.data.value')"
s="$(start '{"platform":"example-openlist"}')"
sid="$(jq -r '.data.id' <<<"$s")"
other="$(curl -sS "$base/api/admin/connect/session/get?id=$sid" -H "Authorization: $perm")"
check "another token of the same admin cannot see the session" '.code==404 and .data.error_code=="not_found"' "$other"
ids=("$sid")
for _ in 1 2 3 4; do
  ids+=("$(start '{"platform":"example-openlist"}' | jq -r '.data.id')")
done
r="$(start '{"platform":"example-openlist"}')"
check "sixth live session is refused (limit)" '.code==429 and .data.error_code=="limit"' "$r"
for i in "${ids[@]}"; do
  api POST /api/admin/connect/session/cancel "$(jq -nc --arg id "$i" '{id:$id}')" >/dev/null
done
r="$(start '{"platform":"example-openlist"}')"
check "cancelled sessions free the limit" '.code==200' "$r"
api POST /api/admin/connect/session/cancel "$(jq -nc --arg id "$(jq -r '.data.id' <<<"$r")" '{id:$id}')" >/dev/null
noauth="$(curl -sS "$base/api/admin/connect/platforms")"
check "admin API refuses requests without a token" '.code==401 or .code==403' "$noauth"
utok="$(login e2e-user "$pass2")"
nonadmin="$(curl -sS "$base/api/admin/connect/platforms" -H "Authorization: $utok")"
check "admin API refuses a non-admin user" '.code==403' "$nonadmin"

echo "== logs"
# OpenList writes to stdout only until logrus is initialized, then to
# data/log/log.log (access log included).
logs="$(podman logs "$name" 2>&1; podman unshare cat "$data/log/log.log" 2>/dev/null)"
[ "$(grep -c 'GIN' <<<"$logs")" -gt 50 ] && ok "access log captured for the checks below" || bad "access log not found"
hasnt "logs do not contain the admin password" "$pass" "$logs"
hasnt "logs do not contain the second user's password" "$pass2" "$logs"
hasnt "logs do not contain the disabled user's password" "$pass3" "$logs"
grep -q 'connect: created storage' <<<"$logs" && ok "lifecycle is logged" || bad "no connect lifecycle log lines"

echo
if [ "$fails" = 0 ]; then
  echo "e2e passed ($image)"
else
  echo "e2e: $fails check(s) failed ($image)"
  exit 1
fi
