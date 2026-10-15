#!/usr/bin/env bash
# Acceptance tests for cloud2api, DESIGN.md §8.
# Expects: openlist + cloud2api containers running (scripts/deploy.sh).
# Isolated runs must set BASE, OL_BASE, OL_PASS_FILE, PORTAL_PASS_FILE,
# C2A_CONTAINER, TESTDATA_DIR and C2A_DB_FILE; never fall back to live targets.
set -u
BASE="${BASE:-http://127.0.0.1:8080}"
KEY="${CLOUD2API_KEY:?set CLOUD2API_KEY to the plaintext sk-... key}"
if [ "$BASE" != http://127.0.0.1:8080 ]; then
  for target in OL_BASE OL_PASS_FILE PORTAL_PASS_FILE C2A_CONTAINER TESTDATA_DIR C2A_DB_FILE; do
    [ -n "${!target:-}" ] || { printf 'Isolated run requires %s\n' "$target" >&2; exit 2; }
  done
fi
OL="${OL_BASE:-http://127.0.0.1:5244}"
OL_PORT=$(python3 -c 'import sys,urllib.parse; u=urllib.parse.urlparse(sys.argv[1]); print(u.port or (443 if u.scheme=="https" else 80))' "$OL")
OL_PASS_FILE="${OL_PASS_FILE:-$(dirname "$0")/../cloud2api-data/openlist_admin_password}"
PORTAL_PASS_FILE="${PORTAL_PASS_FILE:-$(dirname "$0")/../cloud2api-data/admin_password}"
C2A_CONTAINER="${C2A_CONTAINER:-cloud2api}"
TESTDATA_DIR="${TESTDATA_DIR:-$(dirname "$0")/../testdata}"
PASS=0; FAIL=0

ok()   { PASS=$((PASS+1)); echo "PASS  $1"; }
bad()  { FAIL=$((FAIL+1)); echo "FAIL  $1  ($2)"; }
code() { curl -s -o /tmp/acc_body -w '%{http_code}' "$@"; }
body() { cat /tmp/acc_body; }

echo "== P1 basic proxy =="

# A1: list baidu-main root, internal prefix must not leak
c=$(code -X POST "$BASE/api/storage/baidu-main/list" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"path":"/"}')
if [ "$c" = 200 ] && body | grep -q hello.txt && ! body | grep -q '/baidu'; then ok "A1 list baidu-main"; else bad "A1" "code=$c body=$(body | head -c 200)"; fi

# A2: same key lists pan123-main
c=$(code -X POST "$BASE/api/storage/pan123-main/list" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"path":"/"}')
if [ "$c" = 200 ] && body | grep -q hello.txt; then ok "A2 list pan123-main"; else bad "A2" "code=$c body=$(body | head -c 200)"; fi

# A3: same key must not reach aliyun-main
c=$(code -X POST "$BASE/api/storage/aliyun-main/list" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"path":"/"}')
if [ "$c" = 403 ]; then ok "A3 aliyun-main forbidden"; else bad "A3" "code=$c"; fi

# A4: no key and wrong key -> 401
c=$(code -X POST "$BASE/api/storage/baidu-main/list" -H 'Content-Type: application/json' -d '{"path":"/"}')
[ "$c" = 401 ] && ok "A4a no key -> 401" || bad "A4a" "code=$c"
c=$(code -X POST "$BASE/api/storage/baidu-main/list" -H "Authorization: Bearer sk-wrong" -H 'Content-Type: application/json' -d '{"path":"/"}')
[ "$c" = 401 ] && ok "A4b wrong key -> 401" || bad "A4b" "code=$c"

# A5: hostile paths -> 400
c=$(code -X POST "$BASE/api/storage/baidu-main/list" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"path":"/../"}')
[ "$c" = 400 ] && ok "A5a /../ -> 400" || bad "A5a" "code=$c"
c=$(code -X POST "$BASE/api/storage/baidu-main/list" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"path":"/%2e%2e/"}')
[ "$c" = 400 ] && ok "A5b encoded dotdot in body -> 400" || bad "A5b" "code=$c"
c=$(code -X POST "$BASE/api/storage/baidu-main/list" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"path":"/baidu/../123pan"}')
[ "$c" = 400 ] && ok "A5c cross-mount traversal -> 400" || bad "A5c" "code=$c"
c=$(code --path-as-is "$BASE/api/storage/baidu-main/raw/%2e%2e/secret.txt" -H "Authorization: Bearer $KEY")
[ "$c" = 400 ] && ok "A5d encoded dotdot in URL -> 400" || bad "A5d" "code=$c"

# A6: put -> get -> remove roundtrip
c=$(code -X PUT "$BASE/api/storage/baidu-main/put" -H "Authorization: Bearer $KEY" -H 'File-Path: %2Facctest%2Fhello.txt' -H 'Content-Type: application/octet-stream' --data-binary 'roundtrip-content')
if [ "$c" = 200 ]; then ok "A6a put"; else bad "A6a" "code=$c body=$(body | head -c 200)"; fi
c=$(code -X POST "$BASE/api/storage/baidu-main/get" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"path":"/acctest/hello.txt"}')
if [ "$c" = 200 ] && body | grep -q hello.txt; then ok "A6b get"; else bad "A6b" "code=$c body=$(body | head -c 200)"; fi
c=$(code "$BASE/api/storage/baidu-main/raw/acctest/hello.txt" -H "Authorization: Bearer $KEY")
if [ "$c" = 200 ] && [ "$(body)" = "roundtrip-content" ]; then ok "A6c raw download content"; else bad "A6c" "code=$c body=$(body | head -c 100)"; fi
c=$(code -X POST "$BASE/api/storage/baidu-main/remove" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"dir":"/acctest","names":["hello.txt"]}')
[ "$c" = 200 ] && ok "A6d remove" || bad "A6d" "code=$c body=$(body | head -c 200)"
c=$(code -X POST "$BASE/api/storage/baidu-main/get" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"path":"/acctest/hello.txt"}')
[ "$c" != 200 ] && ok "A6e gone after remove" || bad "A6e" "code=$c"

# A7: OpenList is reachable exactly where deploy.sh published it. Pass the
# same OPENLIST_PUBLISH_ADDR (comma-separated list, default 127.0.0.1, "none"
# = unpublished) that the deployment used.
LAN_IP=$(hostname -I | awk '{print $1}')
ol_reachable() { curl -s -m 3 -o /dev/null "http://$1:$OL_PORT/api/public/settings" && return 0 || return 1; }
OL_PUB="${OPENLIST_PUBLISH_ADDR-127.0.0.1}"
[ "$OL_PUB" = none ] && OL_PUB=""
case ",$OL_PUB," in
  *,0.0.0.0,*|*",$LAN_IP,"*) OL_LAN_EXPECTED=1 ;;
  *) OL_LAN_EXPECTED=0 ;;
esac
case ",$OL_PUB," in
  *,127.0.0.1,*|*,0.0.0.0,*) OL_LO_EXPECTED=1 ;;
  *) OL_LO_EXPECTED=0 ;;
esac
if [ -z "$LAN_IP" ]; then
  ok "A7 skipped (no LAN IP detected)"
else
  ol_reachable 127.0.0.1 && lo=1 || lo=0
  ol_reachable "$LAN_IP" && lan=1 || lan=0
  if [ "$lo" = "$OL_LO_EXPECTED" ] && [ "$lan" = "$OL_LAN_EXPECTED" ]; then
    ok "A7 openlist reachability matches publish list '${OL_PUB:-none}' (loopback=$lo LAN=$lan)"
  else
    bad "A7 openlist reachability" "publish='${OL_PUB:-none}' expected loopback=$OL_LO_EXPECTED LAN=$OL_LAN_EXPECTED, got loopback=$lo LAN=$lan"
  fi
fi

# A8 (v0.3.1 leak fix): legacy get responses must not contain the internal
# OpenList address/port or the raw_url/sign/provider keys.
c=$(code -X POST "$BASE/api/storage/baidu-main/get" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"path":"/hello.txt"}')
if [ "$c" = 200 ] && ! body | grep -Eq '"(raw_url|sign|provider)"' && ! body | grep -q '5244'; then
  ok "A8 get response free of raw_url/sign/provider/5244"
else
  bad "A8" "code=$c body=$(body | head -c 200)"
fi

echo "== P2 transfer =="

# Idempotency: remove leftovers from previous runs (errors ignored).
curl -s -o /dev/null -X POST "$BASE/api/storage/baidu-main/remove" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"dir":"/research","names":["hello.txt"]}'
curl -s -o /dev/null -X POST "$BASE/api/storage/pan123-main/remove" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"dir":"/backup","names":["data.csv"]}'
curl -s -o /dev/null -X POST "$BASE/api/storage/pan123-main/remove" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"dir":"/big","names":["big.bin"]}'

# B1: copy within one account
c=$(code -X POST "$BASE/api/storage/baidu-main/copy" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"src_dir":"/","dst_dir":"/research","names":["hello.txt"]}')
if [ "$c" = 200 ]; then ok "B1 same-account copy"; else bad "B1" "code=$c body=$(body | head -c 200)"; fi
c=$(code -X POST "$BASE/api/storage/baidu-main/get" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"path":"/research/hello.txt"}')
[ "$c" = 200 ] && ok "B1b copy landed" || bad "B1b" "code=$c"

# B2: cross-account transfer baidu -> 123pan
c=$(code -X POST "$BASE/api/transfer" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"src_account":"baidu-main","dst_account":"pan123-main","src_dir":"/research","dst_dir":"/backup","names":["data.csv"]}')
if [ "$c" = 200 ]; then ok "B2 cross-account transfer accepted"; else bad "B2" "code=$c body=$(body | head -c 200)"; fi
sleep 2
c=$(code "$BASE/api/storage/pan123-main/raw/backup/data.csv" -H "Authorization: Bearer $KEY")
if [ "$c" = 200 ] && [ "$(body)" = "research dataset v1" ]; then ok "B2b transferred content matches"; else bad "B2b" "code=$c body=$(body | head -c 100)"; fi

# B3: transfer to account outside whitelist -> 403
c=$(code -X POST "$BASE/api/transfer" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"src_account":"baidu-main","dst_account":"aliyun-main","src_dir":"/","dst_dir":"/","names":["hello.txt"]}')
[ "$c" = 403 ] && ok "B3 transfer to non-whitelisted account -> 403" || bad "B3" "code=$c"

# B4: 100MB file transfer, memory stays flat
big="$TESTDATA_DIR/baidu/big.bin"
[ -f "$big" ] || dd if=/dev/zero of="$big" bs=1M count=100 status=none
podman stats --no-stream --format '{{.MemUsage}}' "$C2A_CONTAINER" > /tmp/mem_before
c=$(code -X POST "$BASE/api/transfer" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"src_account":"baidu-main","dst_account":"pan123-main","src_dir":"/","dst_dir":"/big","names":["big.bin"]}')
ok_wait=0
for i in $(seq 1 30); do
  sleep 2
  rc=$(code -X POST "$BASE/api/storage/pan123-main/get" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"path":"/big/big.bin"}')
  [ "$rc" = 200 ] && { ok_wait=1; break; }
done
[ "$ok_wait" = 1 ] && ok "B4a 100MB transfer completed" || bad "B4a" "transfer code=$c, never appeared"
sz=$(stat -c%s "$TESTDATA_DIR/123pan/big/big.bin" 2>/dev/null || echo 0)
[ "$sz" = 104857600 ] && ok "B4b size intact ($sz)" || bad "B4b" "size=$sz"
mem=$(podman stats --no-stream --format '{{.MemUsage}}' "$C2A_CONTAINER" | cut -d/ -f1)
echo "INFO  cloud2api mem before=$(cut -d/ -f1 /tmp/mem_before) after=$mem"
ok "B4c memory sampled (see INFO)"

