#!/usr/bin/env python3
"""Fake pgCellar API for the updater tests.

Same shapes as internal/api: GET /api/v1/updates answers {"data": {...}}
(internal/updates.Status plus the settings), GET /api/v1/updates/ready answers
{"ready", "reason", "window"} without "data", 200 or 409, and takes ?now=1.

Reads its state from files in the directory given as first argument, so the
test runner and the fake docker drive it without restarting it:
  port        written at start: the port it listens on
  key         the API key it accepts
  running     the version that runs now (written by the fake docker)
  latest      the latest release
  image       manifest.image
  digest      manifest.image_digest
  ready       "200" or "409" (inside or outside the window, or busy)
  busy        when present, /ready says 409 even with ?now=1
  queries     appended by the server: every /ready query string seen
  broken      versions (one per line) whose /healthz never answers 200
"""
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

STATE = sys.argv[1]


def read(name, default=""):
    try:
        with open(os.path.join(STATE, name)) as f:
            return f.read().strip()
    except FileNotFoundError:
        return default


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def send(self, code, body):
        data = body if isinstance(body, bytes) else json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        running = read("running")
        if self.path == "/healthz":
            if running and running in read("broken").split():
                self.send(503, b"starting\n")
            else:
                self.send(200, b"ok\n")
            return
        if self.headers.get("Authorization") != "Bearer " + read("key"):
            self.send(401, {"error": "unauthorized"})
            return
        latest = read("latest")
        path, _, query = self.path.partition("?")
        window = "02:00-05:00"
        if path == "/api/v1/updates":
            self.send(200, {"data": {
                "current": running,
                "current_known": True,
                "latest": latest,
                "available": latest != running,
                "checked_at": "2026-10-21T02:10:00Z",
                "notes_url": "https://github.com/dbfactory-io/pgcellar/releases/tag/v" + latest,
                "manifest": {
                    "version": latest,
                    "published_at": "2026-10-21T01:00:00Z",
                    "image": read("image"),
                    "image_digest": read("digest"),
                    "binaries": {},
                    "notes_url": "https://github.com/dbfactory-io/pgcellar/releases/tag/v" + latest,
                },
                "check": True, "interval": "1 day", "auto": False,
                "window": window, "in_window": True, "docker": True,
            }})
        elif path == "/api/v1/updates/ready":
            with open(os.path.join(STATE, "queries"), "a") as f:
                f.write(query + "\n")
            if os.path.exists(os.path.join(STATE, "busy")):
                self.send(409, {"ready": False, "reason": "a backup of chutag is running", "window": window})
            elif read("ready", "200") == "409" and query != "now=1":
                self.send(409, {"ready": False, "reason": "outside the update window 02:00-05:00", "window": window})
            else:
                self.send(200, {"ready": True, "window": window})
        else:
            self.send(404, {"error": "not found"})


server = HTTPServer(("127.0.0.1", 0), Handler)
with open(os.path.join(STATE, "port"), "w") as f:
    f.write(str(server.server_address[1]))
server.serve_forever()
