#!/usr/bin/env python3
"""cloud2api FULL helper CLI for project developers (Python 3, standard
library only).

Developer copy kept in the project repository (tools/c2a-full.py). It is the
historical full helper with the complete command surface, including the
advanced commands that are not part of the basic onboarding served at
/custom/c2a.py. The gateway routes they use remain available; run this tool
from a checkout when you need them.

Environment:
  C2A_URL   gateway base URL, e.g. http://198.51.100.20:8080
  C2A_KEY   API key (sk-...)

Commands:
  me                              show key info, drives, limits, policy
  ls [PATH] [--drive D]           list a directory (default /)
  tree [PATH] [--depth N]         bounded directory tree
  search Q [--path P]             substring search under a path
  stat PATH [--drive D]           file metadata incl. recorded checksums
  put LOCAL REMOTE [--drive D]    upload a file (SHA-256 verified)
  get REMOTE [LOCAL] [--drive D]  download a file (SHA-256 verified)
  rm PATH... [--dry-run]          delete (into the trash when enabled)
  mv FROM TO [--dry-run]          move or rename
  cp SRC DST [--from-drive D] [--to-drive D] [--move] [--dry-run]
                                  cross-drive copy/move as a job (waits)
  fetch URL PATH [--drive D]      server-side download of a URL into a drive
  mkdir PATH [--drive D]          create a directory (idempotent)
  link PATH [--expires-in N]      create a temporary direct link
  jobs [ls [--state S] | get ID [--wait N] | cancel ID]
                                  inspect / long-poll / cancel jobs
  selftest                        server-side roundtrip check per drive
  keys [ls | create ... | rm ID]  child-key management (delegate permission)
  activity [--limit N]            this key's recent operations
  trash ls | trash restore ID     inspect / restore the key trash
"""

import argparse
import hashlib
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

CHUNK = 1024 * 1024


class ApiError(Exception):
    def __init__(self, status, doc):
        self.status = status
        self.doc = doc
        super().__init__("HTTP %d" % status)

    def summary(self):
        err = self.doc.get("error", {}) if isinstance(self.doc, dict) else {}
        return json.dumps(
            {"status": self.status, "error": err or self.doc},
            ensure_ascii=False, indent=2)


def base_url():
    url = os.environ.get("C2A_URL", "").rstrip("/")
    if not url:
        die("C2A_URL is not set")
    return url


def api_key():
    key = os.environ.get("C2A_KEY", "")
    if not key:
        die("C2A_KEY is not set")
    return key


def die(msg, doc=None):
    if doc is not None:
        print(json.dumps(doc, ensure_ascii=False, indent=2), file=sys.stderr)
    else:
        print("c2a: " + msg, file=sys.stderr)
    sys.exit(1)


def request(method, path, data=None, headers=None, key_required=True):
    url = base_url() + path
    req = urllib.request.Request(url, data=data, method=method)
    if key_required:
        req.add_header("apikey", api_key())
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=600) as resp:
            body = resp.read()
            return resp.status, dict(resp.headers), body
    except urllib.error.HTTPError as e:
        body = e.read()
        try:
            doc = json.loads(body.decode("utf-8", "replace"))
        except Exception:
            doc = {"error": {"code": "http_error", "message": body.decode("utf-8", "replace")[:500]}}
        raise ApiError(e.code, doc)
    except urllib.error.URLError as e:
        die("cannot reach %s: %s" % (url, e.reason))


def jreq(method, path, obj=None, key_required=True):
    data = None
    headers = {}
    if obj is not None:
        data = json.dumps(obj).encode("utf-8")
        headers["Content-Type"] = "application/json"
    _, _, body = request(method, path, data=data, headers=headers, key_required=key_required)
    if not body:
        return {}
    try:
        return json.loads(body.decode("utf-8"))
    except Exception:
        die("non-JSON response", {"error": {"code": "bad_response", "message": body[:200].decode("utf-8", "replace")}})


def op_url(op, args, extra=None):
    """Build /v1/<op> or /v1/drives/<drive>/<op> with query string."""
    params = dict(extra or {})
    q = urllib.parse.urlencode(params)
    if getattr(args, "drive", None):
        path = "/v1/drives/%s/%s" % (urllib.parse.quote(args.drive, safe=""), op)
    else:
        path = "/v1/%s" % op
    return path + ("?" + q if q else "")


