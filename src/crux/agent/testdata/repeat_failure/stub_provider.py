#!/usr/bin/env python3
"""Stub OpenAI-compatible chat-completions provider for the repeat-failure
harnesses. The first request in a conversation answers with one shell tool
call running RF_COMMAND (default `ls /nonexistent`); any request that already
carries a tool result answers with plain text, so the CLI ends its turn.

Usage: stub_provider.py <port-file>   (binds 127.0.0.1 on a free port and
writes the port to <port-file>). Tool name: RF_TOOL (default `bash`).
RF_EXTRA_ARGS, a JSON object, adds arguments to the tool call next to
`command` (a CLI whose shell tool requires more fields).

A POST to a path ending in /responses is answered in the OpenAI Responses
streaming format instead (Muse Code); a request that does not offer the tool
(a background helper agent) answers with plain text.
"""
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

COMMAND = os.environ.get("RF_COMMAND", "ls /nonexistent")
TOOL = os.environ.get("RF_TOOL", "bash")
EXTRA_ARGS = json.loads(os.environ.get("RF_EXTRA_ARGS") or "{}")
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
        if self.path.rstrip("/").endswith("/responses"):
            self.responses_api(req)
            return
        answered = any(m.get("role") == "tool" for m in req.get("messages", []))
        if answered:
            events = [chunk({"role": "assistant", "content": "DONE"}), chunk({}, "stop")]
        else:
            args = json.dumps(dict(EXTRA_ARGS, command=COMMAND))
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

    def responses_api(self, req):
        offered = ('"name": "%s"' % TOOL) in json.dumps(req.get("tools", []))
        answered = not offered or any(
            isinstance(i, dict) and i.get("type") == "function_call_output" for i in req.get("input", []))
        if answered:
            item = {"type": "message", "id": "msg_rf_1", "role": "assistant", "status": "completed",
                    "content": [{"type": "output_text", "text": "DONE", "annotations": []}]}
        else:
            item = {"type": "function_call", "id": "fc_rf_1", "call_id": "call_rf_1", "name": TOOL,
                    "arguments": json.dumps(dict(EXTRA_ARGS, command=COMMAND)), "status": "completed"}
        resp = {"id": "resp_rf", "object": "response", "created_at": 0, "model": MODEL, "status": "completed",
                "output": [item], "usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}
        events = [
            {"type": "response.created", "response": dict(resp, status="in_progress", output=[])},
            {"type": "response.output_item.added", "output_index": 0, "item": item},
            {"type": "response.output_item.done", "output_index": 0, "item": item},
            {"type": "response.completed", "response": resp},
        ]
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        for seq, ev in enumerate(events):
            ev["sequence_number"] = seq
            self.wfile.write(b"event: " + ev["type"].encode() + b"\ndata: " + json.dumps(ev).encode() + b"\n\n")
        self.wfile.flush()


def main():
    server = HTTPServer(("127.0.0.1", 0), Handler)
    with open(sys.argv[1], "w") as f:
        f.write(str(server.server_address[1]))
    server.serve_forever()


if __name__ == "__main__":
    main()
