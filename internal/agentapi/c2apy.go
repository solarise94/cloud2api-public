package agentapi

// Agents may read the downloaded source, not just its help: only basic
// commands belong here. The full helper is kept in tools/c2a-full.py.
const c2aPySource = `#!/usr/bin/env python3
"""cloud2api basic helper CLI (Python 3, standard library only).

This is the helper served at /custom/c2a.py. It covers the basic agent
surface only:

  me                              show key info, drives, permissions, limits
  ls [PATH] [--drive D]           list a directory (default /)
  stat PATH [--drive D]           file metadata incl. recorded checksums
  put LOCAL REMOTE [--drive D]    upload a file (SHA-256 verified)
  get REMOTE [LOCAL] [--drive D]  download a file (SHA-256 verified)
  rm PATH... [--dry-run]          delete paths (needs the delete permission)
  mv FROM TO [--dry-run]          move or rename within one drive
  cp SRC DST [--from-drive D] [--to-drive D] [--move] [--dry-run]
                                  copy/move as a transfer job (waits; exits
                                  non-zero unless the job succeeds)
  mkdir PATH [--drive D]          create a directory (idempotent)
  link PATH [--expires-in N] [--drive D]
                                  create a temporary direct link
  jobs get ID [--wait N]          inspect one job (long-poll with --wait)

Drive selection: pass --drive D to target drive D explicitly; when it is
omitted the helper resolves the key's default_drive from 'me' and
addresses /v1/drives/<drive>/... directly. Permissions: what is effective
on an already-granted drive is the union of the key's global_perms and
that drive's perms in 'me'; deleting needs the effective delete
permission, and a 403 forbidden answer is final.

Environment:
  C2A_URL   gateway base URL, e.g. http://198.51.100.20:8080
  C2A_KEY   API key (sk-...)
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


_default_drive = None


def default_drive():
    """Resolve (and cache) the key's default_drive from GET /v1/me."""
    global _default_drive
    if _default_drive is None:
        doc = jreq("GET", "/v1/me")
        drive = (doc.get("key") or {}).get("default_drive") or ""
        if not drive:
            die("this key has no default drive; pass --drive (run 'c2a.py me' to list drives)")
        _default_drive = drive
    return _default_drive


def target_drive(args):
    drive = getattr(args, "drive", None)
    return drive if drive else default_drive()


def drive_url(op, drive, extra=None):
    """Build the canonical per-drive URL /v1/drives/<drive>/<op>."""
    q = urllib.parse.urlencode(extra or {})
    path = "/v1/drives/%s/%s" % (urllib.parse.quote(drive, safe=""), op)
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
    doc = jreq("GET", drive_url("list", target_drive(args), {"path": logical(args.path)}))
    rows = doc.get("items", [])
    for it in rows:
        kind = "d" if it.get("is_dir") else "-"
        print("%s %12d  %s  %s" % (kind, it.get("size", 0), it.get("modified", ""), it.get("path", it.get("name"))))
    print("-- %d item(s), total %s" % (len(rows), doc.get("total")))


def cmd_stat(args):
    doc = jreq("GET", drive_url("stat", target_drive(args), {"path": logical(args.path)}))
    print(json.dumps(doc, ensure_ascii=False, indent=2))


def cmd_put(args):
    local, remote = args.local, logical(args.remote)
    if not os.path.isfile(local):
        die("local file not found: " + local)
    local_sha = sha256_file(local)
    size = os.path.getsize(local)
    url = drive_url("files", target_drive(args), {"path": remote, "on_conflict": args.on_conflict})
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
    url = drive_url("files", target_drive(args), {"path": remote})
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
    doc = jreq("POST", drive_url("delete", target_drive(args)), body)
    if body.get("dry_run"):
        wa = doc.get("would_affect", {})
        print("would affect %s file(s), %s dir(s), %s byte(s)" % (wa.get("files"), wa.get("dirs"), wa.get("bytes")))
        for it in doc.get("items", [])[:200]:
            print("  " + it)
        return
    for r in doc.get("results", []):
        print("%-10s %s" % (r.get("status"), r.get("path")))


def cmd_mv(args):
    body = {"from": logical(args.frm), "to": logical(args.to), "on_conflict": args.on_conflict}
    if args.dry_run:
        body["dry_run"] = True
    doc = jreq("POST", drive_url("move", target_drive(args)), body)
    print(json.dumps(doc, ensure_ascii=False, indent=2))


def cmd_cp(args):
    frm = {"drive": args.from_drive if args.from_drive else default_drive(),
           "path": logical(args.frm)}
    to = {"drive": args.to_drive if args.to_drive else default_drive(),
          "path": logical(args.to)}
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
    job = wait_job(job.get("id"), args.wait)
    if job.get("state") != "succeeded":
        die("transfer job %s has not succeeded (state=%s); inspect with: c2a.py jobs get %s"
            % (job.get("id"), job.get("state"), job.get("id")))


def wait_job(job_id, wait_seconds):
    if not job_id:
        die("no job id in server response")
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


def cmd_mkdir(args):
    doc = jreq("POST", drive_url("mkdir", target_drive(args)), {"path": logical(args.path)})
    print(json.dumps(doc, ensure_ascii=False, indent=2))


def cmd_link(args):
    body = {"drive": target_drive(args), "path": logical(args.path),
            "expires_in": args.expires_in}
    doc = jreq("POST", "/v1/links", body)
    print(json.dumps(doc, ensure_ascii=False, indent=2))


def cmd_jobs(args):
    if args.jobs_cmd != "get" or not args.id:
        die("usage: c2a.py jobs get ID [--wait N]")
    q = ""
    if args.wait is not None:
        q = "?" + urllib.parse.urlencode({"wait": args.wait})
    doc = jreq("GET", "/v1/jobs/%s%s" % (args.id, q))
    print(json.dumps(doc, ensure_ascii=False, indent=2))


def main():
    ap = argparse.ArgumentParser(prog="c2a.py", description="cloud2api basic helper CLI")
    sub = ap.add_subparsers(dest="cmd", required=True)

    sub.add_parser("me").set_defaults(fn=cmd_me)

    p = sub.add_parser("ls");    p.add_argument("path", nargs="?", default="/"); add_drive(p); p.set_defaults(fn=cmd_ls)
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
    p = sub.add_parser("mkdir"); p.add_argument("path"); add_drive(p); p.set_defaults(fn=cmd_mkdir)
    p = sub.add_parser("link");  p.add_argument("path"); p.add_argument("--expires-in", type=int, default=3600); add_drive(p); p.set_defaults(fn=cmd_link)
    p = sub.add_parser("jobs");  p.add_argument("jobs_cmd", choices=["get"])
    p.add_argument("id", nargs="?")
    p.add_argument("--wait", type=int, default=None); p.set_defaults(fn=cmd_jobs)

    args = ap.parse_args()
    try:
        args.fn(args)
    except ApiError as e:
        die(None, e.doc if isinstance(e.doc, dict) else {"error": {"message": str(e)}})
    except KeyboardInterrupt:
        sys.exit(130)


def add_drive(p):
    p.add_argument("--drive", default=None, help="target a specific drive (default: the key default_drive from 'me')")


if __name__ == "__main__":
    main()
`
