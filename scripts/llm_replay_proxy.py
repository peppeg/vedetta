#!/usr/bin/env python3
"""Sits between a benchmark instance and the LLM, so two methods can start from one draft.

  record  every request goes to the LLM; the answers are saved, except the send-back ones
  replay  a request saved before gets the saved answer; anything else goes to the LLM

A request is the same when its body is the same byte for byte. The answers of the
research up to the first draft are the same in every run, because the tools answer from
the same cache. A request holding the sentinel's send-back is never saved nor replayed:
that is the part being compared, and it must be a new answer each time.

Usage: llm_replay_proxy.py --upstream URL --dir DIR --mode record|replay [--port 3099] [--log FILE]
Headers are passed on and never written anywhere: the log holds only hashes and sizes.
"""
import argparse
import hashlib
import http.client
import json
import os
import sys
import threading
import time
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# Words of reviewMessage (internal/agent/loopreview.go) that only a send-back holds.
SEND_BACK = ("write the complete answer again", "reply only with these lines")
HOP_BY_HOP = {"connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailers",
              "transfer-encoding", "upgrade", "host", "content-length"}
UPSTREAM_TIMEOUT = 900  # seconds between two pieces of an answer


def is_send_back(body):
    """True when a user message of the request is the sentinel's send-back."""
    try:
        messages = json.loads(body).get("messages") or []
    except (ValueError, AttributeError):
        return False
    for m in messages:
        content = m.get("content")
        if m.get("role") == "user" and isinstance(content, str) and any(s in content for s in SEND_BACK):
            return True
    return False


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "llm-replay"

    def log_message(self, *_):
        pass  # no headers, no addresses

    def note(self, what, key, size):
        with self.server.lock:
            self.server.counts[what] = self.server.counts.get(what, 0) + 1
            if self.server.log_file:
                self.server.log_file.write(f"{time.strftime('%H:%M:%S')} {what} {key[:16]} {size}\n")
                self.server.log_file.flush()

    def do_GET(self):
        self.forward(b"")

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length)
        if not self.path.endswith("/chat/completions"):
            self.forward(body)
            return
        key = hashlib.sha256(body).hexdigest()
        path = os.path.join(self.server.dir, key + ".sse")
        if is_send_back(body):
            self.note("send-back", key, 0)
            self.forward(body)
            return
        if self.server.mode == "replay" and os.path.exists(path):
            with open(path, "rb") as f:
                saved = f.read()
            self.note("replayed", key, len(saved))
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Content-Length", str(len(saved)))
            self.end_headers()
            self.wfile.write(saved)
            return
        self.note("live" if self.server.mode == "replay" else "recorded", key, 0)
        self.forward(body, path if self.server.mode == "record" else None)

    def forward(self, body, save_to=None):
        up = self.server.upstream
        conn_cls = http.client.HTTPSConnection if up.scheme == "https" else http.client.HTTPConnection
        conn = conn_cls(up.netloc, timeout=UPSTREAM_TIMEOUT)
        headers = {k: v for k, v in self.headers.items() if k.lower() not in HOP_BY_HOP}
        target = up.path.rstrip("/") + self.path[len(self.server.prefix):]
        try:
            conn.request(self.command, target, body=body or None, headers=headers)
            resp = conn.getresponse()
        except OSError as e:
            conn.close()
            self.send_error(502, type(e).__name__)
            return
        self.send_response(resp.status)
        for k, v in resp.getheaders():
            if k.lower() not in HOP_BY_HOP:
                self.send_header(k, v)
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        kept, complete = [], False
        try:
            while True:
                piece = resp.read1(65536)
                if not piece:
                    complete = True
                    break
                kept.append(piece)
                self.wfile.write(b"%x\r\n%s\r\n" % (len(piece), piece))
                self.wfile.flush()
            self.wfile.write(b"0\r\n\r\n")
        except OSError:
            self.close_connection = True
        finally:
            conn.close()
        if save_to and complete and resp.status == 200:
            tmp = f"{save_to}.{threading.get_ident()}.tmp"
            with open(tmp, "wb") as f:
                f.write(b"".join(kept))
            os.replace(tmp, save_to)


class Server(ThreadingHTTPServer):
    daemon_threads = True

    def handle_error(self, request, client_address):
        pass  # the instance closed a connection: nothing to report, and no address in a log


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--upstream", required=True)
    ap.add_argument("--dir", required=True)
    ap.add_argument("--mode", choices=("record", "replay"), required=True)
    ap.add_argument("--port", type=int, default=3099)
    ap.add_argument("--log")
    args = ap.parse_args()
    os.makedirs(args.dir, exist_ok=True)
    up = urllib.parse.urlsplit(args.upstream)
    server = Server(("127.0.0.1", args.port), Handler)
    server.upstream, server.dir, server.mode = up, args.dir, args.mode
    server.prefix = "/v1"
    server.lock = threading.Lock()
    server.counts = {}
    server.log_file = open(args.log, "a") if args.log else None
    print(f"llm_replay_proxy: {args.mode} on port {args.port}", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    print(f"llm_replay_proxy: {server.counts}", file=sys.stderr, flush=True)


if __name__ == "__main__":
    main()