# OpenList admin API helpers. cloud2api never creates, edits or deletes
# OpenList storages (OpenList owns storage configuration), so the tests do it
# here directly, the way an administrator would in the OpenList admin UI.
OL_PASS="$(<"$OL_PASS_FILE")"
OL_TOKEN=$(curl -s -X POST "$OL/api/auth/login" -H 'Content-Type: application/json' \
  -d "{\"username\":\"admin\",\"password\":\"$OL_PASS\"}" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
olget() { curl -s -H "Authorization: $OL_TOKEN" "$OL$1"; }
olpost() { curl -s -X POST -H "Authorization: $OL_TOKEN" -H 'Content-Type: application/json' ${2:+--data-binary "$2"} "$OL$1"; }
ol_sid_by_mount() { # ol_sid_by_mount <mount>
  olget /api/admin/storage/list | python3 -c "
import json, sys
for s in json.load(sys.stdin)['data']['content']:
    if s['mount_path'] == '$1':
        print(s['id']); break"
}
ol_storage_json() { olget "/api/admin/storage/get?id=$1"; }
ol_mk_local() { # ol_mk_local <mount> <root>: create a Local storage, print its id
  olpost /api/admin/storage/create "{\"mount_path\":\"$1\",\"driver\":\"Local\",\"addition\":\"{\\\"root_folder_path\\\":\\\"$2\\\"}\"}" \
    | sed -n 's/.*"id":\([0-9]*\).*/\1/p'
}
ol_rm_mount() { # ol_rm_mount <mount>...: delete the storages at these mounts
  local m sid
  for m in "$@"; do
    sid=$(ol_sid_by_mount "$m")
    if [ -n "$sid" ]; then olpost "/api/admin/storage/delete?id=$sid" >/dev/null; fi
  done
}
ol_set_root() { # ol_set_root <storage id> <root>: edit a Local storage's root in OpenList
  olpost /api/admin/storage/update "$(ol_storage_json "$1" | python3 -c "
import json, sys
d = json.load(sys.stdin)['data']; a = json.loads(d['addition']); a['root_folder_path'] = '$2'
d['addition'] = json.dumps(a); print(json.dumps(d))")" >/dev/null
}
[ -n "$OL_TOKEN" ] || echo "WARN  OpenList admin login failed; groups that need it will fail"

echo "== P3 admin portal =="

PORTAL_PASSWORD="${PORTAL_PASSWORD:-}"
if [ -z "$PORTAL_PASSWORD" ] && [ -f "$PORTAL_PASS_FILE" ]; then
  PORTAL_PASSWORD=$(<"$PORTAL_PASS_FILE")
fi
[ -n "$PORTAL_PASSWORD" ] || { echo "FAIL  C0"; echo "RESULT: portal password missing (set PORTAL_PASSWORD)"; exit 1; }
JAR=/tmp/acc_cookies
rm -f "$JAR"

# C1: unauthenticated access -> 401 for the API, redirect for the browser
c=$(code "$BASE/admin/api/accounts")
[ "$c" = 401 ] && ok "C1a unauthenticated API -> 401" || bad "C1a" "code=$c"
c=$(code "$BASE/admin/")
[ "$c" = 302 ] && ok "C1b unauthenticated page -> 302" || bad "C1b" "code=$c"

# C2: login sets an HttpOnly session cookie
c=$(code -c "$JAR" -X POST "$BASE/admin/api/login" -H 'Content-Type: application/json' -d "{\"password\":\"$PORTAL_PASSWORD\"}")
if [ "$c" = 200 ] && grep -q '^#HttpOnly_' "$JAR"; then ok "C2 login + HttpOnly cookie"; else bad "C2" "code=$c jar=$(head -c 100 "$JAR" 2>/dev/null)"; fi

# Idempotency: drop leftovers from previous runs (errors ignored).
curl -s -b "$JAR" -o /dev/null -X DELETE "$BASE/admin/api/accounts/portal-acct"
curl -s -b "$JAR" -o /dev/null -X DELETE "$BASE/admin/api/accounts/portal-persist"
ol_rm_mount /portal-test /portal-persist

# C3: storages are created in OpenList, then adopted through the portal; the
# legacy accounts listing merges their OpenList status.
C3SID=$(ol_mk_local /portal-test /mnt/portal-test)
C3PSID=$(ol_mk_local /portal-persist /mnt/portal-test)
c=$(code -b "$JAR" -X POST "$BASE/admin/api/drives/adopt" -H 'Content-Type: application/json' -d "{\"storage_id\":${C3SID:-0},\"id\":\"portal-acct\"}")
[ "$c" = 200 ] && ok "C3a OpenList storage adopted as portal-acct" || bad "C3a" "sid=$C3SID code=$c body=$(body | head -c 200)"
c=$(code -b "$JAR" -X POST "$BASE/admin/api/drives/adopt" -H 'Content-Type: application/json' -d "{\"storage_id\":${C3PSID:-0},\"id\":\"portal-persist\"}")
[ "$c" = 200 ] && ok "C3b OpenList storage adopted as portal-persist" || bad "C3b" "sid=$C3PSID code=$c body=$(body | head -c 200)"
c=$(code -b "$JAR" "$BASE/admin/api/accounts")
if [ "$c" = 200 ] && body | grep -q portal-acct && body | grep -q '"openlist_status":"work"'; then ok "C3c accounts listing merges OpenList status"; else bad "C3c" "code=$c body=$(body | head -c 200)"; fi

# C4: 移出 drops only the mapping; the OpenList storage stays, is listed as
# unmanaged again and can be re-adopted.
c=$(code -b "$JAR" -X DELETE "$BASE/admin/api/accounts/portal-acct")
[ "$c" = 200 ] && ok "C4a portal 移出" || bad "C4a" "code=$c body=$(body | head -c 200)"
c=$(code -b "$JAR" "$BASE/admin/api/accounts")
body | grep -q portal-acct && bad "C4b" "account still listed" || ok "C4b account gone from listing"
c=$(olget "/api/admin/storage/get?id=$C3SID" | sed -n 's/^{"code":\([0-9]*\).*/\1/p')
[ "$c" = 200 ] && ok "C4c OpenList storage kept after 移出" || bad "C4c" "storage/get code=$c"
c=$(code -b "$JAR" "$BASE/admin/api/drives/unmanaged")
if [ "$c" = 200 ] && body | python3 -c "
import json, sys
sys.exit(0 if any(s['storage_id'] == ${C3SID:-0} for s in json.load(sys.stdin)['data']['storages']) else 1)"; then
  ok "C4d storage listed as unmanaged again"
else bad "C4d" "code=$c body=$(body | head -c 200)"; fi
c=$(code -b "$JAR" -X POST "$BASE/admin/api/drives/adopt" -H 'Content-Type: application/json' -d "{\"storage_id\":${C3SID:-0},\"id\":\"portal-acct\"}")
[ "$c" = 200 ] && ok "C4e storage re-adoptable after 移出" || bad "C4e" "code=$c body=$(body | head -c 200)"
curl -s -b "$JAR" -o /dev/null -X DELETE "$BASE/admin/api/accounts/portal-acct"
ol_rm_mount /portal-test

# C5: dynamic accounts survive a cloud2api restart (state persistence)
podman restart "$C2A_CONTAINER" >/dev/null
up=0
for i in $(seq 1 15); do
  sleep 1
  [ "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/admin/login")" = 200 ] && { up=1; break; }
done
[ "$up" = 1 ] && ok "C5a cloud2api restarted" || bad "C5a" "service did not come back"
rm -f "$JAR"
c=$(code -c "$JAR" -X POST "$BASE/admin/api/login" -H 'Content-Type: application/json' -d "{\"password\":\"$PORTAL_PASSWORD\"}")
[ "$c" = 200 ] && ok "C5b re-login after restart" || bad "C5b" "code=$c"
c=$(code -b "$JAR" "$BASE/admin/api/accounts")
if [ "$c" = 200 ] && body | grep -q portal-persist; then ok "C5c dynamic account persisted"; else bad "C5c" "code=$c body=$(body | head -c 200)"; fi

# C6 (v0.3.1 admin hardening): cross-origin mutating requests are rejected
# pre-authentication; bodies must be JSON.
c=$(code -X POST "$BASE/admin/api/drives/sync" -H "Origin: http://evil.example" -H 'Content-Type: application/json' -d '{}')
[ "$c" = 403 ] && ok "C6a cross-origin POST /admin/api/drives/sync -> 403" || bad "C6a" "code=$c"
c=$(code -X POST "$BASE/admin/api/login" -H "Origin: null" -H 'Content-Type: application/json' -d '{"password":"x"}')
[ "$c" = 403 ] && ok "C6b Origin:null login -> 403" || bad "C6b" "code=$c"
c=$(code -X POST "$BASE/admin/api/login" -H "Sec-Fetch-Site: cross-site" -H 'Content-Type: application/json' -d '{"password":"x"}')
[ "$c" = 403 ] && ok "C6c Sec-Fetch-Site cross-site login -> 403" || bad "C6c" "code=$c"
c=$(code -X POST "$BASE/admin/api/login" -H 'Content-Type: text/plain' -d '{"password":"x"}')
[ "$c" = 415 ] && ok "C6d non-JSON body login -> 415" || bad "C6d" "code=$c"
# Same-origin Origin header must still work (correct password, so this also
# proves the guard does not block legitimate portal traffic).
c=$(code -X POST "$BASE/admin/api/login" -H "Origin: $BASE" -H 'Content-Type: application/json' -d "{\"password\":\"$PORTAL_PASSWORD\"}")
[ "$c" = 200 ] && ok "C6e same-origin Origin accepted" || bad "C6e" "code=$c"

# C8: the storage-writing portal endpoints are gone (404).
for spec in "POST /admin/api/drives" "POST /admin/api/accounts" "GET /admin/api/providers" \
            "POST /admin/api/login-flows" "GET /admin/api/drives/portal-persist/settings" \
            "PUT /admin/api/drives/portal-persist/settings"; do
  m=${spec%% *}; u=${spec#* }
  c=$(code -b "$JAR" -X "$m" "$BASE$u" -H 'Content-Type: application/json' -d '{}')
  [ "$c" = 404 ] && ok "C8 $spec -> 404" || bad "C8 $spec" "code=$c body=$(body | head -c 120)"
done


# ============================================================================
# Group D — Agent API (v0.4 Phase 1a, UPGRADE §6). Uses a dedicated test key
# created through the admin API and revokes it at the end.
# ============================================================================
echo "== P4 agent API =="

crc64_py() { # crc64_py <string>  -> decimal CRC-64/XZ
  python3 - "$1" <<'PYEOF'
import sys
def crc64(data):
    poly = 0xC96C5795D7870F42
    table = []
    for i in range(256):
        crc = i
        for _ in range(8):
            crc = (crc >> 1) ^ poly if crc & 1 else crc >> 1
        table.append(crc)
    crc = 0xFFFFFFFFFFFFFFFF
    for b in data:
        crc = table[(crc ^ b) & 0xFF] ^ (crc >> 8)
    return crc ^ 0xFFFFFFFFFFFFFFFF
print(crc64(sys.argv[1].encode()))
PYEOF
}

# D0: admin login (fresh cookie jar) and create the dedicated test key.
rm -f /tmp/acc_djar
PORTAL_PASSWORD="${PORTAL_PASSWORD:-}"
if [ -z "$PORTAL_PASSWORD" ] && [ -f "$PORTAL_PASS_FILE" ]; then
  PORTAL_PASSWORD=$(<"$PORTAL_PASS_FILE")
fi
if [ -n "$PORTAL_PASSWORD" ]; then
  c=$(code -c /tmp/acc_djar -X POST "$BASE/admin/api/login" -H 'Content-Type: application/json' -d "{\"password\":\"$PORTAL_PASSWORD\"}")
  [ "$c" = 200 ] && ok "D0a admin login" || bad "D0a" "code=$c"
  KEYREQ='{"id":"acc-test","default_drive":"baidu-main","drives":[{"drive":"baidu-main","root":"/acc-test","perms":["list","read","write","delete","link","purge"]}],"global_perms":["link"],"trash":{"enabled":true,"days":7}}'
  c=$(code -b /tmp/acc_djar -X POST "$BASE/admin/api/keys" -H 'Content-Type: application/json' -d "$KEYREQ")
  AGENT_KEY=$(body | sed -n 's/.*"plaintext":"\([^"]*\)".*/\1/p')
  if [ "$c" = 200 ] && [ -n "$AGENT_KEY" ]; then
    ok "D0b admin created test key"
  else
    bad "D0b" "code=$c body=$(body | head -c 200)"
    echo "RESULT: $PASS passed, $FAIL failed (D group needs the portal key API)"
    exit 1
  fi
  if body | grep -q "onboarding_prompt" && body | grep -q "agent-instructions.txt"; then
    ok "D0c key create returns onboarding prompt"
  else bad "D0c" "no onboarding_prompt"; fi
else
  bad "D0" "portal password missing"
fi

# D0z: clean leftovers from previous runs (idempotent re-runs; errors ignored).
curl -s -o /dev/null -X POST -H "apikey: $AGENT_KEY" -H 'Content-Type: application/json' -d '{"paths":["/d2upload.txt","/d7.bin","/c2a/test.txt"]}' "$BASE/v1/delete"

# D1: /v1/me describes the key with drives, root, perms, policy.
c=$(code -H "apikey: $AGENT_KEY" "$BASE/v1/me")
if [ "$c" = 200 ] && body | grep -q '"baidu-main"' && body | grep -q '"/acc-test"' && body | grep -q '"policy_text"'; then
  ok "D1 /v1/me shape"
else bad "D1" "code=$c body=$(body | head -c 200)"; fi

# D2: default-drive upload with only ?path= (curl -T raw body).
PAYLOAD="acceptance-d-payload-123456789"
c=$(code -X PUT -H "apikey: $AGENT_KEY" -H 'Content-Type: application/octet-stream' --data-binary "$PAYLOAD" "$BASE/v1/files?path=/d2upload.txt")
UP_CRC=$(body | sed -n 's/.*"crc64":"\([0-9]*\)".*/\1/p')
UP_SHA=$(body | sed -n 's/.*"sha256":"\([0-9a-f]*\)".*/\1/p')
if [ "$c" = 200 ] && [ -n "$UP_CRC" ] && [ -n "$UP_SHA" ]; then ok "D2 upload via default drive"; else bad "D2" "code=$c body=$(body | head -c 200)"; fi

# D3: download content matches the uploaded payload.
c=$(code -H "apikey: $AGENT_KEY" "$BASE/v1/files?path=/d2upload.txt")
if [ "$c" = 200 ] && [ "$(body)" = "$PAYLOAD" ]; then ok "D3 download content match"; else bad "D3" "code=$c body=$(body | head -c 100)"; fi

# D4: server CRC64 matches a locally computed CRC-64/XZ (pure python).
WANT_CRC=$(crc64_py "$PAYLOAD")
if [ "$UP_CRC" = "$WANT_CRC" ]; then ok "D4 CRC64 matches local CRC-64/XZ ($WANT_CRC)"; else bad "D4" "server=$UP_CRC local=$WANT_CRC"; fi

# D5: chroot escape attempts -> 400 path_invalid.
c=$(code -H "apikey: $AGENT_KEY" "$BASE/v1/list?path=/../")
[ "$c" = 400 ] && ok "D5a /../ -> 400" || bad "D5a" "code=$c"
c=$(code -H "apikey: $AGENT_KEY" "$BASE/v1/stat?path=/../../hello.txt")
E=$(body | sed -n 's/.*"code":"\([a-z_]*\)".*/\1/p')
[ "$c" = 400 ] && [ "$E" = "path_invalid" ] && ok "D5b escape root -> 400 path_invalid" || bad "D5b" "code=$c err=$E"
c=$(code -H "apikey: $AGENT_KEY" --path-as-is "$BASE/v1/list?path=%2e%2e")
[ "$c" = 400 ] && ok "D5c encoded dotdot -> 400" || bad "D5c" "code=$c"

# D6: delete -> trash -> restore roundtrip.
c=$(code -X POST -H "apikey: $AGENT_KEY" -H 'Content-Type: application/json' -d '{"paths":["/d2upload.txt"]}' "$BASE/v1/delete")
TRASH_ID=$(body | sed -n 's/.*"trash_id":\([0-9]*\).*/\1/p')
if [ "$c" = 200 ] && [ -n "$TRASH_ID" ]; then ok "D6a delete -> trash (id=$TRASH_ID)"; else bad "D6a" "code=$c body=$(body | head -c 200)"; fi
c=$(code -H "apikey: $AGENT_KEY" "$BASE/v1/stat?path=/d2upload.txt")
[ "$c" = 404 ] && ok "D6b file gone after delete" || bad "D6b" "code=$c"
c=$(code -H "apikey: $AGENT_KEY" "$BASE/v1/trash")
if [ "$c" = 200 ] && body | grep -q "/d2upload.txt"; then ok "D6c trash listing"; else bad "D6c" "code=$c body=$(body | head -c 200)"; fi
c=$(code -X POST -H "apikey: $AGENT_KEY" -H 'Content-Type: application/json' -d "{\"id\":$TRASH_ID}" "$BASE/v1/trash/restore")
[ "$c" = 200 ] && ok "D6d restore" || bad "D6d" "code=$c body=$(body | head -c 200)"
c=$(code -H "apikey: $AGENT_KEY" "$BASE/v1/files?path=/d2upload.txt")
if [ "$c" = 200 ] && [ "$(body)" = "$PAYLOAD" ]; then ok "D6e content survives trash roundtrip"; else bad "D6e" "code=$c"; fi

# D7: idempotent retry — same Idempotency-Key replays the stored response
# without a second write (upload body differs but the stored one returns).
c=$(code -X PUT -H "apikey: $AGENT_KEY" -H 'Content-Type: application/octet-stream' -H 'Idempotency-Key: acc-d7' --data-binary "first-write" "$BASE/v1/files?path=/d7.bin")
c2=$(code -X PUT -H "apikey: $AGENT_KEY" -H 'Content-Type: application/octet-stream' -H 'Idempotency-Key: acc-d7' --data-binary "second-write" "$BASE/v1/files?path=/d7.bin")
R1_SHA=$(body | sed -n 's/.*"sha256":"\([0-9a-f]*\)".*/\1/p')
c3=$(code -H "apikey: $AGENT_KEY" "$BASE/v1/files?path=/d7.bin")
if [ "$c" = 200 ] && [ "$c2" = 200 ] && [ "$(body)" != "second-write" ] && [ "$(body)" = "first-write" ]; then
  ok "D7 idempotent retry replays first response"
else bad "D7" "codes=$c/$c2/$c3 body=$(body | head -c 100)"; fi

# D8: link download without a key + expired link -> 410.
c=$(code -X POST -H "apikey: $AGENT_KEY" -H 'Content-Type: application/json' -d '{"path":"/d2upload.txt","expires_in":3600}' "$BASE/v1/links")
LURL=$(body | sed -n 's/.*"url":"\([^"]*\)".*/\1/p')
if [ "$c" = 200 ] && [ -n "$LURL" ]; then
  c=$(code "$LURL")
  if [ "$c" = 200 ] && [ "$(body)" = "$PAYLOAD" ]; then ok "D8a link download without key"; else bad "D8a" "code=$c"; fi
else bad "D8a" "link create code=$c body=$(body | head -c 200)"; fi
c=$(code -X POST -H "apikey: $AGENT_KEY" -H 'Content-Type: application/json' -d '{"path":"/d2upload.txt","expires_in":1}' "$BASE/v1/links")
LURL2=$(body | sed -n 's/.*"url":"\([^"]*\)".*/\1/p')
sleep 2
c=$(code "$LURL2")
[ "$c" = 410 ] && ok "D8b expired link -> 410" || bad "D8b" "code=$c"

# D9: revoke the test key -> 401 everywhere.
c=$(code -b /tmp/acc_djar -X DELETE "$BASE/admin/api/keys/acc-test")
[ "$c" = 200 ] && ok "D9a admin revoked test key" || bad "D9a" "code=$c"
c=$(code -H "apikey: $AGENT_KEY" "$BASE/v1/me")
[ "$c" = 401 ] && ok "D9b revoked key -> 401" || bad "D9b" "code=$c"

# D10: c2a.py helper — put/get/ls/me against the stack.
# A fresh short-lived key for the helper (revoked after).
HELPERREQ='{"id":"acc-helper","default_drive":"baidu-main","drives":[{"drive":"baidu-main","root":"/acc-test","perms":["list","read","write","delete"]}],"trash":{"enabled":true,"days":7}}'
c=$(code -b /tmp/acc_djar -X POST "$BASE/admin/api/keys" -H 'Content-Type: application/json' -d "$HELPERREQ")
HELPER_KEY=$(body | sed -n 's/.*"plaintext":"\([^"]*\)".*/\1/p')
if [ "$c" = 200 ] && [ -n "$HELPER_KEY" ]; then
  curl -s -H "apikey: $HELPER_KEY" "$BASE/custom/c2a.py" -o /tmp/acc_c2a.py
  chmod +x /tmp/acc_c2a.py
  export C2A_URL="$BASE" C2A_KEY="$HELPER_KEY"
  if /tmp/acc_c2a.py me >/dev/null 2>/tmp/acc_c2a_err; then ok "D10a c2a.py me"; else bad "D10a" "$(head -c 150 /tmp/acc_c2a_err)"; fi
  echo "c2a helper payload abc" > /tmp/acc_c2a_local.txt
  if /tmp/acc_c2a.py put /tmp/acc_c2a_local.txt /c2a/test.txt >/dev/null 2>/tmp/acc_c2a_err; then ok "D10b c2a.py put (sha256 verified)"; else bad "D10b" "$(head -c 150 /tmp/acc_c2a_err)"; fi
  if /tmp/acc_c2a.py get /c2a/test.txt /tmp/acc_c2a_dl.txt >/dev/null 2>/tmp/acc_c2a_err && cmp -s /tmp/acc_c2a_local.txt /tmp/acc_c2a_dl.txt; then ok "D10c c2a.py get (sha256 verified)"; else bad "D10c" "$(head -c 150 /tmp/acc_c2a_err)"; fi
  if /tmp/acc_c2a.py ls / >/dev/null 2>/tmp/acc_c2a_err; then ok "D10d c2a.py ls"; else bad "D10d" "$(head -c 150 /tmp/acc_c2a_err)"; fi
  curl -s -b /tmp/acc_djar -o /dev/null -X DELETE "$BASE/admin/api/keys/acc-helper"
else
  bad "D10" "helper key create failed code=$c body=$(body | head -c 150)"
fi

# D11: the static LAN key keeps working on /v1 (default = first granted drive).
c=$(code -H "apikey: $KEY" "$BASE/v1/list?path=/")
if [ "$c" = 200 ]; then ok "D11 static LAN key works on /v1/list"; else bad "D11" "code=$c body=$(body | head -c 150)"; fi

# D12: openapi documents every /v1 route and the SPA serves.
c=$(code "$BASE/apidocumentation/openapi.json")
if [ "$c" = 200 ] && body | grep -q '"/v1/me"' && body | grep -q '"/v1/drives/{drive}/files"'; then ok "D12a openapi.json"; else bad "D12a" "code=$c"; fi
c=$(code "$BASE/custom/agent-instructions.txt")
if [ "$c" = 200 ] && body | grep -q "apikey"; then ok "D12b agent-instructions.txt"; else bad "D12b" "code=$c"; fi
c=$(code "$BASE/healthz"); [ "$c" = 200 ] && ok "D12c healthz" || bad "D12c" "code=$c"
c=$(code "$BASE/readyz"); [ "$c" = 200 ] && ok "D12d readyz" || bad "D12d" "code=$c"


# ============================================================================
# Group E — Agent autonomy (v0.4 Phase 1b, UPGRADE §4.10). Uses a dedicated
# test key with transfer/fetch/delegate grants on baidu-main + pan123-main.
# ============================================================================
echo "== P5 agent autonomy =="

EREQ='{"id":"acc-e","default_drive":"baidu-main","drives":[{"drive":"baidu-main","root":"/acc-e","perms":["list","read","write","delete","link","transfer","fetch"]},{"drive":"pan123-main","root":"/acc-e","perms":["list","read","write","delete","link","transfer","fetch"]}],"global_perms":["link","transfer","fetch","delegate"],"trash":{"enabled":true,"days":7}}'
# Idempotent re-runs: revoke leftovers first, then create fresh.
curl -s -b /tmp/acc_djar -o /dev/null -X DELETE "$BASE/admin/api/keys/acc-e"
curl -s -b /tmp/acc_djar -o /dev/null -X DELETE "$BASE/admin/api/keys/acc-e-child"
c=$(code -b /tmp/acc_djar -X POST "$BASE/admin/api/keys" -H 'Content-Type: application/json' -d "$EREQ")
EKEY=$(body | sed -n 's/.*"plaintext":"\([^"]*\)".*/\1/p')
if [ "$c" = 200 ] && [ -n "$EKEY" ]; then ok "E0 test key created"; else bad "E0" "code=$c body=$(body | head -c 200)"; fi
EHDR="apikey: $EKEY"

# E0z: cleanup artifacts from previous runs (errors ignored).
curl -s -o /dev/null -X POST -H "$EHDR" -H 'Content-Type: application/json' \
  -d '{"paths":["/e-payload.bin","/e-dirtree","/fetched","/research-nots.txt","/research-notes.txt","/e-dir"]}' \
  "$BASE/v1/delete"
curl -s -o /dev/null -X POST -H "$EHDR" -H 'Content-Type: application/json' \
  -d '{"paths":["/e-payload.bin","/fetched/e-payload.bin","/e-dir"]}' \
  "$BASE/v1/drives/pan123-main/delete"

# E1: /v1 index lists endpoints; /v1/me suggests the selftest before it ran.
c=$(code -H "$EHDR" "$BASE/v1")
if [ "$c" = 200 ] && body | grep -q '"/v1/selftest"' && body | grep -q '"/v1/jobs/{id}"' && body | grep -q '"quickstart"'; then
  ok "E1a /v1 index endpoint links"
else bad "E1a" "code=$c body=$(body | head -c 150)"; fi
c=$(code -H "$EHDR" "$BASE/v1/me")
if [ "$c" = 200 ] && body | grep -q 'POST /v1/selftest'; then
  ok "E1b /v1/me next_steps suggests selftest"
else bad "E1b" "code=$c body=$(body | head -c 300)"; fi

# E2: selftest roundtrips every writable drive.
c=$(code -X POST -H "$EHDR" "$BASE/v1/selftest")
if [ "$c" = 200 ] && body | grep -q '"all_ok":true' && body | grep -q '"baidu-main"' && body | grep -q '"pan123-main"'; then
  ok "E2a selftest all_ok"
else bad "E2a" "code=$c body=$(body | head -c 300)"; fi
c=$(code -H "$EHDR" "$BASE/v1/me")
if [ "$c" = 200 ] && body | grep -q '"last_selftest_at":"2'; then
  ok "E2b last_selftest_at recorded"
else bad "E2b" "code=$c body=$(body | head -c 300)"; fi

# E3: cross-drive transfer via job + long-poll -> succeeded, content matches.
EPAYLOAD="e-group-transfer-payload-0123456789abcdef"
c=$(code -X PUT -H "$EHDR" -H 'Content-Type: application/octet-stream' --data-binary "$EPAYLOAD" "$BASE/v1/files?path=/e-payload.bin")
[ "$c" = 200 ] && ok "E3a upload source file" || bad "E3a" "code=$c body=$(body | head -c 150)"
c=$(code -X POST -H "$EHDR" -H 'Content-Type: application/json' \
  -d '{"from":{"drive":"baidu-main","path":"/e-payload.bin"},"to":{"drive":"pan123-main","path":"/e-payload.bin"}}' \
  "$BASE/v1/transfers")
JID=$(body | sed -n 's/.*"id":"\(job-[0-9a-f]*\)".*/\1/p')
if [ "$c" = 202 ] && [ -n "$JID" ]; then ok "E3b transfer -> 202 job"; else bad "E3b" "code=$c body=$(body | head -c 200)"; fi
JSTATE=""
for i in $(seq 1 12); do
  c=$(code -H "$EHDR" "$BASE/v1/jobs/$JID?wait=5")
  JSTATE=$(body | sed -n 's/.*"state":"\([a-z]*\)".*/\1/p')
  [ "$JSTATE" = "succeeded" ] && break
  [ "$JSTATE" = "failed" ] && break
  [ "$JSTATE" = "canceled" ] && break
done
if [ "$JSTATE" = "succeeded" ]; then ok "E3c transfer job succeeded (long-poll)"; else bad "E3c" "state=$JSTATE body=$(body | head -c 200)"; fi
c=$(code -H "$EHDR" "$BASE/v1/drives/pan123-main/files?path=/e-payload.bin")
if [ "$c" = 200 ] && [ "$(body)" = "$EPAYLOAD" ]; then ok "E3d transferred content matches"; else bad "E3d" "code=$c body=$(body | head -c 100)"; fi
c=$(code -H "$EHDR" "$BASE/v1/jobs?state=succeeded")
if [ "$c" = 200 ] && body | grep -q "$JID"; then ok "E3e jobs listing by state"; else bad "E3e" "code=$c"; fi

# E4: fetch via /v1/links URL (openlist shares the netns with cloud2api).
c=$(code -X POST -H "$EHDR" -H 'Content-Type: application/json' -d '{"path":"/e-payload.bin","expires_in":900}' "$BASE/v1/links")
LURL=$(body | sed -n 's/.*"url":"\([^"]*\)".*/\1/p')
if [ "$c" = 200 ] && [ -n "$LURL" ]; then ok "E4a link created for source"; else bad "E4a" "code=$c body=$(body | head -c 200)"; fi
c=$(code -X POST -H "$EHDR" -H 'Content-Type: application/json' \
  -d "{\"url\":\"$LURL\",\"drive\":\"pan123-main\",\"path\":\"/fetched/e-payload.bin\"}" "$BASE/v1/fetch")
FID=$(body | sed -n 's/.*"id":"\(job-[0-9a-f]*\)".*/\1/p')
if [ "$c" = 202 ] && [ -n "$FID" ]; then ok "E4b fetch -> 202 job"; else bad "E4b" "code=$c body=$(body | head -c 200)"; fi
FSTATE=""
for i in $(seq 1 12); do
  c=$(code -H "$EHDR" "$BASE/v1/jobs/$FID?wait=5")
  FSTATE=$(body | sed -n 's/.*"state":"\([a-z]*\)".*/\1/p')
  [ "$FSTATE" = "succeeded" ] && break
  [ "$FSTATE" = "failed" ] && break
done
if [ "$FSTATE" = "succeeded" ]; then ok "E4c fetch job succeeded"; else bad "E4c" "state=$FSTATE body=$(body | head -c 300)"; fi
c=$(code -H "$EHDR" "$BASE/v1/drives/pan123-main/files?path=/fetched/e-payload.bin")
FETCHED_SHA=$(printf '%s' "$EPAYLOAD" | sha256sum | cut -d' ' -f1)
if [ "$c" = 200 ] && [ "$(body)" = "$EPAYLOAD" ]; then ok "E4d fetched content matches (sha256 $FETCHED_SHA)"; else bad "E4d" "code=$c body=$(body | head -c 100)"; fi

# E5: dry-run delete returns counts and deletes nothing.
curl -s -o /dev/null -X POST -H "$EHDR" -H 'Content-Type: application/json' -d '{"path":"/e-dir"}' "$BASE/v1/mkdir"
curl -s -o /dev/null -X PUT -H "$EHDR" -H 'Content-Type: application/octet-stream' --data-binary 'e5' "$BASE/v1/files?path=/e-dir/a.bin"
curl -s -o /dev/null -X PUT -H "$EHDR" -H 'Content-Type: application/octet-stream' --data-binary 'e5' "$BASE/v1/files?path=/e-dir/b.bin"
c=$(code -X POST -H "$EHDR" -H 'Content-Type: application/json' -d '{"paths":["/e-dir"],"dry_run":true}' "$BASE/v1/delete")
if [ "$c" = 200 ] && body | grep -q '"files":2' && body | grep -q '"dirs":1' && body | grep -q '"bytes":4'; then
  ok "E5a dry-run delete reports counts"
else bad "E5a" "code=$c body=$(body | head -c 300)"; fi
c=$(code -H "$EHDR" "$BASE/v1/stat?path=/e-dir/a.bin")
[ "$c" = 200 ] && ok "E5b dry-run deleted nothing" || bad "E5b" "code=$c"

# E6: typo path returns suggestions.
c=$(code -X PUT -H "$EHDR" -H 'Content-Type: application/octet-stream' --data-binary 'e6' "$BASE/v1/files?path=/research-notes.txt")
c=$(code -H "$EHDR" "$BASE/v1/stat?path=/research-nots.txt")
if [ "$c" = 404 ] && body | grep -q '"suggestions":\["/research-notes.txt"\]'; then
  ok "E6 typo path -> suggestion"
else bad "E6" "code=$c body=$(body | head -c 300)"; fi

# E9: child keys — subset ok, superset 403, root escape 403, child works.
c=$(code -X POST -H "$EHDR" -H 'Content-Type: application/json' \
  -d '{"id":"acc-e-child","drives":[{"drive":"baidu-main","root":"/acc-e/sub","perms":["list","read"]}],"ttl_seconds":3600}' "$BASE/v1/keys")
CHILD=$(body | sed -n 's/.*"plaintext":"\([^"]*\)".*/\1/p')
if [ "$c" = 200 ] && [ -n "$CHILD" ]; then ok "E9a child key created (subset)"; else bad "E9a" "code=$c body=$(body | head -c 200)"; fi
curl -s -o /dev/null -X POST -H "$EHDR" -H 'Content-Type: application/json' -d '{"path":"/sub"}' "$BASE/v1/mkdir"
c=$(code -H "apikey: $CHILD" "$BASE/v1/list?path=/")
[ "$c" = 200 ] && ok "E9b child key works" || bad "E9b" "code=$c"
c=$(code -H "apikey: $CHILD" "$BASE/v1/list?path=/../")
[ "$c" = 400 ] && ok "E9c child cannot escape its root" || bad "E9c" "code=$c"
c=$(code -X POST -H "$EHDR" -H 'Content-Type: application/json' \
  -d '{"drives":[{"drive":"baidu-main","root":"/acc-e","perms":["purge"]}]}' "$BASE/v1/keys")
[ "$c" = 403 ] && ok "E9d child with superset perms -> 403" || bad "E9d" "code=$c body=$(body | head -c 150)"
c=$(code -X POST -H "$EHDR" -H 'Content-Type: application/json' \
  -d '{"drives":[{"drive":"baidu-main","root":"/outside","perms":["read"]}]}' "$BASE/v1/keys")
[ "$c" = 403 ] && ok "E9e child root outside parent -> 403" || bad "E9e" "code=$c body=$(body | head -c 150)"
c=$(code -X POST -H "$EHDR" -H 'Content-Type: application/json' \
  -d '{"drives":[{"drive":"aliyun-main","root":"/","perms":["read"]}]}' "$BASE/v1/keys")
[ "$c" = 403 ] && ok "E9f child drive not granted to parent -> 403" || bad "E9f" "code=$c"
c=$(code -H "$EHDR" "$BASE/v1/keys")
if [ "$c" = 200 ] && body | grep -q 'acc-e-child'; then ok "E9g child keys listed"; else bad "E9g" "code=$c"; fi

# E7: removed approval surfaces cannot grant the key access.
c=$(code -X POST -H "$EHDR" -H 'Content-Type: application/json' \
  -d '{"type":"drive_access","drive":"aliyun-main","perms":["list","read"]}' "$BASE/v1/requests")
[ "$c" = 404 ] && ok "E7a agent request route removed" || bad "E7a" "code=$c"
c=$(code -H "$EHDR" "$BASE/v1/requests/1")
[ "$c" = 404 ] && ok "E7b agent request status removed" || bad "E7b" "code=$c"
c=$(code -b /tmp/acc_djar "$BASE/admin/api/requests?state=pending")
[ "$c" = 404 ] && ok "E7c admin request listing removed" || bad "E7c" "code=$c"
c=$(code -b /tmp/acc_djar -X POST "$BASE/admin/api/requests/1/approve" -H 'Content-Type: application/json' \
  -d '{"grant":{"drive":"aliyun-main","perms":["list","read"],"root":"/"}}')
[ "$c" = 404 ] && ok "E7d admin approval removed" || bad "E7d" "code=$c"
c=$(code -H "$EHDR" "$BASE/v1/drives/aliyun-main/list?path=/")
[ "$c" = 403 ] && ok "E7e removed approval cannot extend grants" || bad "E7e" "code=$c"

# E8: overview no longer advertises an approval queue.
c=$(code -b /tmp/acc_djar "$BASE/admin/api/overview")
if [ "$c" = 200 ] && ! body | grep -q 'pending_requests'; then ok "E8 no pending request counter"; else bad "E8" "code=$c"; fi

# E10: activity lists recent ops of this key (and no key material).
c=$(code -H "$EHDR" "$BASE/v1/activity?limit=20")
if [ "$c" = 200 ] && body | grep -q '"upload"' && ! body | grep -q "sk-"; then
  ok "E10 activity lists ops"
else bad "E10" "code=$c body=$(body | head -c 200)"; fi

# E11: tree, search and batch smoke.
c=$(code -X POST -H "$EHDR" -H 'Content-Type: application/json' -d '{"path":"/e-tree"}' "$BASE/v1/mkdir")
curl -s -o /dev/null -X PUT -H "$EHDR" -H 'Content-Type: application/octet-stream' --data-binary 'e11' "$BASE/v1/files?path=/e-tree/needle.txt"
c=$(code -H "$EHDR" "$BASE/v1/tree?path=/&depth=3")
if [ "$c" = 200 ] && body | grep -q '/e-tree/needle.txt'; then ok "E11a tree"; else bad "E11a" "code=$c body=$(body | head -c 200)"; fi
c=$(code -H "$EHDR" "$BASE/v1/search?q=needle&path=/")
if [ "$c" = 200 ] && body | grep -q '/e-tree/needle.txt'; then ok "E11b search"; else bad "E11b" "code=$c body=$(body | head -c 200)"; fi
c=$(code -X POST -H "$EHDR" -H 'Content-Type: application/json' \
  -d '{"ops":[{"op":"mkdir","path":"/e-batch"},{"op":"move","from":"/e-tree/needle.txt","to":"/e-batch/needle.txt"}]}' "$BASE/v1/batch")
if [ "$c" = 200 ] && body | grep -q '"status":"ok"'; then ok "E11c batch sync ops"; else bad "E11c" "code=$c body=$(body | head -c 300)"; fi

# E12: c2a.py new subcommands smoke.
curl -s -H "$EHDR" "$BASE/custom/c2a.py" -o /tmp/acc_e_c2a.py
chmod +x /tmp/acc_e_c2a.py
export C2A_URL="$BASE" C2A_KEY="$EKEY"
if /tmp/acc_e_c2a.py selftest >/dev/null 2>/tmp/acc_e_err; then ok "E12a c2a.py selftest"; else bad "E12a" "$(head -c 150 /tmp/acc_e_err)"; fi
if /tmp/acc_e_c2a.py tree / --depth 2 >/dev/null 2>/tmp/acc_e_err; then ok "E12b c2a.py tree"; else bad "E12b" "$(head -c 150 /tmp/acc_e_err)"; fi
if /tmp/acc_e_c2a.py search needle >/tmp/acc_e_out 2>/tmp/acc_e_err && grep -q needle /tmp/acc_e_out; then ok "E12c c2a.py search"; else bad "E12c" "$(head -c 150 /tmp/acc_e_err)"; fi
if /tmp/acc_e_c2a.py rm /e-batch/needle.txt --dry-run >/tmp/acc_e_out 2>/tmp/acc_e_err && grep -q "would affect" /tmp/acc_e_out; then ok "E12d c2a.py rm --dry-run"; else bad "E12d" "$(head -c 150 /tmp/acc_e_err)"; fi
echo "c12 helper payload" > /tmp/acc_e_local.txt
if /tmp/acc_e_c2a.py put /tmp/acc_e_local.txt /e-c12.bin >/dev/null 2>/tmp/acc_e_err; then ok "E12e c2a.py put"; else bad "E12e" "$(head -c 150 /tmp/acc_e_err)"; fi
if /tmp/acc_e_c2a.py cp /e-c12.bin /e-c12-copy.bin --to-drive pan123-main --wait 60 >/tmp/acc_e_out 2>/tmp/acc_e_err && grep -q "state=succeeded" /tmp/acc_e_out; then
  ok "E12f c2a.py cp cross-drive via job"
else bad "E12f" "$(head -c 200 /tmp/acc_e_out; head -c 150 /tmp/acc_e_err)"; fi
if /tmp/acc_e_c2a.py jobs ls >/tmp/acc_e_out 2>/tmp/acc_e_err && grep -q "job-" /tmp/acc_e_out; then ok "E12g c2a.py jobs ls"; else bad "E12g" "$(head -c 150 /tmp/acc_e_err)"; fi
if /tmp/acc_e_c2a.py keys ls >/tmp/acc_e_out 2>/tmp/acc_e_err && grep -q "acc-e-child" /tmp/acc_e_out; then ok "E12h c2a.py keys ls"; else bad "E12h" "$(head -c 150 /tmp/acc_e_err)"; fi
if /tmp/acc_e_c2a.py --help >/tmp/acc_e_out 2>/tmp/acc_e_err && ! grep -q '\brequest\b' /tmp/acc_e_out; then ok "E12i c2a.py has no approval command"; else bad "E12i" "helper still advertises approval"; fi
if /tmp/acc_e_c2a.py activity --limit 30 >/tmp/acc_e_out 2>/tmp/acc_e_err && grep -q "upload" /tmp/acc_e_out; then ok "E12j c2a.py activity"; else bad "E12j" "$(head -c 150 /tmp/acc_e_err)"; fi

# E13: cleanup test artifacts (trash-safe) and revoke the test keys.
curl -s -o /dev/null -X POST -H "$EHDR" -H 'Content-Type: application/json' \
  -d '{"paths":["/e-payload.bin","/e-dir","/e-tree","/e-batch","/e-c12.bin","/research-notes.txt"]}' "$BASE/v1/delete"
curl -s -o /dev/null -X POST -H "$EHDR" -H 'Content-Type: application/json' \
  -d '{"paths":["/e-payload.bin","/fetched","/e-c12-copy.bin"]}' "$BASE/v1/drives/pan123-main/delete"
curl -s -o /dev/null -b /tmp/acc_djar -X DELETE "$BASE/admin/api/keys/acc-e-child"
c=$(code -b /tmp/acc_djar -X DELETE "$BASE/admin/api/keys/acc-e")
[ "$c" = 200 ] && ok "E13 cleanup + revoke test key" || bad "E13" "code=$c"



# ============================================================================
# Group G — OpenList configuration linkage (v0.4 Phase 1c, UPGRADE §3.7).
# Talks to the OpenList admin API directly (127.0.0.1:5244) to simulate a user
# editing storages inside OpenList. Runs BEFORE C7 (the login limiter would
# block its portal calls) and before F.
# ============================================================================
echo "== P7 openlist linkage =="

if [ -n "$OL_TOKEN" ]; then
  # G0z: leftovers from previous runs (errors ignored).
  curl -s -o /dev/null -b /tmp/acc_djar -X DELETE "$BASE/admin/api/keys/acc-g"
  curl -s -o /dev/null -b /tmp/acc_djar -X DELETE "$BASE/admin/api/drives/g-link"
  curl -s -o /dev/null -b /tmp/acc_djar -X DELETE "$BASE/admin/api/drives/g-adopt"
  for m in /g-link /g-link-2 /g-manual; do
    sid=$(ol_sid_by_mount "$m")
    [ -n "$sid" ] && curl -s -o /dev/null -X POST -H "Authorization: $OL_TOKEN" "$OL/api/admin/storage/delete?id=$sid"
  done

  # G0: storage created in OpenList and adopted + dedicated key + upload.
  GSID=$(ol_mk_local /g-link /mnt/portal-test)
  c=$(code -b /tmp/acc_djar -X POST "$BASE/admin/api/drives/adopt" -H 'Content-Type: application/json' -d "{\"storage_id\":${GSID:-0},\"id\":\"g-link\"}")
  [ "$c" = 200 ] && ok "G0a OpenList storage adopted as g-link" || bad "G0a" "sid=$GSID code=$c body=$(body | head -c 200)"
  GREQ='{"id":"acc-g","default_drive":"g-link","drives":[{"drive":"g-link","root":"/","perms":["list","read","write","delete"]}],"trash":{"enabled":false}}'
  c=$(code -b /tmp/acc_djar -X POST "$BASE/admin/api/keys" -H 'Content-Type: application/json' -d "$GREQ")
  GKEY=$(body | sed -n 's/.*"plaintext":"\([^"]*\)".*/\1/p')
  [ "$c" = 200 ] && [ -n "$GKEY" ] && ok "G0b test key acc-g" || bad "G0b" "code=$c body=$(body | head -c 200)"
  c=$(code -X PUT -H "apikey: $GKEY" -H 'Content-Type: application/octet-stream' --data-binary "g-group-payload-123" "$BASE/v1/files?path=/g1.txt&on_conflict=overwrite")
  [ "$c" = 200 ] && ok "G0c upload through /v1" || bad "G0c" "code=$c body=$(body | head -c 200)"

  # G1: rename the mount in OpenList → cloud2api follows, /v1 keeps working.
  GSID=$(ol_sid_by_mount /g-link)
  olpost /api/admin/storage/update "$(ol_storage_json "$GSID" | python3 -c "
import json, sys
d = json.load(sys.stdin)['data']; d['mount_path'] = '/g-link-2'
print(json.dumps(d))")" >/dev/null
  c=$(code -H "apikey: $GKEY" "$BASE/v1/files?path=/g1.txt")
  if [ "$c" = 200 ] && [ "$(body)" = "g-group-payload-123" ]; then ok "G1a /v1 works after OpenList mount rename"; else bad "G1a" "code=$c body=$(body | head -c 150)"; fi
  c=$(code -b /tmp/acc_djar "$BASE/admin/api/drives")
  if [ "$c" = 200 ] && body | python3 -c '
import json, sys
rows = json.load(sys.stdin)["data"]["drives"]
d = next(r for r in rows if r["id"] == "g-link")
sys.exit(0 if d["mount_path"] == "/g-link-2" and d["state"] == "healthy" else 1)'; then
    ok "G1b drives row followed the rename"
  else bad "G1b" "code=$c body=$(body | head -c 300)"; fi
  c=$(code -b /tmp/acc_djar "$BASE/admin/api/events?limit=20")
  if [ "$c" = 200 ] && body | python3 -c '
import json, sys
evs = json.load(sys.stdin)["data"]["events"]
sys.exit(0 if any(e["kind"] == "mount_renamed" and e["drive"] == "g-link" for e in evs) else 1)'; then
    ok "G1c mount_renamed event recorded"
  else bad "G1c" "code=$c body=$(body | head -c 300)"; fi

  # G2: the drive row links into the OpenList storage editor; customized
  # tracking is gone.
  OLPUB=$(curl -s -b /tmp/acc_djar "$BASE/admin/api/settings" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"].get("openlist_public_url",""))')
  c=$(code -b /tmp/acc_djar "$BASE/admin/api/drives")
  if [ "$c" = 200 ] && body | python3 -c "
import json, sys
d = next(r for r in json.load(sys.stdin)['data']['drives'] if r['id'] == 'g-link')
want = ('$OLPUB'.rstrip('/') + '/@manage/storages/edit/$GSID') if '$OLPUB' else ''
sys.exit(0 if d['openlist_edit_url'] == want and 'customized_fields' not in d
         and d['enabled'] is True and d['storage_disabled'] is False else 1)"; then
    ok "G2 drives row has the OpenList edit link, enabled/storage_disabled, no customized_fields"
  else bad "G2" "code=$c pub=$OLPUB body=$(body | head -c 300)"; fi

  # G3: editing the storage in place in OpenList keeps the mapping; the portal
  # settings endpoints are gone.
  olpost /api/admin/storage/update "$(ol_storage_json "$GSID" | python3 -c "
import json, sys
d = json.load(sys.stdin)['data']; d['cache_expiration'] = 99; d['remark'] = 'edited-in-openlist'
print(json.dumps(d))")" >/dev/null
  c=$(code -H "apikey: $GKEY" "$BASE/v1/files?path=/g1.txt")
  [ "$c" = 200 ] && ok "G3a in-place OpenList edit keeps the drive working" || bad "G3a" "code=$c body=$(body | head -c 150)"
  c=$(code -b /tmp/acc_djar "$BASE/admin/api/drives/g-link/settings")
  [ "$c" = 404 ] && ok "G3b drive settings endpoint removed" || bad "G3b" "code=$c"

  # G4: the instance settings no longer carry oplist_api_base.
  c=$(code -b /tmp/acc_djar "$BASE/admin/api/settings")
  [ "$c" = 200 ] && ! body | grep -q oplist_api_base && ok "G4a settings without oplist_api_base" || bad "G4a" "code=$c body=$(body | head -c 200)"
  c=$(code -b /tmp/acc_djar -X PUT "$BASE/admin/api/settings" -H 'Content-Type: application/json' -d '{"oplist_api_base":"https://api-cn.oplist.org"}')
  [ "$c" = 400 ] && ok "G4b PUT oplist_api_base -> 400" || bad "G4b" "code=$c body=$(body | head -c 200)"

  # G6: disable in OpenList → /v1 503 drive_unavailable; enable → recovery.
  olpost "/api/admin/storage/disable?id=$GSID" >/dev/null
  c=$(code -H "apikey: $GKEY" "$BASE/v1/files?path=/g1.txt")
  E=$(body | sed -n 's/.*"code":"\([a-z_]*\)".*/\1/p')
  if [ "$c" = 503 ] && [ "$E" = "drive_unavailable" ]; then ok "G6a disabled in OpenList → 503 drive_unavailable"; else bad "G6a" "code=$c err=$E body=$(body | head -c 150)"; fi
  olpost "/api/admin/storage/enable?id=$GSID" >/dev/null
  c=$(code -H "apikey: $GKEY" "$BASE/v1/files?path=/g1.txt")
  [ "$c" = 200 ] && ok "G6b re-enabled → /v1 recovers" || bad "G6b" "code=$c body=$(body | head -c 150)"
  # G6c: while disabled in OpenList the row says so (enabled stays true).
  olpost "/api/admin/storage/disable?id=$GSID" >/dev/null
  c=$(code -b /tmp/acc_djar "$BASE/admin/api/drives")
  if [ "$c" = 200 ] && body | python3 -c '
import json, sys
d = next(r for r in json.load(sys.stdin)["data"]["drives"] if r["id"] == "g-link")
sys.exit(0 if d["state"] == "disabled" and d["enabled"] is True and d["storage_disabled"] is True else 1)'; then
    ok "G6c row: storage_disabled=true, enabled=true"
  else bad "G6c" "code=$c body=$(body | head -c 300)"; fi
  olpost "/api/admin/storage/enable?id=$GSID" >/dev/null

  # G6d-i: the portal switch closes cloud2api only; the OpenList storage is
  # not modified (same modified timestamp, still enabled).
  olsnap() { ol_storage_json "$GSID" | python3 -c 'import json,sys; d=json.load(sys.stdin)["data"]; print(d["modified"], d["disabled"])'; }
  SNAP0=$(olsnap)
  c=$(code -b /tmp/acc_djar -X POST "$BASE/admin/api/drives/g-link/disable" -H 'Content-Type: application/json' -d '{}')
  if [ "$c" = 200 ] && body | python3 -c '
import json, sys
d = json.load(sys.stdin)["data"]
sys.exit(0 if d["enabled"] is False and d["state"] == "disabled" and d["storage_disabled"] is False else 1)'; then
    ok "G6d portal disable returns the row (enabled=false)"
  else bad "G6d" "code=$c body=$(body | head -c 200)"; fi
  c=$(code -H "apikey: $GKEY" "$BASE/v1/files?path=/g1.txt")
  E=$(body | sed -n 's/.*"code":"\([a-z_]*\)".*/\1/p')
  [ "$c" = 503 ] && [ "$E" = "drive_unavailable" ] && ok "G6e portal-disabled drive → /v1 503" || bad "G6e" "code=$c err=$E"
  SNAP1=$(olsnap)
  [ -n "$SNAP0" ] && [ "$SNAP1" = "$SNAP0" ] && ok "G6f OpenList storage untouched by portal disable ($SNAP1)" || bad "G6f" "before=$SNAP0 after=$SNAP1"
  c=$(code -b /tmp/acc_djar -X POST "$BASE/admin/api/drives/g-link/enable" -H 'Content-Type: application/json' -d '{}')
  if [ "$c" = 200 ] && body | python3 -c '
import json, sys
d = json.load(sys.stdin)["data"]
sys.exit(0 if d["enabled"] is True and d["state"] == "healthy" else 1)'; then
    ok "G6g portal enable returns the healthy row"
  else bad "G6g" "code=$c body=$(body | head -c 200)"; fi
  c=$(code -H "apikey: $GKEY" "$BASE/v1/files?path=/g1.txt")
  [ "$c" = 200 ] && ok "G6h /v1 recovers after portal enable" || bad "G6h" "code=$c"
  SNAP2=$(olsnap)
  [ "$SNAP2" = "$SNAP0" ] && ok "G6i OpenList storage untouched by portal enable" || bad "G6i" "before=$SNAP0 after=$SNAP2"

  # G7: manual storage in OpenList → unmanaged → adopt → grant → /v1 → release.
  olpost /api/admin/storage/create '{"mount_path":"/g-manual","driver":"Local","addition":"{\"root_folder_path\":\"/mnt/portal-test\",\"thumbnail\":false}","cache_expiration":30,"status":"work"}' >/dev/null
  MSID=$(ol_sid_by_mount /g-manual)
  c=$(code -b /tmp/acc_djar "$BASE/admin/api/drives/unmanaged")
  if [ "$c" = 200 ] && body | grep -q "\"storage_id\":$MSID"; then ok "G7a manual storage listed as unmanaged"; else bad "G7a" "code=$c sid=$MSID body=$(body | head -c 300)"; fi
  c=$(code -b /tmp/acc_djar -X POST "$BASE/admin/api/drives/adopt" -H 'Content-Type: application/json' -d "{\"storage_id\":$MSID,\"id\":\"g-adopt\"}")
  [ "$c" = 200 ] && ok "G7b storage adopted" || bad "G7b" "code=$c body=$(body | head -c 200)"
  c=$(code -b /tmp/acc_djar -X PATCH "$BASE/admin/api/keys/acc-g" -H 'Content-Type: application/json' \
    -d '{"drives":[{"drive":"g-link","root":"/","perms":["list","read","write","delete"]},{"drive":"g-adopt","root":"/","perms":["list","read","write","delete"]}]}')
  [ "$c" = 200 ] && ok "G7c key granted the adopted drive" || bad "G7c" "code=$c body=$(body | head -c 200)"
  c=$(code -H "apikey: $GKEY" "$BASE/v1/drives/g-adopt/list?path=/")
  [ "$c" = 200 ] && ok "G7d adopted drive serves /v1" || bad "G7d" "code=$c body=$(body | head -c 200)"
  c=$(code -b /tmp/acc_djar -X DELETE "$BASE/admin/api/drives/g-adopt")
  [ "$c" = 200 ] && ok "G7e 移出 (no parameter)" || bad "G7e" "code=$c body=$(body | head -c 200)"
  c=$(olget "/api/admin/storage/get?id=$MSID" | sed -n 's/^{"code":\([0-9]*\).*/\1/p')
  [ "$c" = 200 ] && ok "G7f OpenList storage survived 移出" || bad "G7f" "storage/get code=$c"
  c=$(code -b /tmp/acc_djar "$BASE/admin/api/drives/unmanaged")
  body | grep -q "\"storage_id\":$MSID," && ok "G7g storage back in the unmanaged list" || bad "G7g" "body=$(body | head -c 200)"
  olpost "/api/admin/storage/delete?id=$MSID" >/dev/null

  # G8: delete the managed drive's storage in OpenList → orphaned + 503.
  olpost "/api/admin/storage/delete?id=$GSID" >/dev/null
  c=$(code -b /tmp/acc_djar "$BASE/admin/api/drives")
  if [ "$c" = 200 ] && body | python3 -c '
import json, sys
rows = json.load(sys.stdin)["data"]["drives"]
d = next(r for r in rows if r["id"] == "g-link")
sys.exit(0 if d["state"] == "orphaned" else 1)'; then
    ok "G8a drives row orphaned after OpenList delete"
  else bad "G8a" "code=$c body=$(body | head -c 300)"; fi
  c=$(code -H "apikey: $GKEY" "$BASE/v1/files?path=/g1.txt")
  E=$(body | sed -n 's/.*"code":"\([a-z_]*\)".*/\1/p')
  if [ "$c" = 503 ] && [ "$E" = "drive_unavailable" ]; then ok "G8b /v1 503 drive_unavailable"; else bad "G8b" "code=$c err=$E"; fi
  c=$(code -b /tmp/acc_djar "$BASE/admin/api/events?limit=20")
  body | grep -q '"kind":"deleted_in_openlist"' && ok "G8c deleted_in_openlist event" || bad "G8c" "no event"

  # G9: cleanup.
  curl -s -o /dev/null -b /tmp/acc_djar -X DELETE "$BASE/admin/api/keys/acc-g"
  c=$(code -b /tmp/acc_djar -X DELETE "$BASE/admin/api/drives/g-link")
  [ "$c" = 200 ] && ok "G9 orphaned drive removed (移出)" || bad "G9" "code=$c"
else
  bad "G0" "openlist admin login failed (token empty)"
fi


# C7 (v0.3.1): failed-login rate limit — 5 wrong passwords per client IP per
# minute, then 429 with Retry-After. Kept LAST because it blocks further
# logins from this IP for up to a minute.
n401=0; n429=0; last=0
for i in $(seq 1 8); do
  last=$(code -X POST "$BASE/admin/api/login" -H 'Content-Type: application/json' -d '{"password":"definitely-wrong"}')
  case "$last" in
    401) n401=$((n401+1));;
    429) n429=$((n429+1));;
  esac
done
[ "$n401" -le 5 ] && [ "$n429" -ge 1 ] && ok "C7a failed-login rate limit (401x$n401 then 429x$n429)" || bad "C7a" "401x$n401 429x$n429 last=$last"
curl -s -D /tmp/acc_hdr -o /dev/null -X POST "$BASE/admin/api/login" -H 'Content-Type: application/json' -d "{\"password\":\"$PORTAL_PASSWORD\"}"
if grep -qi '^retry-after: [0-9]' /tmp/acc_hdr; then ok "C7b blocked login carries Retry-After"; else bad "C7b" "no Retry-After header"; fi


# ============================================================================
# Group H — Gokapi compatibility layer (v0.4 Phase 1d, UPGRADE §4.7). Follows
# the Gokapi relay agent-instructions literally (curl, apikey header, multipart
# upload, CRC-64/XZ verification) against a dedicated test key. Runs before F
# so the LAN-key invariant stays last.
# ============================================================================
echo "== P8 gokapi compat =="
# Group H scratch space: the host /tmp is a small per-user tmpfs; keep the
# multi-MiB fixtures on the repository filesystem instead.
HSCRATCH="$(cd "$(dirname "$0")/.." && pwd)/.acc-scratch"
mkdir -p "$HSCRATCH"
rm -f "$HSCRATCH"/acc_h_*.bin "$HSCRATCH"/acc_hq*.bin

crc64_file() { # crc64_file <path> -> decimal CRC-64/XZ (pure python)
  python3 - "$1" <<'PYEOF'
import sys
def crc64_file(path):
    poly = 0xC96C5795D7870F42
    table = []
    for i in range(256):
        crc = i
        for _ in range(8):
            crc = (crc >> 1) ^ poly if crc & 1 else crc >> 1
        table.append(crc)
    crc = 0xFFFFFFFFFFFFFFFF
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1024*1024), b""):
            for b in chunk:
                crc = table[(crc ^ b) & 0xFF] ^ (crc >> 8)
    return crc ^ 0xFFFFFFFFFFFFFFFF
