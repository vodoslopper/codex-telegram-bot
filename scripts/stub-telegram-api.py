#!/usr/bin/env python3
"""A stub Telegram Bot API, for smoke-testing the compiled binary end to end.

It is deliberately dumb: it answers the methods the bot calls, serves one
scripted update on the first getUpdates, and writes every sendMessage it receives
to a file so the caller can assert on what a user would have seen.

This exists because `go test` exercises the bot against an in-memory fake, which
never touches main.go: the wiring, the startup probes, the lock file, the signal
handling. Running the real binary against this stub does.
"""
import json
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

SENT = os.environ.get("STUB_SENT_FILE", "/tmp/stub-sent.jsonl")
UPDATE_TEXT = os.environ.get("STUB_UPDATE_TEXT", "Summarize this repository")
USER_ID = int(os.environ.get("STUB_USER_ID", "111"))
CHAT_ID = int(os.environ.get("STUB_CHAT_ID", "111"))
UPDATE_ID = int(os.environ.get("STUB_UPDATE_ID", "1001"))

served = {"updates": 0}
lock = threading.Lock()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def _send(self, payload, status=200):
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b"{}"
        try:
            body = json.loads(raw or b"{}")
        except json.JSONDecodeError:
            body = {}
        method = self.path.rsplit("/", 1)[-1]

        if method == "getMe":
            self._send({"ok": True, "result": {
                "id": 1, "is_bot": True, "first_name": "Stub", "username": "stub_bot"}})
        elif method == "getWebhookInfo":
            self._send({"ok": True, "result": {
                "url": "https://example.invalid/old-hook", "pending_update_count": 2}})
        elif method == "deleteWebhook":
            self._send({"ok": True, "result": True})
        elif method == "getUpdates":
            with lock:
                served["updates"] += 1
                first = served["updates"] == 1
            if first:
                self._send({"ok": True, "result": [{
                    "update_id": UPDATE_ID,
                    "message": {
                        "message_id": UPDATE_ID,
                        "date": int(time.time()),
                        "text": UPDATE_TEXT,
                        "from": {"id": USER_ID, "is_bot": False, "first_name": "Tester"},
                        "chat": {"id": CHAT_ID, "type": "private"},
                    },
                }]})
            else:
                # A real long poll would block here; sleeping keeps the bot from
                # spinning while the smoke test watches it work.
                time.sleep(1.0)
                self._send({"ok": True, "result": []})
        elif method in ("setMyCommands", "answerCallbackQuery"):
            self._send({"ok": True, "result": True})
        elif method in ("sendMessage", "sendChatAction"):
            if method == "sendMessage":
                with lock, open(SENT, "a", encoding="utf-8") as fh:
                    fh.write(json.dumps(body, ensure_ascii=False) + "\n")
            self._send({"ok": True, "result": {"message_id": 1, "date": int(time.time())}})
        else:
            self._send({"ok": False, "error_code": 404,
                        "description": f"stub does not implement {method}"}, 404)

    do_GET = do_POST


def main():
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 0
    server = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    print(server.server_address[1], flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
