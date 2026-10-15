#!/usr/bin/env python3
"""Mutation check of the connect module's safety properties.

Each entry breaks one property on purpose (cleanup only removes this
attempt's storage, re-authorization refuses concurrent edits and reports the
restore honestly, sessions are bound to their token, secrets never reach a
response, the callback page echoes nothing, ...). Every mutant must make a
test fail; a surviving mutant means a property is no longer tested.

    openlist-patch/scripts/mutation-check.py [mutant-id ...]

Environment: OPENLIST_MIRROR (see prepare-tree.sh).
"""
import os
import shutil
import subprocess
import sys
import tempfile

here = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
work = os.path.join(os.environ.get("XDG_CACHE_HOME", os.path.expanduser("~/.cache")), "openlist-connect", "work")
os.makedirs(work, exist_ok=True)
tmp = tempfile.mkdtemp(prefix="mutate.", dir=work)
tree = os.path.join(tmp, "openlist")
subprocess.run([os.path.join(here, "scripts", "prepare-tree.sh"), tree], check=True,
               env=dict(os.environ, SKIP_FRONTEND="1"), stdout=subprocess.DEVNULL)
only = sys.argv[1:]


M = [
    # id, file, old, new, package
    ("create-cleanup-identity", "internal/loginbridge/create.go",
     "if row.MountPath == mount && row.Driver == m.Driver {", "if row != nil {", "./internal/loginbridge/"),
    ("create-id0-no-cleanup", "internal/loginbridge/create.go",
     "\tif id == 0 {\n", "\tif false {\n", "./internal/loginbridge/"),
    ("create-trust-delete-error", "internal/loginbridge/create.go",
     "if _, aerr := db.GetStorageById(id); aerr != nil {", "if delErr == nil {", "./internal/loginbridge/"),
    ("reauth-skip-modified-check", "internal/loginbridge/reauth.go",
     "if orig.Modified.UnixMilli() != target.Modified.UnixMilli() {", "if false {", "./internal/loginbridge/"),
    ("reauth-restore-over-external-edit", "internal/loginbridge/reauth.go",
     "if cur.Modified.Before(before.Add(-timeWindowSlack)) || cur.Modified.After(after.Add(timeWindowSlack)) {", "if false {", "./internal/loginbridge/"),
    ("reauth-claim-restored", "internal/loginbridge/reauth.go",
     "report.Restored = additionRestored(finalAddition, origAddition, credKeys, auth.Fields) && storageConfigEqual(final, orig)",
     "report.Restored = len(finalAddition) >= 0", "./internal/loginbridge/"),
    ("reauth-merge-create-fields", "internal/loginbridge/reauth.go",
     "\tfor k, v := range auth.Fields {\n\t\tnewAddition[k] = v\n\t}",
     "\tfor k, v := range auth.Fields {\n\t\tnewAddition[k] = v\n\t}\n\tfor k, v := range auth.CreateFields {\n\t\tnewAddition[k] = v\n\t}", "./internal/loginbridge/"),
    ("reauth-no-old-secret-scrub", "internal/loginbridge/reauth.go",
     "secrets = append(secrets, s)", "_ = s", "./internal/loginbridge/"),
    ("saveapp-inherit-any-secret", "internal/loginbridge/apps.go",
     "if app.ClientSecret == \"\" && found && prev.Mode == app.Mode && prev.ClientID == app.ClientID {",
     "if app.ClientSecret == \"\" && found {", "./internal/loginbridge/"),
    ("restored-ignores-original", "internal/loginbridge/reauth.go",
     "\t\tif ov, had := orig[k]; had && normalizeValue(ov) == normalizeValue(cv) {\n\t\t\tcontinue\n\t\t}\n", "", "./internal/loginbridge/"),
    ("carries-ignores-original", "internal/loginbridge/reauth.go",
     "\t\t\tif ov, ok := origAddition[k]; ok && normalizeValue(ov) == normalizeValue(v) {\n\t\t\t\tcontinue\n\t\t\t}\n", "", "./internal/loginbridge/"),
    ("manager-ignore-token-fp", "pkg/loginsdk/manager.go",
     "if !ok || s.owner.UserID != owner.UserID || s.owner.TokenFP != owner.TokenFP {",
     "if !ok || s.owner.UserID != owner.UserID {", "./pkg/loginsdk/"),
    ("manager-save-twice", "pkg/loginsdk/manager.go",
     "\tcase PhaseSaved, PhaseSaveFailed:\n\t\tv := s.viewLocked(m.cfg)",
     "\tcase \"never\":\n\t\tv := s.viewLocked(m.cfg)", "./pkg/loginsdk/ ./server/connect/"),
    ("manager-state-reusable", "pkg/loginsdk/manager.go",
     "\ts.stateUsed = true\n\ts.busy = busyExchanging", "\ts.busy = busyExchanging", "./pkg/loginsdk/ ./server/connect/"),
    ("manager-no-owner-limit", "pkg/loginsdk/manager.go",
     "over := m.liveCountLocked(in.Owner.UserID) >= m.cfg.MaxPerOwner", "over := false", "./pkg/loginsdk/"),
    ("manager-keep-auth-after-save", "pkg/loginsdk/manager.go",
     "\t\t\ts.phase = PhaseSaveFailed\n\t\t}\n\t\ts.wipeAuthLocked()", "\t\t\ts.phase = PhaseSaveFailed\n\t\t}", "./pkg/loginsdk/"),
    ("manager-pending-pkce-off", "pkg/loginsdk/manager.go",
     "\t\tif meta.PKCE {\n\t\t\ts.pkce = NewPKCE()\n\t\t}", "", "./pkg/loginsdk/"),
    ("sdk-validateauth-any-field", "pkg/loginsdk/registry.go",
     "if !allowed[k] {", "if false {", "./pkg/loginsdk/"),
    ("sdk-scrub-noop", "pkg/loginsdk/util.go",
     "func Scrub(msg string, secrets ...string) string {", "func Scrub(msg string, secrets ...string) string {\n\treturn msg\n}\n\nfunc scrubUnused(msg string, secrets ...string) string {", "./pkg/loginsdk/ ./internal/loginbridge/ ./server/connect/"),
    ("api-leak-client-secret", "server/connect/api.go",
     "p.ClientID = app.ClientID", "p.ClientID = app.ClientID + app.ClientSecret", "./server/connect/"),
    ("api-unscrubbed-target-status", "server/connect/api.go",
     "\"status\":     loginsdk.Scrub(st.Status, secrets...),", "\"status\":     st.Status,", "./server/connect/"),
    ("api-owner-fp-constant", "server/connect/api.go",
     "sum := sha256.Sum256([]byte(c.GetHeader(\"Authorization\")))", "sum := sha256.Sum256(nil)", "./server/connect/"),
    ("api-reauth-skip-checktarget", "server/connect/api.go",
     "\t\tif err := loginbridge.CheckTarget(st, a); err != nil {\n\t\t\tsdkErr(c, err)\n\t\t\treturn\n\t\t}\n\t\tt := loginbridge.TargetOf(st)",
     "\t\tt := loginbridge.TargetOf(st)", "./server/connect/"),
    ("callback-echo-query", "server/connect/callback.go",
     "html.EscapeString(message),", "html.EscapeString(message+c.Request.URL.RawQuery),", "./server/connect/"),
    ("callback-no-referrer-policy", "server/connect/callback.go",
     "c.Header(\"Referrer-Policy\", \"no-referrer\")", "", "./server/connect/"),
    ("page-spa-fallthrough", "server/connect/page.go",
     "\tdefault:\n\t\tc.String(http.StatusNotFound, \"404 not found\\n\")",
     "\tdefault:\n\t\tserveWebFile(c, \"web/index.html\", \"text/html; charset=utf-8\", \"no-store\")", "./server/connect/"),
    ("onedrive-allow-sharepoint", "pkg/loginsdk/adapters/onedrive/onedrive.go",
     "if anyBool(addition[\"is_sharepoint\"]) {", "if false {", "./pkg/loginsdk/adapters/onedrive/"),
    ("aliyun-scan-as-waiting", "pkg/loginsdk/adapters/aliyunopen/aliyunopen.go",
     "case \"ScanSuccess\":\n\t\treturn &loginsdk.QRPoll{Status: loginsdk.QRScanned}, nil",
     "case \"ScanSuccess\":\n\t\treturn &loginsdk.QRPoll{Status: loginsdk.QRWaiting}, nil", "./pkg/loginsdk/adapters/aliyunopen/"),
    ("baidu-online-renewal", "pkg/loginsdk/adapters/baidu/baidu.go",
     "\n\t\t\t\"use_online_api\": false,", "\n\t\t\t\"use_online_api\": true,", "./pkg/loginsdk/adapters/baidu/"),
    ("gdrive-no-pkce", "pkg/loginsdk/adapters/googledrive/googledrive.go",
     "form.Set(\"code_verifier\", pkce.Verifier)", "", "./pkg/loginsdk/adapters/googledrive/"),
    ("pan123-keep-old-token", "pkg/loginsdk/adapters/pan123/pan123.go",
     "\t\t\t\"AccessToken\": \"\",\n", "", "./pkg/loginsdk/adapters/pan123/"),

    # Login modes: each app mode logs in with its own method, defaults need no
    # saved profile, the public app's credentials stay server-side, pasted
    # tokens are validated and renew through the online service.
    ("manager-method-from-meta", "pkg/loginsdk/manager.go",
     "method:    meta.MethodFor(app.Mode),", "method:    meta.Method,", "./pkg/loginsdk/"),
    ("manager-no-form-guide", "pkg/loginsdk/manager.go",
     "s.guide = fg.FormGuide(app, in.Target != nil)", "_ = fg", "./pkg/loginsdk/ ./server/connect/"),
    ("manager-qr-hint-not-restored", "pkg/loginsdk/manager.go",
     "\t\tif s.qrMessage == \"\" && s.qr != nil {\n\t\t\ts.qrMessage = s.qr.Message\n\t\t}\n", "", "./pkg/loginsdk/"),
    ("manager-qr-expiry-keeps-hint", "pkg/loginsdk/manager.go",
     "s.qrStatus, s.qrMessage = QRExpired, \"\"", "s.qrStatus = QRExpired", "./pkg/loginsdk/"),
    ("sdk-default-online-no-url", "pkg/loginsdk/sdk.go",
     "Mode: AppOnlineService, ServiceURL: DefaultServiceURL}, true", "Mode: AppOnlineService}, true", "./pkg/loginsdk/ ./server/connect/"),
    ("sdk-resolve-public-noop", "pkg/loginsdk/sdk.go",
     "\tif saved.Mode != AppPublic {\n\t\treturn saved\n\t}", "\tif true {\n\t\treturn saved\n\t}", "./pkg/loginsdk/ ./server/connect/"),
    ("registry-public-app-unchecked", "pkg/loginsdk/registry.go",
     "if app.Mode != AppPublic || app.ClientID == \"\" || app.ClientSecret == \"\" {", "if _ = app; false {", "./pkg/loginsdk/"),
    ("registry-oauth-mode-unchecked", "pkg/loginsdk/registry.go",
     "if _, ok := a.(OAuthAdapter); !ok {", "if false {", "./pkg/loginsdk/"),
    ("pastetoken-allow-whitespace", "pkg/loginsdk/httpx.go",
     "if strings.ContainsAny(tok, \" \\t\\r\\n\") {", "if false {", "./pkg/loginsdk/"),
    ("pastetoken-no-length-check", "pkg/loginsdk/httpx.go",
     "if n := len(tok); n < 16 || n > 8192 {", "if n := len(tok); n < 0 {", "./pkg/loginsdk/"),
    ("api-public-payload-client-id", "server/connect/api.go",
     "\tif app.Mode != loginsdk.AppPublic {\n\t\tp.ClientID = app.ClientID", "\tif true {\n\t\tp.ClientID = app.ClientID", "./server/connect/"),
    ("api-public-save-keeps-fields", "server/connect/api.go",
     "\t\tinput = loginbridge.AppInput{Platform: req.Platform, Mode: req.Mode}\n", "", "./server/connect/"),
    ("api-no-default-app", "server/connect/api.go",
     "def, has := loginsdk.DefaultApp(a)", "def, has := loginsdk.App{}, false", "./server/connect/"),
    ("suggest-ignore-taken", "internal/loginbridge/suggest.go",
     "continue // taken", "return mount, nil // taken", "./internal/loginbridge/ ./server/connect/"),
    ("baidu-device-pending-fails", "pkg/loginsdk/adapters/baidu/baidu.go",
     "case \"authorization_pending\", \"slow_down\":", "case \"slow_down\":", "./pkg/loginsdk/adapters/baidu/"),
    ("baidu-device-denied-waits", "pkg/loginsdk/adapters/baidu/baidu.go",
     "case \"access_denied\":\n\t\treturn &loginsdk.QRPoll{Status: loginsdk.QRCancelled}, nil",
     "case \"access_denied\":\n\t\treturn &loginsdk.QRPoll{Status: loginsdk.QRWaiting}, nil", "./pkg/loginsdk/adapters/baidu/"),
    ("baidu-public-online-renewal", "pkg/loginsdk/adapters/baidu/baidu.go",
     "\n\t\t\t\t\"use_online_api\": false,", "\n\t\t\t\t\"use_online_api\": true,", "./pkg/loginsdk/adapters/baidu/"),
    ("baidu-paste-direct-renewal", "pkg/loginsdk/adapters/baidu/baidu.go",
     "\"use_online_api\":  true,", "\"use_online_api\":  false,", "./pkg/loginsdk/adapters/baidu/"),
    ("baidu-guide-wrong-driver", "pkg/loginsdk/adapters/baidu/baidu.go",
     "\"driver_txt\": \"baiduyun_go\"", "\"driver_txt\": \"baiduyun_ex\"", "./pkg/loginsdk/adapters/baidu/"),
    ("onedrive-paste-wrong-renewapi", "pkg/loginsdk/adapters/onedrive/onedrive.go",
     "\"api_url_address\": svc + \"/onedrive/renewapi\",", "\"api_url_address\": svc,", "./pkg/loginsdk/adapters/onedrive/"),
    ("onedrive-paste-no-region", "pkg/loginsdk/adapters/onedrive/onedrive.go",
     "\t\tCreateFields: map[string]any{\"region\": \"global\", \"is_sharepoint\": false},\n\t\tSecrets:      []string{rt},",
     "\t\tSecrets: []string{rt},", "./pkg/loginsdk/adapters/onedrive/"),
    ("gdrive-guide-hash", "pkg/loginsdk/adapters/googledrive/googledrive.go",
     "URL:   serviceURL(app) + \"/\",", "URL:   serviceURL(app) + \"/#e30=\",", "./pkg/loginsdk/adapters/googledrive/"),
]