def logical(p):
    if not p.startswith("/"):
        p = "/" + p
    return p


class FileStream:
    """File-like body for urllib that streams from disk."""

    def __init__(self, fh):
        self.fh = fh
        self.length = os.fstat(fh.fileno()).st_size

    def read(self, n=-1):
        return self.fh.read(n)


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        while True:
            chunk = f.read(CHUNK)
            if not chunk:
                break
            h.update(chunk)
    return h.hexdigest()


def cmd_me(args):
    doc = jreq("GET", "/v1/me")
    print(json.dumps(doc, ensure_ascii=False, indent=2))


def cmd_ls(args):
    doc = jreq("GET", op_url("list", args, {"path": logical(args.path)}))
    rows = doc.get("items", [])
    for it in rows:
        kind = "d" if it.get("is_dir") else "-"
        print("%s %12d  %s  %s" % (kind, it.get("size", 0), it.get("modified", ""), it.get("path", it.get("name"))))
    print("-- %d item(s), total %s" % (len(rows), doc.get("total")))


def cmd_tree(args):
    q = urllib.parse.urlencode({"path": logical(args.path), "depth": args.depth, "max_entries": args.max_entries})
    doc = jreq("GET", "/v1/tree?" + q)
    for it in doc.get("items", []):
        indent = "  " * max(0, it.get("depth", 1) - 1)
        kind = "d" if it.get("is_dir") else "-"
        print("%s%s %12d  %s" % (indent, kind, it.get("size", 0), it.get("path")))
    if doc.get("truncated"):
        print("-- truncated (raise max_entries or lower depth)")


def cmd_search(args):
    q = urllib.parse.urlencode({"q": args.query, "path": logical(args.path)})
    doc = jreq("GET", "/v1/search?" + q)
    for it in doc.get("items", []):
        kind = "d" if it.get("is_dir") else "-"
        print("%s %12d  %s" % (kind, it.get("size", 0), it.get("path")))
    print("-- %d match(es), engine=%s%s" % (len(doc.get("items", [])), doc.get("engine"), ", truncated" if doc.get("truncated") else ""))


def cmd_stat(args):
    doc = jreq("GET", op_url("stat", args, {"path": logical(args.path)}))
    print(json.dumps(doc, ensure_ascii=False, indent=2))


def cmd_put(args):
    local, remote = args.local, logical(args.remote)
    if not os.path.isfile(local):
        die("local file not found: " + local)
    local_sha = sha256_file(local)
    size = os.path.getsize(local)
    url = op_url("files", args, {"path": remote, "on_conflict": args.on_conflict})
    fh = open(local, "rb")
    body = FileStream(fh)
    headers = {
        "Content-Type": "application/octet-stream",
        "Content-Length": str(body.length),
        "X-Expected-SHA256": local_sha,
    }
    try:
        _, _, resp = request("PUT", url, data=body, headers=headers)
        doc = json.loads(resp.decode("utf-8"))
    finally:
        fh.close()
    if doc.get("sha256") != local_sha:
        die("server sha256 mismatch after upload", doc)
    if doc.get("size") not in (size, None):
        die("server size mismatch after upload", doc)
    print(json.dumps(doc, ensure_ascii=False, indent=2))


def cmd_get(args):
    remote = logical(args.remote)
    local = args.local or os.path.basename(remote)
    url = op_url("files", args, {"path": remote})
    req = urllib.request.Request(base_url() + url, method="GET")
    req.add_header("apikey", api_key())
    h = hashlib.sha256()
    try:
        resp = urllib.request.urlopen(req, timeout=600)
    except urllib.error.HTTPError as e:
        body = e.read()
        try:
            doc = json.loads(body.decode("utf-8", "replace"))
        except Exception:
            doc = {"error": {"code": "http_error", "message": str(e)}}
        raise ApiError(e.code, doc)
    with resp, open(local, "wb") as out:
        while True:
            chunk = resp.read(CHUNK)
            if not chunk:
                break
            h.update(chunk)
            out.write(chunk)
    want = resp.headers.get("X-C2A-SHA256")
    if want and want != h.hexdigest():
        os.unlink(local)
        die("sha256 mismatch: server recorded %s, downloaded file is %s" % (want, h.hexdigest()))
    print("downloaded %s (%d bytes, sha256 %s)" % (local, os.path.getsize(local), h.hexdigest()))