print(crc64_file(sys.argv[1]))
PYEOF
}

# H0z: idempotent re-runs — drop leftover keys first (errors ignored).
curl -s -o /dev/null -b /tmp/acc_djar -X DELETE "$BASE/admin/api/keys/acc-h"
curl -s -o /dev/null -b /tmp/acc_djar -X DELETE "$BASE/admin/api/keys/acc-h-quota"
curl -s -o /dev/null -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"dir":"/","names":["acc-h"]}' "$BASE/api/storage/baidu-main/remove"

# H0: dedicated relay test key (relay permission + default relay drive).
HREQ='{"id":"acc-h","default_drive":"baidu-main","drives":[{"drive":"baidu-main","root":"/acc-h","perms":["list","read","write","delete"]}],"global_perms":["relay"],"trash":{"enabled":true,"days":7}}'
c=$(code -b /tmp/acc_djar -X POST "$BASE/admin/api/keys" -H 'Content-Type: application/json' -d "$HREQ")
HKEY=$(body | sed -n 's/.*"plaintext":"\([^"]*\)".*/\1/p')
if [ "$c" = 200 ] && [ -n "$HKEY" ]; then ok "H0a relay test key created"; else bad "H0a" "code=$c body=$(body | head -c 200)"; fi
# The keys API exposes the relay settings (§8.2 addendum; map keys marshal
# alphabetically: drive, max_age_days, quota_bytes).
c=$(code -b /tmp/acc_djar "$BASE/admin/api/keys")
if [ "$c" = 200 ] && body | grep -q '"relay":{"drive":null,"max_age_days":0,"quota_bytes":0}'; then
  ok "H0b admin keys API carries relay settings"
