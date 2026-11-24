#!/usr/bin/env python3
"""LLM fault proxy for the client retry tests (V25-T7.1.5, docs/SPEC_T71_RITENTATIVI_LLM.md).

Forwards every request to the real LLM server (streaming included), except
inside a fault window, where it answers like a LiteLLM gateway in trouble:
  503  "No available servers: No available decode workers ..."
  429  "No deployments available for selected model, Try again in N seconds"

The window is measured from the first request the proxy receives, so it does
not depend on how long Vedetta takes to start.

Usage:
  python3 scripts/llm_fault_proxy.py --upstream http://HOST:PORT [--port 8740]
      [--fail 503|429] [--from SECS] [--for SECS] [--retry-in SECS]

Examples (section 3 of the spec):
  --fail 503 --from 60 --for 20        a 20 s hiccup, one minute after the first call
  --fail 429 --from 60 --for 60 --retry-in 137
  --fail 503 --from 60 --for 300       a long outage, like round m1E_1

Every request is logged on stderr with its time from the first request and
whether it was forwarded or failed. Only the Python standard library is used.
"""

import argparse
import http.client
import json
import sys
import threading
import time
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HOP_HEADERS = {"connection", "keep-alive", "transfer-encoding", "content-length", "host"}


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--upstream", required=True, help="real LLM server, e.g. http://HOST:8731")
    ap.add_argument("--port", type=int, default=8740)
    ap.add_argument("--fail", type=int, choices=[503, 429], default=503)
    ap.add_argument("--from", dest="start", type=float, default=60, help="window start, seconds after the first request")
    ap.add_argument("--for", dest="length", type=float, default=0, help="window length in seconds (0 = never fail)")
    ap.add_argument("--retry-in", type=float, default=137, help="seconds in the 429 'Try again in' text")
    args = ap.parse_args()

    up = urllib.parse.urlsplit(args.upstream)
    if up.scheme not in ("http", "https") or not up.hostname:
        sys.exit("llm_fault_proxy: --upstream must be http(s)://host:port")

    state = {"t0": None, "forwarded": 0, "failed": 0}
    lock = threading.Lock()

    def elapsed():
        with lock:
            if state["t0"] is None:
                state["t0"] = time.monotonic()
            return time.monotonic() - state["t0"]

    def in_window(t):
        return args.length > 0 and args.start <= t < args.start + args.length

    def fault_body():
        if args.fail == 429:
            msg = (f"No deployments available for selected model, Try again in {args.retry_in:.1f} seconds. "
                   "Passed model=fault-proxy. pre-call-checks=True")
            return json.dumps({"error": {"message": msg, "type": "None", "param": "None", "code": "429"}})
        msg = ("litellm.ServiceUnavailableError: ServiceUnavailableError: OpenAIException - "
               "No available servers: No available decode workers (all circuits open or unhealthy)")
        return json.dumps({"error": {"message": msg, "type": None, "param": None, "code": "503"}})

    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.0"  # close-delimited bodies: streaming passes through unchanged

        def log_message(self, fmt, *a):
            pass

        def _note(self, t, what):
            with lock:
                state[what] += 1
                n = state["forwarded"] + state["failed"]
            print(f"[fault-proxy] t={t:7.1f}s #{n} {self.command} {self.path} -> {what}", file=sys.stderr, flush=True)

        def _handle(self):
            t = elapsed()
            body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
            if in_window(t):
                self._note(t, "failed")
                data = fault_body().encode()
                self.send_response(args.fail)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)
                return
            self._note(t, "forwarded")
            conn_cls = http.client.HTTPSConnection if up.scheme == "https" else http.client.HTTPConnection
            conn = conn_cls(up.hostname, up.port, timeout=600)
            try:
                headers = {k: v for k, v in self.headers.items() if k.lower() not in HOP_HEADERS}
                conn.request(self.command, self.path, body=body or None, headers=headers)
                resp = conn.getresponse()
                self.send_response(resp.status)
                for k, v in resp.getheaders():
                    if k.lower() not in HOP_HEADERS:
                        self.send_header(k, v)
                self.end_headers()
                while chunk := resp.read1(65536):
                    self.wfile.write(chunk)
                    self.wfile.flush()
            except (OSError, http.client.HTTPException) as e:
                print(f"[fault-proxy] upstream error: {e}", file=sys.stderr, flush=True)
            finally:
                conn.close()

        do_GET = _handle
        do_POST = _handle

    srv = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    window = (f"{args.fail} from {args.start:g}s for {args.length:g}s" if args.length > 0 else "none")
    print(f"[fault-proxy] listening on 127.0.0.1:{args.port} -> {up.scheme}://{up.netloc}, fault window: {window}",
          file=sys.stderr, flush=True)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
    print(f"[fault-proxy] forwarded {state['forwarded']}, failed {state['failed']}", file=sys.stderr, flush=True)


if __name__ == "__main__":
    main()