def cmd_rm(args):
    body = {"paths": [logical(p) for p in args.paths]}
    if args.dry_run:
        body["dry_run"] = True
    doc = jreq("POST", op_url("delete", args), body)
    if body.get("dry_run"):
        wa = doc.get("would_affect", {})
        print("would affect %s file(s), %s dir(s), %s byte(s)" % (wa.get("files"), wa.get("dirs"), wa.get("bytes")))
        for it in doc.get("items", [])[:200]:
            print("  " + it)
        return
    for r in doc.get("results", []):
        print("%-10s %s" % (r.get("status"), r.get("path")))
        if r.get("status") == "trashed" and r.get("trash_id") is not None:
            print("           trash id: %s (restore with: c2a.py trash restore %s)" % (r["trash_id"], r["trash_id"]))


def cmd_mv(args):
    body = {"from": logical(args.frm), "to": logical(args.to), "on_conflict": args.on_conflict}
    if args.dry_run:
        body["dry_run"] = True
    doc = jreq("POST", op_url("move", args), body)
    print(json.dumps(doc, ensure_ascii=False, indent=2))


def cmd_cp(args):
    frm = {"drive": args.from_drive, "path": logical(args.frm)}
    to = {"drive": args.to_drive, "path": logical(args.to)}
    body = {"from": frm, "to": to, "mode": "move" if args.move else "copy", "on_conflict": args.on_conflict}
    if args.dry_run:
        body["dry_run"] = True
    doc = jreq("POST", "/v1/transfers", body)
    if doc.get("dry_run"):
        wa = doc.get("would_affect", {})
        print("would affect %s file(s), %s dir(s), %s byte(s)" % (wa.get("files"), wa.get("dirs"), wa.get("bytes")))
        for it in doc.get("items", [])[:200]:
            print("  " + it)
        return
    job = doc.get("job", {})
    print("job %s %s -> %s (%s)" % (job.get("id"), frm, to, job.get("state")))
    wait_job(job.get("id"), args.wait)


def wait_job(job_id, wait_seconds):
    deadline = time.time() + max(0, wait_seconds)
    while True:
        q = urllib.parse.urlencode({"wait": 60})
        doc = jreq("GET", "/v1/jobs/%s?%s" % (job_id, q))
        job = doc.get("job", {})
        state = job.get("state")
        print("  state=%s progress=%s" % (state, job.get("progress")))
        if state in ("succeeded", "failed", "canceled"):
            if job.get("error"):
                print("  error: " + job.get("error"))
            if job.get("result"):
                print("  result: " + json.dumps(job.get("result"), ensure_ascii=False))
            return job
        if time.time() >= deadline:
            print("  still %s; keep polling with: c2a.py jobs get %s --wait 60" % (state, job_id))
            return job
        time.sleep(1)


def cmd_fetch(args):
    body = {"url": args.url, "path": logical(args.path), "on_conflict": args.on_conflict}
    if args.drive:
        body["drive"] = args.drive
    doc = jreq("POST", "/v1/fetch", body)
    job = doc.get("job", {})
    print("job %s fetch %s -> %s (%s)" % (job.get("id"), args.url, args.path, job.get("state")))
    wait_job(job.get("id"), args.wait)


def cmd_mkdir(args):
    doc = jreq("POST", op_url("mkdir", args), {"path": logical(args.path)})
    print(json.dumps(doc, ensure_ascii=False, indent=2))


def cmd_link(args):
    doc = jreq("POST", "/v1/links",
               {"path": logical(args.path), "expires_in": args.expires_in})
    print(json.dumps(doc, ensure_ascii=False, indent=2))