else bad "H0b" "code=$c body=$(body | head -c 300)"; fi
c=$(code -H "apikey: $HKEY" "$BASE/v1/me")
if [ "$c" = 200 ] && body | grep -q '"relay":{"drive":"baidu-main","enabled":true' \
  && body | grep -q "\"quota_bytes\":$(python3 -c 'print(200<<30)')" && body | grep -q '"max_age_days":7'; then
  ok "H0c /v1/me relay section (enabled, 200GiB, 7d)"
else bad "H0c" "code=$c body=$(body | head -c 300)"; fi

# H1: connection check exactly as the Gokapi instructions describe.
c=$(code --fail-with-body --silent --show-error -H "apikey: $HKEY" "$BASE/api/info/version")
if [ "$c" = 200 ] && body | grep -q '"Version":"' && body | grep -q '"VersionInt":'; then
  ok "H1 GET /api/info/version with apikey header"
else bad "H1" "code=$c body=$(body | head -c 200)"; fi

# H2: upload exactly as the Gokapi instructions describe (multipart, 0/7).
head -c 1048576 /dev/urandom > $HSCRATCH/acc_h_test.bin
c=$(code --fail-with-body --silent --show-error \
  -H "apikey: $HKEY" -H "accept: application/json" \
  -F "file=@$HSCRATCH/acc_h_test.bin" -F 'allowedDownloads=0' -F 'expiryDays=7' \
  "$BASE/api/files/add")
