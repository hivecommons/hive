#!/usr/bin/env python3
"""Stub OpenAI-compatible chat-completions provider for the repeat-failure
harnesses. The first request in a conversation answers with one shell tool
call running RF_COMMAND (default `ls /nonexistent`); any request that already
carries a tool result answers with plain text, so the CLI ends its turn.

Usage: stub_provider.py <port-file>   (binds 127.0.0.1 on a free port and
writes the port to <port-file>). Tool name: RF_TOOL (default `bash`).
"""
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

COMMAND = os.environ.get("RF_COMMAND", "ls /nonexistent")
TOOL = os.environ.get("RF_TOOL", "bash")
MODEL = "rf-stub"


def chunk(delta, finish=None):
    return {
        "id": "rf-stub",
        "object": "chat.completion.chunk",
        "created": 0,
        "model": MODEL,
        "choices": [{"index": 0, "delta": delta, "finish_reason": finish}],
    }


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        body = json.dumps({"object": "list", "data": [{"id": MODEL, "object": "model"}]}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        req = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))) or b"{}")
        answered = any(m.get("role") == "tool" for m in req.get("messages", []))
        if answered:
            events = [chunk({"role": "assistant", "content": "DONE"}), chunk({}, "stop")]
        else:
            args = json.dumps({"command": COMMAND})
            call = {"index": 0, "id": "call_rf_1", "type": "function", "function": {"name": TOOL, "arguments": args}}
            events = [chunk({"role": "assistant", "tool_calls": [call]}), chunk({}, "tool_calls")]
        if not req.get("stream"):
            msg = {"role": "assistant", "content": "DONE"} if answered else {
                "role": "assistant", "content": None, "tool_calls": [dict(call, index=None)]}
            body = json.dumps({"id": "rf-stub", "object": "chat.completion", "model": MODEL,
                               "choices": [{"index": 0, "message": msg,
                                            "finish_reason": "stop" if answered else "tool_calls"}],
                               "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        for ev in events:
            self.wfile.write(b"data: " + json.dumps(ev).encode() + b"\n\n")
        usage = chunk({})
        usage["choices"] = []
        usage["usage"] = {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
        self.wfile.write(b"data: " + json.dumps(usage).encode() + b"\n\n")
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()


def main():
    server = HTTPServer(("127.0.0.1", 0), Handler)
    with open(sys.argv[1], "w") as f:
        f.write(str(server.server_address[1]))
    server.serve_forever()


if __name__ == "__main__":
    main()
