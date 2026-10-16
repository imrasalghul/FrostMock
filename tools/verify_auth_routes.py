"""Exercise auth routes through a real, isolated Bifrost and native plugin.

Usage: python3 tools/verify_auth_routes.py --binary /path/to/bifrost --plugin /path/to/chatgpt.so
Uses temporary credentials/configuration and a local fake OAuth issuer only.
"""
import argparse
import http.server
import json
import os
from pathlib import Path
import secrets
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request


class OAuth(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length", "0")))
        if self.path == "/api/accounts/deviceauth/usercode":
            self.send_response(200)
            self.end_headers()
            self.wfile.write(json.dumps({"device_auth_id": "fixture", "user_code": "TEST-CODE", "interval": 60}).encode())
        else:
            self.send_response(403)
            self.end_headers()
            self.wfile.write(b'{}')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True)
    parser.add_argument("--plugin", required=True)
    parser.add_argument("--port", type=int, default=18082)
    args = parser.parse_args()
    binary, plugin = Path(args.binary).resolve(), Path(args.plugin).resolve()
    token = secrets.token_urlsafe(32)
    issuer = http.server.ThreadingHTTPServer(("127.0.0.1", 0), OAuth)
    thread = threading.Thread(target=issuer.serve_forever, daemon=True)
    thread.start()
    try:
        with tempfile.TemporaryDirectory(prefix="chatgpt-auth-routes-") as directory:
            root = Path(directory)
            config = {"setup_token": token, "plugins": [{"name": "chatgpt", "enabled": True, "path": str(plugin), "placement": "post_builtin", "config": {"model_sync": False}}]}
            (root / "config.json").write_text(json.dumps(config))
            (root / "config.json").chmod(0o600)
            env = {k: v for k, v in os.environ.items() if not k.startswith(("PG", "BIFROST", "CHATGPT_LOCAL_", "CODEX_"))}
            env.update(HOME=str(root / "home"), CHATGPT_LOCAL_HOME=str(root / "auth"), CODEX_HOME=str(root / "codex"), CHATGPT_LOCAL_ISSUER="http://127.0.0.1:" + str(issuer.server_port))

            def request(method, path, headers=None):
                req = urllib.request.Request("http://127.0.0.1:" + str(args.port) + path, method=method, headers=headers or {})
                try:
                    response = urllib.request.urlopen(req, timeout=5)
                except urllib.error.HTTPError as error:
                    response = error
                with response:
                    body = response.read()
                    content_type = response.headers.get("Content-Type", "")
                    return response.status, content_type, json.loads(body) if "application/json" in content_type else None

            with (root / "gateway.log").open("w") as log:
                process = subprocess.Popen([str(binary), "-app-dir", str(root), "-host", "127.0.0.1", "-port", str(args.port)], stdout=log, stderr=subprocess.STDOUT, env=env)
                try:
                    for _ in range(90):
                        assert process.poll() is None, "Isolated Bifrost exited during startup"
                        try:
                            if request("GET", "/health")[0] == 200:
                                break
                        except OSError:
                            pass
                        time.sleep(.5)
                    else:
                        raise AssertionError("Isolated Bifrost did not start")

                    auth = {"X-Bifrost-Setup-Token": token}
                    # The health route can become ready before custom plugins
                    # finish loading. Wait for the authenticated plugin route,
                    # so startup timing cannot masquerade as a loader failure.
                    for _ in range(60):
                        assert process.poll() is None, "Isolated Bifrost exited while loading the plugin"
                        status, kind, body = request("GET", "/chatgpt/status", auth)
                        if status == 200:
                            break
                        assert status in (404, 503), ("Plugin readiness failed", status, kind, body)
                        time.sleep(.5)
                    else:
                        raise AssertionError(("Plugin did not become ready", status, kind, body))

                    for path in ("/chatgpt/status", "/chatgpt/login", "/chatgpt/refresh", "/chatgpt/logout"):
                        for headers in ({}, {"x-bf-vk": "sk-bf-not-an-admin"}):
                            status, kind, _ = request("GET", path, headers)
                            assert status == 401 and "application/json" in kind, (path, "must require management authentication", status, kind)
                        status, _, _ = request("GET", path, {"X-Bifrost-Setup-Token": "incorrect"})
                        assert status == 403, (path, "invalid setup token accepted", status)

                    status, kind, body = request("GET", "/chatgpt/status", auth)
                    assert status == 200 and "application/json" in kind and body["logged_in"] is False, (status, kind, body)
                    for path in ("/chatgpt/login", "/chatgpt/refresh", "/chatgpt/logout"):
                        assert request("GET", path, auth)[0] == 405, (path, "GET must not change credentials")
                    assert request("POST", "/chatgpt/status", auth)[0] == 405
                    assert request("POST", "/chatgpt/refresh", auth)[0] == 401

                    status, _, body = request("POST", "/chatgpt/login", auth)
                    assert status == 200 and body["user_code"] == "TEST-CODE" and body["status"] == "pending", (status, body)
                    assert request("GET", "/chatgpt/status", auth)[2]["login"]["status"] == "pending"
                    status, _, body = request("POST", "/chatgpt/logout", auth)
                    assert status == 200 and body["status"] == "logged_out", (status, body)
                    body = request("GET", "/chatgpt/status", auth)[2]
                    assert body["logged_in"] is False and "login" not in body
                    assert not (root / "auth" / "auth.json").exists()
                    print("Auth routes passed: management authentication, JSON status, method guards, device login, refresh and logout through real Bifrost")
                finally:
                    process.terminate()
                    try:
                        process.wait(timeout=15)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait()
    finally:
        issuer.shutdown()
        issuer.server_close()
        thread.join(timeout=5)


if __name__ == "__main__":
    main()
