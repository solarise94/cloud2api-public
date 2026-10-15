#!/usr/bin/env python3
"""connect 页面开发服务器。

以生产 CSP 提供 /@connect/ 下的静态页面与开发用 mock.js：
  /@connect            → 301 到 /@connect/
  /@connect/           → src/server/connect/web/index.html
  /@connect/app.*      → 同目录静态文件
  /@connect/mock.js    → dev/connect-mock.js（生产构建不存在）
  其余路径             → 404

用法：python3 serve.py [端口]     # 默认 8098，监听 127.0.0.1
"""

import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

DEV_DIR = os.path.dirname(os.path.abspath(__file__))
WEB_DIR = os.path.realpath(os.path.join(DEV_DIR, os.pardir, "src", "server", "connect", "web"))
MOCK_FILE = os.path.join(DEV_DIR, "connect-mock.js")

CSP = ("default-src 'self'; script-src 'self'; style-src 'self'; "
       "img-src 'self' https: data: blob:; connect-src 'self'; object-src 'none'; "
       "base-uri 'none'; form-action 'self'; frame-ancestors 'none'")

CONTENT_TYPES = {
    ".html": "text/html; charset=utf-8",
    ".js": "text/javascript; charset=utf-8",
    ".css": "text/css; charset=utf-8",
    ".svg": "image/svg+xml",
    ".png": "image/png",
    ".ico": "image/x-icon",
}


class Handler(BaseHTTPRequestHandler):
    server_version = "connect-dev/1.0"

    # ----- 基础响应 -----
    def _respond(self, status, body, ctype="text/plain; charset=utf-8", headers=None):
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Content-Security-Policy", CSP)
        self.send_header("X-Content-Type-Options", "nosniff")
        self.send_header("Cache-Control", "no-store")
        for key, value in (headers or {}).items():
            self.send_header(key, value)
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def _not_found(self):
        self._respond(404, "404 not found\n".encode("utf-8"))

    def _serve_file(self, path, ctype=None):
        try:
            with open(path, "rb") as handle:
                body = handle.read()
        except OSError:
            self._not_found()
            return
        if ctype is None:
            ctype = CONTENT_TYPES.get(os.path.splitext(path)[1], "application/octet-stream")
        self._respond(200, body, ctype)

    # ----- 路由 -----
    def _route(self):
        path = self.path.split("?", 1)[0].split("#", 1)[0]
        if path in ("/@connect", "/@connect/index.html"):
            self._respond(301, b"", "text/plain", {"Location": "/@connect/"})
        elif path == "/@connect/":
            self._serve_file(os.path.join(WEB_DIR, "index.html"))
        elif path == "/@connect/mock.js":
            self._serve_file(MOCK_FILE)
        elif path.startswith("/@connect/"):
            rel = path[len("/@connect/"):]
            full = os.path.realpath(os.path.join(WEB_DIR, rel))
            if os.path.isfile(full) and full.startswith(WEB_DIR + os.sep):
                self._serve_file(full)
            else:
                self._not_found()
        else:
            self._not_found()

    do_GET = _route
    do_HEAD = _route

    def log_message(self, fmt, *args):
        sys.stderr.write("[%s] %s\n" % (self.log_date_time_string(), fmt % args))


def main():
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8098
    server = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    print("serving /@connect/ from %s" % WEB_DIR)
    print("open http://127.0.0.1:%d/@connect/  (mock: /@connect/?mock=1)" % port)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