HID=$(body | sed -n 's/.*"Id":"\([^"]*\)".*/\1/p' | head -1)
HSIZE=$(body | sed -n 's/.*"SizeBytes":\([0-9]*\).*/\1/p' | head -1)
HCRC=$(body | sed -n 's/.*"CRC64":"\([0-9]*\)".*/\1/p' | head -1)
HHOT=$(body | sed -n 's/.*"UrlHotlink":"\([^"]*\)".*/\1/p' | head -1)
WANT_CRC=$(crc64_file $HSCRATCH/acc_h_test.bin)
if [ "$c" = 200 ] && body | grep -q '"Result":"OK"' && [ "$HSIZE" = 1048576 ]; then
  ok "H2a upload returns Result OK + SizeBytes == local size"
else bad "H2a" "code=$c size=$HSIZE body=$(body | head -c 300)"; fi
if [ -n "$HCRC" ] && [ "$HCRC" = "$WANT_CRC" ]; then
  ok "H2b CRC64 == local CRC-64/XZ ($HCRC)"
else bad "H2b" "server=$HCRC local=$WANT_CRC"; fi

# H3: keyless downloads via UrlHotlink and /downloadFile?id= are byte-identical.
if [ -n "$HHOT" ]; then
  c=$(code "$HHOT")
  curl -s --fail -o $HSCRATCH/acc_h_hot.bin "$HHOT"
  if [ "$c" = 200 ] && cmp -s $HSCRATCH/acc_h_test.bin $HSCRATCH/acc_h_hot.bin; then
    ok "H3a UrlHotlink download byte-identical"
  else bad "H3a" "code=$c"; fi