def cmd_jobs(args):
    if args.jobs_cmd == "ls":
        q = ""
        if args.state:
            q = "?" + urllib.parse.urlencode({"state": args.state})
        doc = jreq("GET", "/v1/jobs" + q)
        for j in doc.get("jobs", []):
            print("%-16s %-10s %-9s %6s  %s" % (j.get("id"), j.get("kind"), j.get("state"), j.get("progress"), j.get("created_at")))
    elif args.jobs_cmd == "get":
        if not args.id:
            die("usage: c2a.py jobs get ID [--wait N]")
        if args.wait is not None:
            q = "?" + urllib.parse.urlencode({"wait": args.wait})
            doc = jreq("GET", "/v1/jobs/%s%s" % (args.id, q))
        else:
            doc = jreq("GET", "/v1/jobs/%s" % args.id)
        print(json.dumps(doc, ensure_ascii=False, indent=2))
    elif args.jobs_cmd == "cancel":
        if not args.id:
            die("usage: c2a.py jobs cancel ID")
        doc = jreq("POST", "/v1/jobs/%s/cancel" % args.id, {})
        print(json.dumps(doc, ensure_ascii=False, indent=2))
    else:
        die("usage: c2a.py jobs ls | jobs get ID [--wait N] | jobs cancel ID")


def cmd_selftest(args):
    doc = jreq("POST", "/v1/selftest", {})
    ok_all = doc.get("all_ok")
    for d in doc.get("drives", []):
        if d.get("ok"):
            print("%-16s OK    %4d ms  crc64=%s" % (d.get("drive"), d.get("latency_ms", 0), d.get("crc64")))
        else:
            print("%-16s FAIL  %s" % (d.get("drive"), d.get("error")))
    if not ok_all:
        sys.exit(1)


def cmd_keys(args):
    if args.keys_cmd == "ls":
        doc = jreq("GET", "/v1/keys")
        for k in doc.get("keys", []):
            print("%-24s revoked=%-5s expires %s" % (k.get("id"), k.get("revoked"), k.get("expires_at")))
    elif args.keys_cmd == "create":
        drives = []
        for spec in args.drives:
            parts = spec.split(":", 2)
            drive = parts[0]
            root = parts[1] if len(parts) > 1 else "/"
            perms = parts[2].split(",") if len(parts) > 2 else ["list", "read"]
            drives.append({"drive": drive, "root": root, "perms": perms})
        body = {"drives": drives, "ttl_seconds": args.ttl}
        if args.key_id:
            body["id"] = args.key_id
        if args.global_perms:
            body["global_perms"] = args.global_perms.split(",")
        doc = jreq("POST", "/v1/keys", body)
        k = doc.get("key", {})
        print("child key %s" % k.get("id"))
        print("plaintext (shown once): %s" % doc.get("plaintext"))
        print("expires: %s" % doc.get("expires_at"))
    elif args.keys_cmd == "rm":
        doc = jreq("DELETE", "/v1/keys/%s" % args.key_id)
        print(json.dumps(doc, ensure_ascii=False, indent=2))
    else:
        die("usage: c2a.py keys ls | keys create --drives DRIVE[:ROOT][:PERMS]... | keys rm ID")


def cmd_activity(args):
    q = "?" + urllib.parse.urlencode({"limit": args.limit})
    doc = jreq("GET", "/v1/activity" + q)
    for e in doc.get("entries", []):
        print("%s %-8s %-14s %-6s %s" % (e.get("ts"), e.get("drive"), e.get("op"), e.get("status"), e.get("path")))


def cmd_trash(args):
    if args.trash_cmd == "ls":
        doc = jreq("GET", "/v1/trash")
        for it in doc.get("items", []):
            print("%-6s %-12s %s" % (it.get("id"), it.get("drive"), it.get("path")))
    elif args.trash_cmd == "restore":
        doc = jreq("POST", "/v1/trash/restore", {"id": int(args.id)})
        print(json.dumps(doc, ensure_ascii=False, indent=2))
    else:
        die("usage: c2a.py trash ls | trash restore ID")