def run(pkgs):
    cmd = [
        "podman", "run", "--rm", "-v", f"{tree}:/src:Z",
        "-v", "openlist-gomod:/go/pkg/mod", "-v", "openlist-gocache:/root/.cache/go-build",
        "-w", "/src", "-e", "GOTOOLCHAIN=local", "-e", "GOFLAGS=-buildvcs=false",
        "localhost/openlist-connect-gotest:latest", "sh", "-c",
        f"go test -count=1 -tags 'jsoniter loginsdk_example' {pkgs} >/tmp/out 2>&1; rc=$?; tail -5 /tmp/out; exit $rc",
    ]
    p = subprocess.run(cmd, capture_output=True, text=True, cwd=os.path.expanduser("~"))
    return p.returncode, p.stdout + p.stderr


rc, out = run("./pkg/loginsdk/... ./internal/loginbridge/... ./server/connect/...")
if rc != 0:
    print("baseline failed:\n" + out)
    sys.exit(2)
print("baseline ok")

survived = []
for mid, rel, old, new, pkgs in M:
    if only and mid not in only:
        continue
    path = os.path.join(tree, rel)
    src = open(path, encoding="utf-8").read()
    n = src.count(old)
    if n != 1:
        print(f"SKIP  {mid}: pattern found {n} times")
        survived.append(mid + " (pattern)")
        continue
    open(path, "w", encoding="utf-8").write(src.replace(old, new))
    try:
        rc, out = run(pkgs)
    finally:
        open(path, "w", encoding="utf-8").write(src)
    if rc == 0:
        print(f"SURVIVED {mid}")
        survived.append(mid)
    elif "FAIL" not in out:
        print(f"ERROR    {mid}: {out.strip()[-300:]}")
        survived.append(mid + " (error)")
    else:
        last = [l for l in out.strip().splitlines() if l.strip()][-3:]
        print(f"killed   {mid}  | {' / '.join(x.strip()[:90] for x in last)}")

print()
print("survivors:", survived if survived else "none")
shutil.rmtree(tmp, ignore_errors=True)
sys.exit(1 if survived else 0)
