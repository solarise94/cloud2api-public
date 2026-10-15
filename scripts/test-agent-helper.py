#!/usr/bin/env python3
"""Local tests for the BASIC c2a.py helper served from internal/agentapi/c2apy.go.

Everything runs against a local mock HTTP server; no real endpoints are
contacted and no repository artifacts are produced (compiled copies and
downloads live in a temp directory).

Run:  python3 scripts/test-agent-helper.py
"""

import hashlib
import json
import os
import py_compile
import re
import subprocess
import sys
import tempfile
import threading
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
C2APY_GO = os.path.join(ROOT, "internal", "agentapi", "c2apy.go")

DEFAULT_DRIVE = "d1"
BASIC_COMMANDS = ["me", "ls", "stat", "put", "get", "rm", "mv", "cp", "mkdir", "link", "jobs"]
ADVANCED_COMMANDS = ["tree", "search", "fetch", "selftest", "keys", "activity", "trash"]
ADVANCED_URLS = [
    "/v1/selftest", "/v1/tree", "/v1/search", "/v1/batch", "/v1/fetch",
    "/v1/keys", "/v1/activity", "/v1/trash", "/v1/list", "/v1/stat?",
    "/v1/files?", "/v1/mkdir", "/v1/delete?", "/v1/move?", "/api/", "/custom/gokapi",
]

failures = []
MOCK = {"transfer_id": "job-1"}  # which job id POST /v1/transfers hands out


def check(cond, msg):
    print(("ok   " if cond else "FAIL ") + msg)
    if not cond:
        failures.append(msg)


def extract_helper():
    with open(C2APY_GO, encoding="utf-8") as f:
        src = f.read()
    return src.split("const c2aPySource = `", 1)[1].rsplit("`", 1)[0]


class MockHandler(BaseHTTPRequestHandler):
    requests = []  # (method, path_with_query, body_bytes)

    def _record(self):
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length) if length else b""
        MockHandler.requests.append((self.command, self.path, body))
        return body

    def _json(self, doc, headers=None):
        payload = json.dumps(doc).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        self._record()
        p = urllib.parse.urlparse(self.path).path
        if p == "/v1/me":
            self._json({
                "key": {"id": "k1", "default_drive": DEFAULT_DRIVE},
                "drives": [
                    {"id": DEFAULT_DRIVE, "perms": ["list", "read", "write", "delete", "link"], "default": True},
                    {"id": "d2", "perms": ["list", "read", "write", "link"]},
                ],
            })
        elif p == "/v1/jobs/job-1":
            self._json({"job": {"id": "job-1", "kind": "transfer", "state": "succeeded", "progress": "1/1"}})
        elif p == "/v1/jobs/job-fail":
            self._json({"job": {"id": "job-fail", "kind": "transfer", "state": "failed",
                                "error": "upstream boom"}})
        elif p == "/v1/jobs/job-canceled":
            self._json({"job": {"id": "job-canceled", "state": "canceled"}})
        elif p == "/v1/jobs/job-running":
            self._json({"job": {"id": "job-running", "state": "running"}})
        elif re.match(r"^/v1/drives/[^/]+/list$", p):
            self._json({"items": [{"name": "a.txt", "is_dir": False, "size": 3,
                                   "modified": "now", "path": "/a.txt"}], "total": 1})
        elif re.match(r"^/v1/drives/[^/]+/stat$", p):
            self._json({"path": "/a.txt", "is_dir": False, "size": 3})
        elif re.match(r"^/v1/drives/[^/]+/files$", p):
            data = b"abc"
            self.send_response(200)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(len(data)))
            self.send_header("X-C2A-SHA256", hashlib.sha256(data).hexdigest())
            self.end_headers()
            self.wfile.write(data)
        else:
            self._json({})

    def do_POST(self):
        body = self._record()
        p = urllib.parse.urlparse(self.path).path
        if p == "/v1/links":
            doc = json.loads(body or b"{}")
            self._json({"url": "http://x/l/tok", "expires_at": "soon", "drive": doc.get("drive")})
        elif p == "/v1/transfers":
            self._json({"job": {"id": MOCK["transfer_id"], "kind": "transfer", "state": "queued"}})
        elif p.endswith("/delete"):
            self._json({"results": [{"path": "/a.txt", "status": "deleted"}]})
        elif p.endswith("/move"):
            self._json({"from": "/a", "to": "/b"})
        elif p.endswith("/mkdir"):
            self._json({"path": "/x", "created": True})
        else:
            self._json({})

    def do_PUT(self):
        body = self._record()
        self._json({"path": "/up.bin", "size": len(body),
                    "sha256": hashlib.sha256(body).hexdigest(),
                    "crc64": "11051210869376104954"})

    def log_message(self, *a):
        pass


