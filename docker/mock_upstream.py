#!/usr/bin/env python3
"""Throwaway mock upstream for the docker delivery tests (DOCKER-003).

Serves just enough of the aistudio + account.xiaomi.com wire so the real
mimo2api binary — running in a container with `account.xiaomi.com` mapped to
this host via `--add-host` — performs exactly one STS session renewal and
persists runtime state back to /data/accounts.json.

FAKE values only. This file is a test support asset, never part of the image
(docker/ is in .dockerignore).
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 18099
HOST = "account.xiaomi.com:%d" % PORT
TOKEN = "FAKE-session-token-ccc000"


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *a):  # silence per-request logging
        pass

    def _send(self, code, body=b"", ctype="text/plain"):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        path = self.path.split("?", 1)[0]
        if path == "/open-apis/user/mi/get":
            # 401 + loginUrl: the renewal probe reads loginUrl from the body.
            login = (
                "http://%s/pass/serviceLogin?callback=cb&sid=xiaomichatbot"
                "&qs=%%3Fsid%%3Dxiaomichatbot&_sign=FAKESIGN"
                "&serviceParam=sp&_group=DEFAULT" % HOST
            )
            self._send(401, json.dumps({"loginUrl": login}).encode(), "application/json")
        elif path == "/pass/serviceLogin":
            # XSSI envelope: code 0 + STS location to follow.
            body = ('&&&START&&&{"code":0,"location":"http://%s/sts?sign=FAKE"}' % HOST).encode()
            self._send(200, body)
        elif path == "/sts":
            # Final hop plants the session cookies (parsed by collectCookies).
            cookies = [
                'xiaomichatbot_serviceToken="%s"' % TOKEN,
                'xiaomichatbot_ph="FAKE-ph-999"',
                'xiaomichatbot_slh="FAKE-slh-111"',
                'userId=88888888',
                'cUserId=FAKE-cuser-000',
            ]
            self.send_response(200)
            self.send_header("Content-Type", "text/html")
            for c in cookies:
                self.send_header("Set-Cookie", c + "; Path=/")
            self.send_header("Content-Length", "2")
            self.end_headers()
            self.wfile.write(b"ok")
        else:
            self._send(404, b"not found")


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
