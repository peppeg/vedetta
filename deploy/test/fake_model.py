#!/usr/bin/env python3
"""A model that costs nothing, for deploy/test/install.sh: it searches once, reads one
page, then writes an answer that cites it. Any later request gets the same answer."""
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PAGE = "https://www.iana.org/help/example-domains"
ANSWER = (
    "The example domains are reserved for documentation, and nobody can register them "
    f"([IANA]({PAGE})). They may be used in documents without asking for permission."
)


def call(name, args):
    return {"tool_calls": [{"index": 0, "id": f"c-{name}", "type": "function",
                            "function": {"name": name, "arguments": json.dumps(args)}}]}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def send(self, body, kind):
        self.send_response(200)
        self.send_header("Content-Type", kind)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        self.send(json.dumps({"object": "list", "data": [{"id": "fake", "context_length": 131072}]}).encode(), "application/json")

    def do_POST(self):
        req = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
        done = [c["function"]["name"] for m in req.get("messages", []) for c in m.get("tool_calls") or []]
        delta, why = {"content": ANSWER}, "stop"
        if req.get("tools") and "search" not in done:
            delta, why = call("search", {"query": "example domains reserved documentation", "lang": "en"}), "tool_calls"
        elif req.get("tools") and "read" not in done:
            delta, why = call("read", {"url": PAGE}), "tool_calls"
        chunk = {"id": "x", "object": "chat.completion.chunk", "model": "fake", "choices": [{"index": 0, "delta": delta, "finish_reason": why}]}
        if req.get("stream"):
            self.send(b"data: " + json.dumps(chunk).encode() + b"\n\ndata: [DONE]\n\n", "text/event-stream")
        else:
            msg = {"role": "assistant", "content": delta.get("content", "")}
            self.send(json.dumps({"id": "x", "model": "fake", "choices": [{"index": 0, "message": msg, "finish_reason": "stop"}]}).encode(), "application/json")


ThreadingHTTPServer(("0.0.0.0", int(sys.argv[1])), Handler).serve_forever()