else bad "H3a" "no UrlHotlink in $(head -c 200 /tmp/acc_body)"; fi
c=$(code "$BASE/downloadFile?id=$HID")
curl -s --fail -o $HSCRATCH/acc_h_dl.bin "$BASE/downloadFile?id=$HID"
if [ "$c" = 200 ] && cmp -s $HSCRATCH/acc_h_test.bin $HSCRATCH/acc_h_dl.bin; then
  ok "H3b /downloadFile?id= byte-identical (no key)"
else bad "H3b" "code=$c"; fi
c=$(code "$BASE/d?id=$HID")
if [ "$c" = 200 ] && body | grep -q "acc_h_test.bin" && body | grep -q "downloadFile?id=$HID"; then
  ok "H3c /d?id= share page (UrlDownload) renders name + button"
else bad "H3c" "code=$c body=$(body | head -c 200)"; fi

# H4: the file appears in the list and in list/{id}.
c=$(code -H "apikey: $HKEY" "$BASE/api/files/list")
if [ "$c" = 200 ] && body | grep -q "\"Id\":\"$HID\"" && body | grep -q "$HCRC"; then
  ok "H4a list contains the upload"
else bad "H4a" "code=$c body=$(body | head -c 300)"; fi
c=$(code -H "apikey: $HKEY" "$BASE/api/files/list/$HID")
if [ "$c" = 200 ] && body | grep -q "\"CRC64\":\"$HCRC\"" && body | grep -q '"UnlimitedDownloads":true'; then
  ok "H4b list/{id} metadata (unlimited downloads)"
else bad "H4b" "code=$c body=$(body | head -c 300)"; fi

# H5: delete -> gone everywhere.
c=$(code --fail-with-body -X DELETE "$BASE/api/files/delete" -H "apikey: $HKEY" -H "id: $HID")
[ "$c" = 200 ] && ok "H5a delete" || bad "H5a" "code=$c body=$(body | head -c 200)"
c=$(code -H "apikey: $HKEY" "$BASE/api/files/list/$HID"); [ "$c" = 404 ] && ok "H5b metadata gone" || bad "H5b" "code=$c"
c=$(code "$BASE/downloadFile?id=$HID"); [ "$c" = 404 ] && ok "H5c download gone" || bad "H5c" "code=$c"

# H6: allowedDownloads=1 -> second download fails 404/410.
c=$(code --fail-with-body -H "apikey: $HKEY" -H "accept: application/json" \
  -F "file=@$HSCRATCH/acc_h_test.bin" -F 'allowedDownloads=1' -F 'expiryDays=7' "$BASE/api/files/add")
ONCE_ID=$(body | sed -n 's/.*"Id":"\([^"]*\)".*/\1/p' | head -1)
c=$(code "$BASE/downloadFile?id=$ONCE_ID")
[ "$c" = 200 ] && ok "H6a first download OK" || bad "H6a" "code=$c"
c=$(code "$BASE/downloadFile?id=$ONCE_ID")
case "$c" in 404|410) ok "H6b second download rejected ($c)";; *) bad "H6b" "code=$c";; esac
curl -s -o /dev/null -X DELETE -H "apikey: $HKEY" -H "id: $ONCE_ID" "$BASE/api/files/delete"