def run_helper(script, workdir, url, args):
    env = dict(os.environ)
    env["C2A_URL"] = url
    env["C2A_KEY"] = "sk-test"
    return subprocess.run([sys.executable, script] + args, env=env, cwd=workdir,
                          capture_output=True, text=True, timeout=120)


def recorded_after(handler, index):
    return handler.requests[index:]


def main():
    temp = tempfile.TemporaryDirectory(prefix="c2a-helper-test-")
    tmp = temp.name
    script = os.path.join(tmp, "c2a_basic_test.py")

    # 1. The embedded source parses.
    src = extract_helper()
    check(src.startswith("#!/usr/bin/env python3"), "extracted served helper source")
    compile(src, script, "exec")
    with open(script, "w", encoding="utf-8") as handle:
        handle.write(src)
    py_compile.compile(script, cfile=os.path.join(tmp, "c2a_basic_test.pyc"), doraise=True)
    print("ok   served helper source parses (py_compile into temp dir)")

    # 2. Static source checks: only basic commands and URLs are declared.
    found = re.findall(r'sub\.add_parser\("([a-z]+)"\)', src)
    check(found == BASIC_COMMANDS,
          "subcommands are exactly the basic set (%s)" % ", ".join(found))
    for bad in ADVANCED_URLS:
        check(bad not in src, "source does not declare %s" % bad)
    for word in ADVANCED_COMMANDS + ["advanced", "batch", "gokapi"]:
        check(word not in src.lower(), "source does not mention %r" % word)
    # "canceled" is a legal job state name; only a quoted cancel choice would
    # advertise the cancel subcommand.
    check('"cancel"' not in src, "source does not declare a cancel subcommand")

    server = ThreadingHTTPServer(("127.0.0.1", 0), MockHandler)
    url = "http://127.0.0.1:%d" % server.server_address[1]
    threading.Thread(target=server.serve_forever, daemon=True).start()

    try:
        # 3. --help: basic commands only, zero HTTP requests.
        before = len(MockHandler.requests)
        p = run_helper(script, tmp, url, ["--help"])
        made = MockHandler.requests[before:]
        check(p.returncode == 0, "--help exits 0")
        check(made == [], "--help makes no HTTP calls")
        for cmd in BASIC_COMMANDS:
            check(cmd in p.stdout, "--help lists %s" % cmd)
        for cmd in ADVANCED_COMMANDS:
            check(cmd not in p.stdout, "--help omits %s" % cmd)
        check("approval" not in p.stdout, "--help has no approval command")

        # 4. jobs --help: only 'get'.
        p = run_helper(script, tmp, url, ["jobs", "--help"])
        check(p.returncode == 0, "jobs --help exits 0")
        check("{get}" in p.stdout, "jobs subcommand is get-only (%r)" % p.stdout.strip().splitlines()[-1])
        check("cancel" not in p.stdout and not re.search(r"\bls\b", p.stdout),
              "jobs --help does not offer ls/cancel")

        # 5. me: a single GET, nothing mutating.
        before = len(MockHandler.requests)
        p = run_helper(script, tmp, url, ["me"])
        made = MockHandler.requests[before:]
        check(p.returncode == 0, "me exits 0")
        check([(m, path.split("?")[0]) for m, path, _ in made] == [("GET", "/v1/me")],
              "me issues exactly GET /v1/me (%s)" % made)

        # 6. Default drive discovery: me -> /v1/drives/d1/list (never /v1/list).
        before = len(MockHandler.requests)
        p = run_helper(script, tmp, url, ["ls", "/"])
        made = MockHandler.requests[before:]
        paths = [urllib.parse.urlparse(path).path for _, path, _ in made]
        check(p.returncode == 0, "ls exits 0")
        check(paths == ["/v1/me", "/v1/drives/%s/list" % DEFAULT_DRIVE],
              "ls resolves default_drive via me and uses the per-drive URL (%s)" % paths)
        check(all(not path.startswith("/v1/list") for _, path, _ in made),
              "ls never calls the default-drive alias /v1/list")

        # 7. Explicit --drive is percent-encoded into the per-drive URL.
        before = len(MockHandler.requests)
        p = run_helper(script, tmp, url, ["ls", "/", "--drive", "my drive"])
        made = MockHandler.requests[before:]
        check(any(path.split("?")[0] == "/v1/drives/my%20drive/list" for _, path, _ in made),
              "explicit --drive is quoted into /v1/drives/<drive>/list (%s)" % [p_ for _, p_, _ in made])
        check(p.returncode == 0, "ls --drive exits 0")

        # 8. link: --drive lands in the body; default otherwise from me.
        before = len(MockHandler.requests)
        p = run_helper(script, tmp, url, ["link", "/f.bin", "--drive", "d2"])
        made = MockHandler.requests[before:]
        links = [b for m, path, b in made if path.startswith("/v1/links")]
        check(len(links) == 1 and json.loads(links[0]).get("drive") == "d2",
              "link --drive d2 sends body drive=d2 (%s)" % links)
        check(p.returncode == 0, "link --drive exits 0")
        before = len(MockHandler.requests)
        p = run_helper(script, tmp, url, ["link", "/f.bin"])
        made = MockHandler.requests[before:]
        links = [b for m, path, b in made if path.startswith("/v1/links")]
        check(len(links) == 1 and json.loads(links[0]).get("drive") == DEFAULT_DRIVE,
              "link without --drive uses default_drive from me (%s)" % links)
        check(p.returncode == 0, "link exits 0")

        # 9. jobs get: read-only query of one job.
        before = len(MockHandler.requests)
        p = run_helper(script, tmp, url, ["jobs", "get", "job-1", "--wait", "1"])
        made = MockHandler.requests[before:]
        check(p.returncode == 0, "jobs get exits 0")
        check(all(m == "GET" for m, _, _ in made), "jobs get performs no mutating calls")
        check(any(path.startswith("/v1/jobs/job-1") for _, path, _ in made),
              "jobs get queries /v1/jobs/{id} (%s)" % [p_ for _, p_, _ in made])
        check("succeeded" in p.stdout, "jobs get prints the job state")

        # 10. rm --dry-run posts dry_run to the per-drive delete endpoint.
        before = len(MockHandler.requests)
        p = run_helper(script, tmp, url, ["rm", "/a.txt", "--dry-run"])
        made = MockHandler.requests[before:]
        dels = [(path, b) for m, path, b in made if path.startswith("/v1/drives/")]
        check(len(dels) == 1 and dels[0][0].startswith("/v1/drives/%s/delete" % DEFAULT_DRIVE),
              "rm --dry-run targets /v1/drives/<drive>/delete (%s)" % dels)
        check(json.loads(dels[0][1]).get("dry_run") is True, "rm --dry-run sends dry_run=true")
        check("would affect" in p.stdout, "rm --dry-run prints would_affect")
        check("trash" not in p.stdout.lower(), "rm does not suggest a trash command")

        # 11. put/get roundtrip with SHA-256 verification.
        local = os.path.join(tmp, "local.bin")
        with open(local, "wb") as f:
            f.write(b"abc")
        before = len(MockHandler.requests)
        p = run_helper(script, tmp, url, ["put", local, "/up.bin"])
        made = MockHandler.requests[before:]
        puts = [(m, path.split("?")[0]) for m, path, _ in made]
        check(("PUT", "/v1/drives/%s/files" % DEFAULT_DRIVE) in puts,
              "put uploads to the per-drive files URL (%s)" % puts)
        check(p.returncode == 0, "put exits 0 with matching checksums")
        out = os.path.join(tmp, "out.bin")
        p = run_helper(script, tmp, url, ["get", "/a.txt", out])
        check(p.returncode == 0 and open(out, "rb").read() == b"abc",
              "get downloads and verifies the file")

        # 12. mkdir and mv target the per-drive endpoints.
        before = len(MockHandler.requests)
        p = run_helper(script, tmp, url, ["mkdir", "/x"])
        made = MockHandler.requests[before:]
        check(any(path.startswith("/v1/drives/%s/mkdir" % DEFAULT_DRIVE) for _, path, _ in made),
              "mkdir targets the per-drive endpoint")
        before = len(MockHandler.requests)
        p = run_helper(script, tmp, url, ["mv", "/a", "/b"])
        made = MockHandler.requests[before:]
        check(any(path.startswith("/v1/drives/%s/move" % DEFAULT_DRIVE) for _, path, _ in made),
              "mv targets the per-drive endpoint")

        # 13. cp waits for the transfer job; exit 0 only on success.
        MOCK["transfer_id"] = "job-1"
        before = len(MockHandler.requests)
        p = run_helper(script, tmp, url, ["cp", "/a.bin", "/b.bin", "--from-drive", DEFAULT_DRIVE,
                                          "--to-drive", "d2", "--wait", "5"])
        made = MockHandler.requests[before:]
        transfers = [b for m, path, b in made if path.startswith("/v1/transfers")]
        check(len(transfers) == 1 and json.loads(transfers[0])["to"]["drive"] == "d2",
              "cp posts the transfer with explicit drives (%s)" % transfers)
        check(p.returncode == 0, "cp exits 0 when the job succeeds")

        MOCK["transfer_id"] = "job-fail"
        before = len(MockHandler.requests)
        p = run_helper(script, tmp, url, ["cp", "/a.bin", "/b.bin", "--wait", "5"])
        made = MockHandler.requests[before:]
        transfers = [b for m, path, b in made if path.startswith("/v1/transfers")]
        body = json.loads(transfers[0]) if transfers else {}
        check(body.get("from", {}).get("drive") == DEFAULT_DRIVE
              and body.get("to", {}).get("drive") == DEFAULT_DRIVE,
              "cp without --drive resolves both sides from me (%s)" % transfers)
        check(p.returncode != 0, "cp exits non-zero when the job fails")
        check("not succeeded" in (p.stderr + p.stdout), "cp explains the failed job")

        for job_id, state in (("job-canceled", "canceled"), ("job-running", "running")):
            MOCK["transfer_id"] = job_id
            p = run_helper(script, tmp, url, ["cp", "/a.bin", "/b.bin", "--wait", "0"])
            check(p.returncode != 0, "cp exits non-zero for %s job" % state)
            check("not succeeded" in p.stderr and state in p.stderr,
                  "cp reports unconfirmed completion for %s job" % state)
    finally:
        server.shutdown()
        server.server_close()
        temp.cleanup()

    print()
    if failures:
        print("%d check(s) FAILED" % len(failures))
        sys.exit(1)
    print("all checks passed")


if __name__ == "__main__":
    main()