def main():
    ap = argparse.ArgumentParser(prog="c2a.py", description="cloud2api helper CLI")
    sub = ap.add_subparsers(dest="cmd", required=True)

    sub.add_parser("me").set_defaults(fn=cmd_me)

    p = sub.add_parser("ls");    p.add_argument("path", nargs="?", default="/"); add_drive(p); p.set_defaults(fn=cmd_ls)
    p = sub.add_parser("tree");  p.add_argument("path", nargs="?", default="/")
    p.add_argument("--depth", type=int, default=3); p.add_argument("--max-entries", type=int, default=2000)
    add_drive(p); p.set_defaults(fn=cmd_tree)
    p = sub.add_parser("search"); p.add_argument("query"); p.add_argument("--path", default="/"); add_drive(p); p.set_defaults(fn=cmd_search)
    p = sub.add_parser("stat");  p.add_argument("path"); add_drive(p); p.set_defaults(fn=cmd_stat)
    p = sub.add_parser("put");   p.add_argument("local"); p.add_argument("remote")
    p.add_argument("--on-conflict", default="fail", choices=["fail", "overwrite", "rename"])
    add_drive(p); p.set_defaults(fn=cmd_put)
    p = sub.add_parser("get");   p.add_argument("remote"); p.add_argument("local", nargs="?"); add_drive(p); p.set_defaults(fn=cmd_get)
    p = sub.add_parser("rm");    p.add_argument("paths", nargs="+"); p.add_argument("--dry-run", action="store_true"); add_drive(p); p.set_defaults(fn=cmd_rm)
    p = sub.add_parser("mv");    p.add_argument("frm"); p.add_argument("to")
    p.add_argument("--on-conflict", default="fail", choices=["fail", "overwrite", "rename"])
    p.add_argument("--dry-run", action="store_true"); add_drive(p); p.set_defaults(fn=cmd_mv)
    p = sub.add_parser("cp");    p.add_argument("frm"); p.add_argument("to")
    p.add_argument("--from-drive", default=None); p.add_argument("--to-drive", default=None)
    p.add_argument("--on-conflict", default="fail", choices=["fail", "overwrite", "rename"])
    p.add_argument("--move", action="store_true")
    p.add_argument("--dry-run", action="store_true")
    p.add_argument("--wait", type=int, default=120); p.set_defaults(fn=cmd_cp)
    p = sub.add_parser("fetch"); p.add_argument("url"); p.add_argument("path")
    p.add_argument("--drive", default=None)
    p.add_argument("--on-conflict", default="fail", choices=["fail", "overwrite", "rename"])
    p.add_argument("--wait", type=int, default=120); p.set_defaults(fn=cmd_fetch)
    p = sub.add_parser("mkdir"); p.add_argument("path"); add_drive(p); p.set_defaults(fn=cmd_mkdir)
    p = sub.add_parser("link");  p.add_argument("path"); p.add_argument("--expires-in", type=int, default=3600); p.set_defaults(fn=cmd_link)
    p = sub.add_parser("jobs");  p.add_argument("jobs_cmd", choices=["ls", "get", "cancel"])
    p.add_argument("id", nargs="?")
    p.add_argument("--state", default=None); p.add_argument("--wait", type=int, default=None); p.set_defaults(fn=cmd_jobs)
    p = sub.add_parser("selftest"); p.set_defaults(fn=cmd_selftest)
    p = sub.add_parser("keys");  p.add_argument("keys_cmd", choices=["ls", "create", "rm"])
    p.add_argument("key_id", nargs="?")
    p.add_argument("--drives", nargs="+", default=[], help="each DRIVE[:ROOT][:PERMS,PERMS]")
    p.add_argument("--global-perms", default=None)
    p.add_argument("--ttl", type=int, default=86400); p.set_defaults(fn=cmd_keys)
    p = sub.add_parser("activity"); p.add_argument("--limit", type=int, default=50); p.set_defaults(fn=cmd_activity)
    p = sub.add_parser("trash"); p.add_argument("trash_cmd", choices=["ls", "restore"]); p.add_argument("id", nargs="?"); p.set_defaults(fn=cmd_trash)

    args = ap.parse_args()
    try:
        args.fn(args)
    except ApiError as e:
        die(None, e.doc if isinstance(e.doc, dict) else {"error": {"message": str(e)}})
    except KeyboardInterrupt:
        sys.exit(130)


def add_drive(p):
    p.add_argument("--drive", default=None, help="target a specific drive (default: the key default drive)")


if __name__ == "__main__":
    main()