# H7: chunked upload of a ~12 MiB file in 5 MiB chunks, blocking completion.
head -c 12582912 /dev/urandom > $HSCRATCH/acc_h_big.bin
CHUNK_UUID="acc-h-$(date +%s)-chunkupload"
HUPLOAD=0
for off in 0 5242880 10485760; do
  dd if=$HSCRATCH/acc_h_big.bin of=$HSCRATCH/acc_h_chunk.bin bs=1M skip=$((off/1048576)) count=5 status=none
  c=$(code --fail-with-body -H "apikey: $HKEY" \
    -F "file=@$HSCRATCH/acc_h_chunk.bin" -F "uuid=$CHUNK_UUID" -F "filesize=12582912" -F "offset=$off" \
    "$BASE/api/chunk/add")
  if [ "$c" != 200 ]; then HUPLOAD=1; bad "H7 chunk @$off" "code=$c body=$(body | head -c 200)"; break; fi
done
if [ "$HUPLOAD" = 0 ]; then
  c=$(code --fail-with-body -X POST "$BASE/api/chunk/complete" -H "apikey: $HKEY" \
    -H "uuid: $CHUNK_UUID" -H "filename: big chunk upload.bin" -H "filesize: 12582912" \
    -H "contenttype: application/octet-stream" -H "allowedDownloads: 0" -H "expiryDays: 7" \
    -H "nonblocking: false")
  BIG_CRC=$(body | sed -n 's/.*"CRC64":"\([0-9]*\)".*/\1/p' | head -1)
  BIG_ID=$(body | sed -n 's/.*"Id":"\([^"]*\)".*/\1/p' | head -1)
  WANT_BIG=$(crc64_file $HSCRATCH/acc_h_big.bin)
  if [ "$c" = 200 ] && [ "$BIG_CRC" = "$WANT_BIG" ]; then
    ok "H7a chunked upload + blocking complete, CRC64 matches ($BIG_CRC)"
  else bad "H7a" "code=$c crc=$BIG_CRC want=$WANT_BIG body=$(body | head -c 200)"; fi
  c=$(code "$BASE/downloadFile?id=$BIG_ID")
  curl -s --fail -o $HSCRATCH/acc_h_big_dl.bin "$BASE/downloadFile?id=$BIG_ID"
  if [ "$c" = 200 ] && cmp -s $HSCRATCH/acc_h_big.bin $HSCRATCH/acc_h_big_dl.bin; then
    ok "H7b chunked file downloads byte-identical"
  else bad "H7b" "code=$c"; fi
  curl -s -o /dev/null -X DELETE -H "apikey: $HKEY" -H "id: $BIG_ID" "$BASE/api/files/delete"
fi

# H8: unsupported Gokapi admin endpoints answer 403 JSON.
c=$(code -H "apikey: $HKEY" "$BASE/api/logs/get")
[ "$c" = 403 ] && body | grep -q "not supported" && ok "H8a /api/logs/get -> 403" || bad "H8a" "code=$c"
c=$(code -X POST -H "apikey: $HKEY" "$BASE/api/user/create")
[ "$c" = 403 ] && ok "H8b /api/user/create -> 403" || bad "H8b" "code=$c"

# H9: quota eviction with a tiny-quota key (oldest completed files evicted).
HQREQ='{"id":"acc-h-quota","default_drive":"baidu-main","drives":[{"drive":"baidu-main","root":"/acc-hq","perms":["list","read","write","delete"]}],"global_perms":["relay"],"relay":{"quota_bytes":300},"trash":{"enabled":false}}'
c=$(code -b /tmp/acc_djar -X POST "$BASE/admin/api/keys" -H 'Content-Type: application/json' -d "$HQREQ")
HQKEY=$(body | sed -n 's/.*"plaintext":"\([^"]*\)".*/\1/p')
if [ "$c" = 200 ] && [ -n "$HQKEY" ]; then ok "H9a tiny-quota key created"; else bad "H9a" "code=$c body=$(body | head -c 200)"; fi
if [ -n "$HQKEY" ]; then
  for i in 1 2 3; do head -c 120 /dev/urandom > $HSCRATCH/acc_hq.bin
    c=$(code -H "apikey: $HQKEY" -F "file=@$HSCRATCH/acc_hq.bin" -F 'allowedDownloads=0' -F 'expiryDays=7' "$BASE/api/files/add")
    [ "$c" = 200 ] || bad "H9 upload $i" "code=$c body=$(body | head -c 200)"
  done
  c=$(code -H "apikey: $HQKEY" "$BASE/api/files/list")
  NFILES=$(body | python3 -c 'import json,sys; d=json.load(sys.stdin); print(len(d) if d else 0)')
  [ "$NFILES" = 2 ] && ok "H9b quota eviction left the 2 newest files" || bad "H9b" "files=$NFILES body=$(body | head -c 300)"
  head -c 500 /dev/urandom > $HSCRATCH/acc_hq_big.bin
  c=$(code -H "apikey: $HQKEY" -F "file=@$HSCRATCH/acc_hq_big.bin" -F 'allowedDownloads=0' -F 'expiryDays=7' "$BASE/api/files/add")
  c2=$(code -H "apikey: $HQKEY" "$BASE/api/files/list")
  N2=$(body | python3 -c 'import json,sys; d=json.load(sys.stdin); print(len(d) if d else 0)')
  if [ "$c" = 400 ] && [ "$N2" = 2 ]; then
    ok "H9c oversized upload rejected (400) without evicting"
  else bad "H9c" "code=$c files=$N2"; fi
fi

# H10: expired file purged permanently (retention test hook: backdate the row
# via sqlite on the host-mounted DB, then trigger POST /admin/api/maintenance,
# the manual form of the 5-minute relay sweep; documented in §4.7 notes).
DBFILE="${C2A_DB_FILE:-$(dirname "$0")/../cloud2api-data/cloud2api.db}"
c=$(code -H "apikey: $HKEY" -F "file=@$HSCRATCH/acc_h_test.bin" -F 'allowedDownloads=0' -F 'expiryDays=1' "$BASE/api/files/add")
EXP_ID=$(body | sed -n 's/.*"Id":"\([^"]*\)".*/\1/p' | head -1)
if [ -n "$EXP_ID" ] && [ -f "$DBFILE" ]; then
  python3 - "$DBFILE" "$EXP_ID" <<'PYEOF'
import sqlite3, sys
conn = sqlite3.connect(sys.argv[1], timeout=30)
conn.execute("UPDATE relay_files SET expire_at='2020-01-01T00:00:00.000000000Z' WHERE id=?", (sys.argv[2],))
conn.commit(); conn.close()
print("backdated")
PYEOF
  c=$(code -b /tmp/acc_djar -X POST "$BASE/admin/api/maintenance")
  if [ "$c" = 200 ] && body | grep -q '"relay_files_purged":[1-9]'; then
    ok "H10a maintenance run purged the expired relay file"
  else bad "H10a" "code=$c body=$(body | head -c 300)"; fi
  c=$(code "$BASE/downloadFile?id=$EXP_ID")
  [ "$c" = 404 ] && ok "H10b expired download gone" || bad "H10b" "code=$c"
else
  bad "H10" "upload code=$c id=$EXP_ID db=$DBFILE"
fi

# H11: the gokapi instruction assets render (no key inside).
c=$(code "$BASE/custom/gokapi-agent-instructions.txt")
if [ "$c" = 200 ] && body | grep -q "swap the Base URL and API key" && body | grep -q "11051210869376104954" && ! body | grep -q "$HKEY"; then
  ok "H11a gokapi-agent-instructions.txt rendered keyless"
else bad "H11a" "code=$c"; fi
c=$(code "$BASE/custom/agent-instructions.txt")
if [ "$c" = 200 ] && body | grep -q "Gokapi-compatible file relay"; then
  ok "H11b agent-instructions.txt mentions the relay"
else bad "H11b" "code=$c"; fi
c=$(code "$BASE/apidocumentation/openapi.json")
if [ "$c" = 200 ] && body | grep -q '"/api/files/add"' && body | grep -q '"gokapi-compat"'; then
  ok "H11c openapi.json documents gokapi-compat"
else bad "H11c" "code=$c"; fi

# H12: cleanup — remove the keys' relay files, then hard-delete the test keys
# (second DELETE hard-deletes a revoked key; idempotent re-runs stay clean).
for K in "$HKEY" "$HQKEY"; do
  [ -z "$K" ] && continue
  curl -s -H "apikey: $K" "$BASE/api/files/list" | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
    for f in d or []:
        print(f["Id"])
except Exception:
    pass' | while read -r fid; do
    curl -s -o /dev/null -X DELETE -H "apikey: $K" -H "id: $fid" "$BASE/api/files/delete"
  done
done
curl -s -o /dev/null -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"dir":"/","names":["acc-h","acc-hq"]}' "$BASE/api/storage/baidu-main/remove"
for KID in acc-h acc-h-quota; do
  curl -s -o /dev/null -b /tmp/acc_djar -X DELETE "$BASE/admin/api/keys/$KID"
  c=$(code -b /tmp/acc_djar -X DELETE "$BASE/admin/api/keys/$KID")
  [ "$c" = 200 ] && ok "H12 $KID revoked + hard-deleted" || bad "H12" "$KID code=$c"
done
rm -rf "$HSCRATCH"


# ============================================================================
# Group I — re-login happens in OpenList (IMPLEMENTATION-openlist-entry.md):
# the administrator fixes a storage directly in OpenList, then refreshes the
# portal. A Local storage with a missing root stands in for broken credentials;
# no permission-request or approval API is involved.
# ============================================================================
echo "== P9 reauth through OpenList =="
if [ -n "${OL_TOKEN:-}" ]; then
  I4ROOT="$TESTDATA_DIR/portal/acc-i4"
  I4ROOT2="$TESTDATA_DIR/portal/acc-i4-v2"
  # I0: leftovers from previous runs (errors ignored).
  curl -s -o /dev/null -b /tmp/acc_djar -X DELETE "$BASE/admin/api/keys/acc-i4-key"
  curl -s -o /dev/null -b /tmp/acc_djar -X DELETE "$BASE/admin/api/keys/acc-i4-key"
  curl -s -o /dev/null -b /tmp/acc_djar -X DELETE "$BASE/admin/api/drives/acc-i4"
  ol_rm_mount /acc-i4
  mkdir -p "$I4ROOT" "$I4ROOT2" && echo "v2 file" > "$I4ROOT2/v2.txt"

  # I1: storage created in OpenList and adopted; a key granted on it.
  I4SID=$(ol_mk_local /acc-i4 /mnt/portal-test/acc-i4)
  c=$(code -b /tmp/acc_djar -X POST "$BASE/admin/api/drives/adopt" -H 'Content-Type: application/json' -d "{\"storage_id\":${I4SID:-0},\"id\":\"acc-i4\"}")
  [ "$c" = 200 ] && ok "I1a acc-i4 adopted" || bad "I1a" "sid=$I4SID code=$c body=$(body | head -c 200)"
  c=$(code -b /tmp/acc_djar -X POST "$BASE/admin/api/keys" -H 'Content-Type: application/json' \
    -d '{"id":"acc-i4-key","drives":[{"drive":"acc-i4","root":"/","perms":["list","read","write"]}],"global_perms":[]}')
  I4KEY=$(body | sed -n 's/.*"plaintext":"\([^"]*\)".*/\1/p')
  [ "$c" = 200 ] && [ -n "$I4KEY" ] && ok "I1b test key acc-i4-key" || bad "I1b" "code=$c body=$(body | head -c 200)"

  # I2: the storage breaks in OpenList; requests to it must not succeed.
  ol_set_root "$I4SID" /mnt/portal-test/acc-i4-missing
  I2STATUS=$(ol_storage_json "$I4SID" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["status"])')
  [ -n "$I2STATUS" ] && [ "$I2STATUS" != work ] && ok "I2a OpenList reports the storage broken: $(echo "$I2STATUS" | head -c 60)" || bad "I2a" "status=$I2STATUS"
  c=$(code -H "apikey: $I4KEY" "$BASE/v1/drives/acc-i4/list?path=/")
  [ "$c" != 200 ] && ok "I2b broken storage does not serve files" || bad "I2b" "unexpected success"

  # I3: refreshing does not repair the backing storage or change permissions.
  c=$(code -b /tmp/acc_djar -X POST "$BASE/admin/api/drives/sync" -H 'Content-Type: application/json' -d '{}')
  [ "$c" = 200 ] && ok "I3a sync while broken" || bad "I3a" "code=$c"
  c=$(code -H "apikey: $I4KEY" "$BASE/v1/drives/acc-i4/list?path=/")
  [ "$c" != 200 ] && ok "I3b sync alone does not repair the storage" || bad "I3b" "unexpected success"

  # I4: repaired in OpenList, then refreshed; mapping and grant stay intact.
  ol_set_root "$I4SID" /mnt/portal-test/acc-i4-v2
  c=$(code -b /tmp/acc_djar -X POST "$BASE/admin/api/drives/sync" -H 'Content-Type: application/json' -d '{}')
  [ "$c" = 200 ] && ok "I4a sync after direct OpenList repair" || bad "I4a" "code=$c"
  I4STATUS=$(ol_storage_json "$I4SID" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["status"])')
  [ "$I4STATUS" = work ] && ok "I4b OpenList repair is initialized" || bad "I4b" "status=$I4STATUS"
  c=$(code -b /tmp/acc_djar "$BASE/admin/api/drives")
  if [ "$c" = 200 ] && body | python3 -c "
