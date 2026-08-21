#!/usr/bin/env python3
"""Minimal smart-HTTP git server: wraps `git http-backend` as CGI.

Argo CD resolves revisions with go-git, which speaks only the smart HTTP
protocol, so a static file server (dumb HTTP) is not enough.

Usage: git-smart-http.py <git-project-root> <port>
"""
import os
import subprocess
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = os.path.abspath(sys.argv[1])
PORT = int(sys.argv[2])


def find_backend():
    """Locate git-http-backend. `git --exec-path` is authoritative across distributions."""
    override = os.environ.get("GIT_HTTP_BACKEND")
    if override:
        return override
    try:
        exec_path = subprocess.run(
            ["git", "--exec-path"], stdout=subprocess.PIPE, check=True, text=True
        ).stdout.strip()
    except (OSError, subprocess.CalledProcessError):
        exec_path = ""
    candidates = [os.path.join(exec_path, "git-http-backend")] if exec_path else []
    candidates += [
        "/usr/lib/git-core/git-http-backend",
        "/usr/libexec/git-core/git-http-backend",
    ]
    for candidate in candidates:
        if os.path.isfile(candidate) and os.access(candidate, os.X_OK):
            return candidate
    sys.exit(
        "git-http-backend not found; set GIT_HTTP_BACKEND to its path. Tried: "
        + ", ".join(candidates)
    )


BACKEND = find_backend()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.0"

    def run_backend(self, method):
        path, _, query = self.path.partition("?")
        env = {
            "GIT_PROJECT_ROOT": ROOT,
            "GIT_HTTP_EXPORT_ALL": "1",
            "PATH_INFO": path,
            "QUERY_STRING": query,
            "REQUEST_METHOD": method,
            "REMOTE_ADDR": self.client_address[0],
            "PATH": os.environ.get("PATH", ""),
        }
        for header, var in (
            ("Content-Type", "CONTENT_TYPE"),
            ("Content-Length", "CONTENT_LENGTH"),
            ("Content-Encoding", "HTTP_CONTENT_ENCODING"),
            ("Git-Protocol", "HTTP_GIT_PROTOCOL"),
        ):
            value = self.headers.get(header)
            if value:
                env[var] = value

        body = b""
        length = int(self.headers.get("Content-Length") or 0)
        if length:
            body = self.rfile.read(length)

        proc = subprocess.run(
            [BACKEND], env=env, input=body, stdout=subprocess.PIPE, stderr=subprocess.PIPE
        )
        if proc.returncode != 0:
            self.send_response(500)
            self.end_headers()
            self.wfile.write(proc.stderr)
            return

        head, _, payload = proc.stdout.partition(b"\r\n\r\n")
        headers = [line for line in head.split(b"\r\n") if line]
        status = 200
        emit = []
        for line in headers:
            name, _, value = line.partition(b":")
            if name.strip().lower() == b"status":
                status = int(value.strip().split()[0])
            else:
                emit.append((name.decode(), value.strip().decode()))
        self.send_response(status)
        for name, value in emit:
            self.send_header(name, value)
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        self.run_backend("GET")

    def do_POST(self):
        self.run_backend("POST")


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