import json, sys
d = next(r for r in json.load(sys.stdin)['data']['drives'] if r['id'] == 'acc-i4')
sys.exit(0 if d['storage_id'] == ${I4SID:-0} and d['mount_path'] == '/acc-i4' and d['state'] == 'healthy' else 1)"; then
    ok "I4c drive id, storage id and mount unchanged"
  else bad "I4c" "code=$c body=$(body | head -c 300)"; fi
  c=$(code -H "apikey: $I4KEY" "$BASE/v1/drives/acc-i4/list?path=/")
  [ "$c" = 200 ] && body | grep -q v2.txt && ok "I4d key grant serves the fixed storage" || bad "I4d" "code=$c body=$(body | head -c 200)"

  # I5: the static simulated netdisks report their real driver (Local).
  DRIVERS=$(curl -s -b /tmp/acc_djar "$BASE/admin/api/drives" | python3 -c '
import json, sys
d = {x["id"]: x["driver"] for x in json.load(sys.stdin)["data"]["drives"]}
print(d.get("baidu-main","?"), d.get("pan123-main","?"), d.get("aliyun-main","?"))')
  [ "$DRIVERS" = "Local Local Local" ] && ok "I5 static drives report their real driver (Local)" || bad "I5" "drivers=$DRIVERS"

  # I6: repairing credentials never creates or processes permission approvals.
  c=$(code -b /tmp/acc_djar "$BASE/admin/api/overview")
  if [ "$c" = 200 ] && ! body | grep -q pending_requests; then
    ok "I6 overview has no approval queue"
  else bad "I6" "code=$c"; fi

  # I7: cleanup — key, mapping, and the storage (deleted in OpenList).
  curl -s -o /dev/null -b /tmp/acc_djar -X DELETE "$BASE/admin/api/keys/acc-i4-key"
  c=$(code -b /tmp/acc_djar -X DELETE "$BASE/admin/api/keys/acc-i4-key")
  [ "$c" = 200 ] && ok "I7a acc-i4-key revoked + hard-deleted" || bad "I7a" "code=$c"
  c=$(code -b /tmp/acc_djar -X DELETE "$BASE/admin/api/drives/acc-i4")
  [ "$c" = 200 ] && ok "I7b acc-i4 移出" || bad "I7b" "code=$c"
  ol_rm_mount /acc-i4
  [ -z "$(ol_sid_by_mount /acc-i4)" ] && ok "I7c storage deleted in OpenList" || bad "I7c" "storage left"
  rm -rf "$I4ROOT" "$I4ROOT2"
else
  bad "I0" "OpenList admin login failed; group I skipped"
fi


# ============================================================================
# Group R — replacement storages (IMPLEMENTATION-openlist-entry.md step 1):
# when a drive's OpenList storage is deleted and a new one is created at the
# same mount, every access path stays blocked until an administrator confirms
# the replacement — including right after a successful request (warm caches).
# ============================================================================
echo "== P10 replacement storage confirmation =="
if [ -n "${OL_TOKEN:-}" ]; then
  # R0: leftovers from previous runs (errors ignored).
  curl -s -o /dev/null -b /tmp/acc_djar -X DELETE "$BASE/admin/api/keys/acc-r"
  curl -s -o /dev/null -b /tmp/acc_djar -X DELETE "$BASE/admin/api/keys/acc-r"
  for d in acc-r1 acc-r2; do curl -s -o /dev/null -b /tmp/acc_djar -X DELETE "$BASE/admin/api/drives/$d"; done
  for m in /acc-r1 /acc-r2; do
    sid=$(ol_sid_by_mount "$m")
    [ -n "$sid" ] && curl -s -o /dev/null -X POST -H "Authorization: $OL_TOKEN" "$OL/api/admin/storage/delete?id=$sid"
  done

  # R1: two storages created in OpenList, adopted by cloud2api.
  rcreate() { # rcreate <mount> <root>
    olpost /api/admin/storage/create "{\"mount_path\":\"$1\",\"driver\":\"Local\",\"addition\":\"{\\\"root_folder_path\\\":\\\"$2\\\"}\"}" \
      | sed -n 's/.*"id":\([0-9]*\).*/\1/p'
  }
  R1SID=$(rcreate /acc-r1 /mnt/portal-test)
  R2SID=$(rcreate /acc-r2 /mnt/portal-test)
  c1=$(code -b /tmp/acc_djar -X POST "$BASE/admin/api/drives/adopt" -H 'Content-Type: application/json' -d "{\"storage_id\":$R1SID,\"id\":\"acc-r1\"}")
  c2=$(code -b /tmp/acc_djar -X POST "$BASE/admin/api/drives/adopt" -H 'Content-Type: application/json' -d "{\"storage_id\":$R2SID,\"id\":\"acc-r2\"}")
  if [ -n "$R1SID" ] && [ -n "$R2SID" ] && [ "$c1" = 200 ] && [ "$c2" = 200 ]; then ok "R1 storages created in OpenList and adopted"; else bad "R1" "sids=$R1SID/$R2SID codes=$c1/$c2 body=$(body | head -c 150)"; fi
  RREQ='{"id":"acc-r","default_drive":"acc-r1","drives":[{"drive":"acc-r1","root":"/","perms":["list","read","write","transfer"]},{"drive":"acc-r2","root":"/","perms":["list","read","write","transfer"]}],"global_perms":["list","read","write","transfer"],"trash":{"enabled":false}}'
  c=$(code -b /tmp/acc_djar -X POST "$BASE/admin/api/keys" -H 'Content-Type: application/json' -d "$RREQ")
  RKEY=$(body | sed -n 's/.*"plaintext":"\([^"]*\)".*/\1/p')
  [ "$c" = 200 ] && [ -n "$RKEY" ] && ok "R1b test key acc-r" || bad "R1b" "code=$c body=$(body | head -c 150)"

  # R2: warm every cache with successful requests on both paths.
  c=$(code -H "apikey: $RKEY" "$BASE/v1/drives/acc-r1/list?path=/")
  c2=$(code -X POST "$BASE/api/storage/acc-r1/list" -H "Authorization: Bearer $RKEY" -H 'Content-Type: application/json' -d '{"path":"/"}')
  curl -s -o /dev/null -H "apikey: $RKEY" "$BASE/v1/me"
  [ "$c" = 200 ] && [ "$c2" = 200 ] && ok "R2 acc-r1 reachable before the swap" || bad "R2" "v1=$c legacy=$c2"

  # R3: delete the storage in OpenList and create a different one at the same
  # mount; access immediately, without any refresh in between.
  curl -s -o /dev/null -X POST -H "Authorization: $OL_TOKEN" "$OL/api/admin/storage/delete?id=$R1SID"
  RNEW=$(rcreate /acc-r1 /mnt)
  [ -n "$RNEW" ] && [ "$RNEW" != "$R1SID" ] && ok "R3 replacement storage $RNEW created at /acc-r1" || bad "R3" "new=$RNEW"
  c=$(code -H "apikey: $RKEY" "$BASE/v1/drives/acc-r1/list?path=/")
  [ "$c" = 503 ] && body | grep -q drive_unavailable && ok "R3a /v1 list blocked" || bad "R3a" "code=$c body=$(body | head -c 150)"
  c=$(code -X POST "$BASE/api/storage/acc-r1/list" -H "Authorization: Bearer $RKEY" -H 'Content-Type: application/json' -d '{"path":"/"}')
  [ "$c" = 503 ] && ok "R3b legacy /api/storage list blocked" || bad "R3b" "code=$c body=$(body | head -c 150)"
  c=$(code "$BASE/api/storage/acc-r1/raw/hello.txt" -H "Authorization: Bearer $RKEY")
  [ "$c" = 503 ] && ok "R3c legacy raw download blocked" || bad "R3c" "code=$c"
  c=$(code -X POST "$BASE/api/transfer" -H "Authorization: Bearer $RKEY" -H 'Content-Type: application/json' -d '{"src_account":"acc-r1","dst_account":"acc-r2","src_dir":"/","dst_dir":"/","names":["x"]}')
  [ "$c" = 503 ] && ok "R3d legacy transfer with replaced source blocked" || bad "R3d" "code=$c body=$(body | head -c 150)"
  c=$(code -X POST "$BASE/api/transfer" -H "Authorization: Bearer $RKEY" -H 'Content-Type: application/json' -d '{"src_account":"acc-r2","dst_account":"acc-r1","src_dir":"/","dst_dir":"/","names":["x"]}')
  [ "$c" = 503 ] && ok "R3e legacy transfer with replaced destination blocked" || bad "R3e" "code=$c body=$(body | head -c 150)"
  c=$(code -X POST -H "apikey: $RKEY" -H 'Content-Type: application/json' -d '{"from":{"drive":"acc-r2","path":"/x"},"to":{"drive":"acc-r1","path":"/x"}}' "$BASE/v1/transfers")
  [ "$c" = 503 ] && ok "R3f /v1 transfer into replaced drive blocked" || bad "R3f" "code=$c body=$(body | head -c 150)"
  c=$(code -X POST -H "apikey: $RKEY" -H 'Content-Type: application/json' -d '{"from":{"drive":"acc-r1","path":"/x"},"to":{"drive":"acc-r2","path":"/x"}}' "$BASE/v1/transfers")
  [ "$c" = 503 ] && ok "R3g /v1 transfer out of replaced drive blocked" || bad "R3g" "code=$c body=$(body | head -c 150)"

  # R4: the portal shows the candidate; it is not offered for adoption.
  c=$(code -b /tmp/acc_djar "$BASE/admin/api/drives")
  if [ "$c" = 200 ] && body | python3 -c "
import json, sys
d = [x for x in json.load(sys.stdin)['data']['drives'] if x['id'] == 'acc-r1'][0]
sys.exit(0 if d['state'] == 'replacement_found' and d['replacement']['storage_id'] == $RNEW and d['storage_id'] == $R1SID else 1)"; then
    ok "R4a drive shows replacement_found with the candidate"
  else bad "R4a" "code=$c body=$(body | head -c 300)"; fi
  c=$(code -b /tmp/acc_djar "$BASE/admin/api/drives/unmanaged")
  if [ "$c" = 200 ] && body | python3 -c "
import json, sys
s = [x for x in json.load(sys.stdin)['data']['storages'] if x['storage_id'] == $RNEW]
sys.exit(0 if s and s[0].get('replacement_for') == 'acc-r1' else 1)"; then
    ok "R4b candidate listed as replacement_for acc-r1"
  else bad "R4b" "code=$c body=$(body | head -c 300)"; fi
  c=$(code -b /tmp/acc_djar -X POST "$BASE/admin/api/drives/adopt" -H 'Content-Type: application/json' -d "{\"storage_id\":$RNEW,\"id\":\"acc-r1b\"}")
  [ "$c" = 409 ] && ok "R4c candidate cannot be adopted as another drive" || bad "R4c" "code=$c"
  c=$(code -H "apikey: $RKEY" "$BASE/v1/drives/acc-r1/list?path=/")
  [ "$c" = 503 ] && ok "R4d still blocked after the portal saw it" || bad "R4d" "code=$c"

  # R5: the administrator confirms; access resumes on the new storage.
  c=$(code -b /tmp/acc_djar -X POST "$BASE/admin/api/drives/acc-r1/rebind" -H 'Content-Type: application/json' -d "{\"storage_id\":$RNEW}")
  [ "$c" = 200 ] && body | grep -q "\"storage_id\":$RNEW" && ok "R5a rebind confirmed" || bad "R5a" "code=$c body=$(body | head -c 200)"
  c=$(code -H "apikey: $RKEY" "$BASE/v1/drives/acc-r1/list?path=/")
  [ "$c" = 200 ] && ok "R5b /v1 access resumes" || bad "R5b" "code=$c body=$(body | head -c 150)"
  c=$(code -X POST "$BASE/api/storage/acc-r1/list" -H "Authorization: Bearer $RKEY" -H 'Content-Type: application/json' -d '{"path":"/"}')
  [ "$c" = 200 ] && ok "R5c legacy access resumes" || bad "R5c" "code=$c"

  # R6: cleanup — key, drives and storages.
  curl -s -o /dev/null -b /tmp/acc_djar -X DELETE "$BASE/admin/api/keys/acc-r"
  curl -s -o /dev/null -b /tmp/acc_djar -X DELETE "$BASE/admin/api/keys/acc-r"
  for d in acc-r1 acc-r2; do curl -s -o /dev/null -b /tmp/acc_djar -X DELETE "$BASE/admin/api/drives/$d"; done
  ol_rm_mount /acc-r1 /acc-r2
  RLEFT=0
  for m in /acc-r1 /acc-r2; do [ -n "$(ol_sid_by_mount "$m")" ] && RLEFT=$((RLEFT+1)); done
  [ "$RLEFT" = 0 ] && ok "R6 cleanup: mappings removed, storages deleted in OpenList" || bad "R6" "left=$RLEFT"
else
  bad "R0" "OpenList admin login failed; group R skipped"
fi

# Group F — the static LAN key must survive every test run untouched (both the
# legacy data plane and /v1; guards the 2026-10-12 revocation incident).
# ============================================================================
echo "== P6 LAN key invariant =="
c=$(code -X POST "$BASE/api/storage/baidu-main/list" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{"path":"/"}')
[ "$c" = 200 ] && ok "F1 LAN key legacy list" || bad "F1" "code=$c body=$(body | head -c 150)"
c=$(code -H "apikey: $KEY" "$BASE/v1/me")
[ "$c" = 200 ] && ok "F2 LAN key /v1/me" || bad "F2" "code=$c body=$(body | head -c 150)"

echo
echo "RESULT: $PASS passed, $FAIL failed"
[ "$FAIL" = 0 ]
