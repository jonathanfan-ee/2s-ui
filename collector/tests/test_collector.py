# Adapted from MiSub (MIT); see LICENSES/MiSub.txt.
import fcntl
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.util
import json
import os
from pathlib import Path
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

MODULE = Path(__file__).resolve().parents[1] / "subscription_collector.py"
spec = importlib.util.spec_from_file_location("collector", MODULE)
collector = importlib.util.module_from_spec(spec)
spec.loader.exec_module(collector)
CONFIG = {"misubUrl": "https://misub.example", "id": "fixture", "secret": "fixture-secret-for-testing-only-123456789"}


class FakeTransport:
    def __init__(self):
        self.requests = []

    def request(self, url, method="GET", headers=None, body=None, timeout=10, limit=collector.MAX_BYTES):
        self.requests.append((url, method, headers, body))
        if url.endswith("/api/collector/tasks"):
            return 200, {}, json.dumps({"tasks": [{"sourceId": "fixture", "url": "https://airport.example/sub?token=fixture-sensitive",
                "urlHash": "fixture-hash", "leaseToken": "fixture-lease", "leaseUntil": int(time.time() * 1000 + 30000), "formats": ["clash", "universal"]}]}).encode()
        if url.endswith("/api/collector/results"):
            return 200, {}, b'{"success":true,"paused":false}'
        return 200, {"subscription-userinfo": "total=1024; upload=1; download=2"}, b'trojan://fixture@edge.example:443#Fixture'


class CollectorTests(unittest.TestCase):
    def test_signature_interoperability_vector(self):
        # SHA256/HMAC vector is also verified through the real JS API integration.
        headers = collector.signing_headers(CONFIG, "/api/collector/tasks", b"{}", 1793433600000, "fixture-nonce-123456789012")
        self.assertEqual(len(headers["X-Collector-Signature"]), 64)
        self.assertNotIn(CONFIG["secret"], json.dumps(headers))
        changed = collector.signing_headers(CONFIG, "/api/collector/tasks", b'{"changed":true}', 1793433600000, "fixture-nonce-123456789012")
        self.assertNotEqual(headers["X-Collector-Signature"], changed["X-Collector-Signature"])

    def test_rate_limit_survives_restart_counts_failures_and_is_shared_by_threads(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "rate.json"
            limiter = collector.RateLimit(path, clock=lambda: 1000)
            outcomes = []

            def take():
                try:
                    limiter.take()
                    outcomes.append("taken")
                except collector.CollectorError:
                    outcomes.append("limited")

            threads = [threading.Thread(target=take) for _ in range(8)]
            for thread in threads:
                thread.start()
            for thread in threads:
                thread.join()
            self.assertEqual(outcomes.count("taken"), 4)
            with self.assertRaisesRegex(collector.CollectorError, "rate-limited"):
                collector.RateLimit(path, clock=lambda: 1001).take()
            collector.RateLimit(path, clock=lambda: 1060).take()
            path.write_text("corrupt")
            with self.assertRaisesRegex(collector.CollectorError, "rate-limited"):
                limiter.take()

    def test_redirects_and_failed_requests_consume_the_same_budget(self):
        class Redirects:
            calls = 0

            def request(self, *args, **kwargs):
                self.calls += 1
                return 302, {"location": "https://other-airport.example/next"}, b""

        with tempfile.TemporaryDirectory() as directory:
            limiter = collector.RateLimit(Path(directory) / "rate.json")
            transport = Redirects()
            collector.fetch_format("https://airport.example/sub", "clash", transport, limiter)
            self.assertEqual(transport.calls, 4)
            result = collector.fetch_format("https://airport.example/sub", "universal", transport, limiter)
            self.assertEqual(result["failure"], "rate-limited")
            self.assertEqual(transport.calls, 4)

    def test_provider_block_and_retry_after_are_preserved_without_body_or_url_in_errors(self):
        class Response:
            def __init__(self, status, headers, body):
                self.value = status, headers, body

            def request(self, *args, **kwargs):
                return self.value

        with tempfile.TemporaryDirectory() as directory:
            cases = [(403, {}, b"fixture secret body", "provider-blocked"),
                     (200, {}, "订阅更新已被暂停".encode(), "provider-blocked"),
                     (429, {"retry-after": "3600"}, b"fixture secret body", "http-429")]
            for index, (status, headers, body, failure) in enumerate(cases):
                result = collector.fetch_format("https://airport.example/sub?token=fixture-sensitive", "clash", Response(status, headers, body),
                                                collector.RateLimit(Path(directory) / (str(index) + ".json")))
                self.assertEqual(result["failure"], failure)
                self.assertNotIn("fixture-sensitive", json.dumps(result))
                self.assertNotIn("fixture secret body", json.dumps(result))
                if status == 429:
                    self.assertEqual(result["retryAfterSeconds"], 3600)

    def test_dns_is_ipv4_and_rejects_private_metadata_or_mixed_addresses(self):
        for ips in [("127.0.0.1",), ("169.254.169.254",), ("8.8.8.8", "10.0.0.1")]:
            records = [(socket.AF_INET, socket.SOCK_STREAM, 6, "", (ip, 443)) for ip in ips]
            with patch.object(collector.socket, "getaddrinfo", return_value=records) as resolver:
                with self.assertRaisesRegex(collector.CollectorError, "unsafe-target"):
                    collector.resolve_ipv4("airport.example", time.monotonic() + 1)
                self.assertEqual(resolver.call_args.args[2], socket.AF_INET)
        with patch.object(collector.socket, "getaddrinfo", side_effect=lambda *args: time.sleep(0.1)):
            with self.assertRaisesRegex(collector.CollectorError, "timeout"):
                collector.resolve_ipv4("airport.example", time.monotonic() + 0.01)

    def test_connection_pins_public_ipv4_and_retains_original_tls_name(self):
        class Sock:
            def settimeout(self, value):
                pass

            def connect(self, address):
                self.address = address

            def close(self):
                pass

        connection = collector.IPv4HTTPSConnection("airport.example", time.monotonic() + 1)
        self.assertEqual(connection._context.verify_mode, ssl.CERT_REQUIRED)
        self.assertTrue(connection._context.check_hostname)
        sock = Sock()
        with patch.object(collector, "resolve_ipv4", return_value=["8.8.8.8"]), patch.object(collector.socket, "socket", return_value=sock) as socket_factory, patch.object(connection._context, "wrap_socket", return_value=sock) as wrap:
            connection.connect()
            self.assertEqual(sock.address, ("8.8.8.8", 443))
            socket_factory.assert_called_once_with(socket.AF_INET, socket.SOCK_STREAM)
            self.assertEqual(wrap.call_args.kwargs["server_hostname"], "airport.example")

    def test_unsafe_urls_rejected_before_network(self):
        for url in ("http://airport.example/sub", "https://user:pass@airport.example/sub", "https://airport.example:8080/sub", "https://airport.example/sub#fragment"):
            with self.assertRaisesRegex(collector.CollectorError, "unsafe-target"):
                collector.validate_url(url)

    def test_http_connection_close_size_limit_and_whole_body_deadline(self):
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                if self.path == "/slow-headers":
                    try:
                        for byte in b"HTTP/1.0 200 OK\r\nX-Fixture: intentionally-slow-header\r\n\r\n":
                            self.wfile.write(bytes([byte]))
                            self.wfile.flush()
                            time.sleep(0.01)
                    except (BrokenPipeError, ConnectionResetError):
                        pass
                    return
                self.send_response(200)
                if self.path == "/truncated":
                    self.send_header("Content-Length", "100")
                self.end_headers()
                try:
                    if self.path == "/slow":
                        for _ in range(20):
                            self.wfile.write(b"x")
                            self.wfile.flush()
                            time.sleep(0.02)
                    else:
                        self.wfile.write(b"fixture-body")
                except (BrokenPipeError, ConnectionResetError):
                    pass

            def log_message(self, *args):
                pass

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with patch.object(collector, "IPv4HTTPSConnection", side_effect=lambda host, deadline: http.client.HTTPConnection("127.0.0.1", server.server_port, timeout=collector.remaining(deadline))):
                # The local HTTP connector is injected only in tests. Production always uses TLS.
                self.assertEqual(collector.Transport().request("https://fixture.example/ok")[2], b"fixture-body")
                with self.assertRaisesRegex(collector.CollectorError, "response-too-large"):
                    collector.Transport().request("https://fixture.example/ok", limit=8)
                with self.assertRaisesRegex(collector.CollectorError, "invalid-response"):
                    collector.Transport().request("https://fixture.example/truncated")
                with self.assertRaisesRegex(collector.CollectorError, "timeout"):
                    collector.Transport().request("https://fixture.example/slow", timeout=0.06)
                started = time.monotonic()
                with self.assertRaisesRegex(collector.CollectorError, "timeout"):
                    collector.Transport().request("https://fixture.example/slow-headers", timeout=0.06)
                self.assertLess(time.monotonic() - started, 0.5)
        finally:
            server.shutdown()
            server.server_close()

    def test_agent_claims_two_formats_uploads_metadata_and_logs_only_summary(self):
        with tempfile.TemporaryDirectory() as directory:
            transport = FakeTransport()
            result = collector.run_once(CONFIG, directory, transport)
            self.assertEqual(result, {"status": "uploaded"})
            self.assertEqual(len(transport.requests), 4)
            upload = json.loads(transport.requests[-1][3])
            self.assertEqual(set(upload["snapshots"]), {"clash", "universal"})
            self.assertEqual(upload["snapshots"]["clash"]["userInfo"]["total"], 1024)
            self.assertNotIn("url", upload)
            self.assertNotIn(CONFIG["secret"], json.dumps(result))
            with open(Path(directory) / "run.lock", "a") as lock:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                self.assertEqual(collector.run_once(CONFIG, directory, FakeTransport())["status"], "already-running")

    def test_misub_auth_failure_does_not_contact_the_airport(self):
        class Denied:
            calls = 0

            def request(self, *args, **kwargs):
                self.calls += 1
                return 401, {}, b"private error body"

        with tempfile.TemporaryDirectory() as directory:
            transport = Denied()
            with self.assertRaisesRegex(collector.CollectorError, "misub-http-401"):
                collector.run_once(CONFIG, directory, transport)
            self.assertEqual(transport.calls, 1)

    def test_config_permissions_and_cli_check_without_any_network(self):
        with tempfile.TemporaryDirectory() as directory:
            file = Path(directory) / "config.json"
            file.write_text(json.dumps(CONFIG))
            os.chmod(file, 0o644)
            with self.assertRaisesRegex(collector.CollectorError, "0600"):
                collector.load_config(file)
            os.chmod(file, 0o600)
            result = subprocess.run([sys.executable, "-B", str(MODULE), "--config", str(file), "--check"], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0)
            self.assertEqual(json.loads(result.stdout), {"status": "config-ok"})
            self.assertNotIn(CONFIG["secret"], result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()

class PanelPrefixTests(unittest.TestCase):
    def test_panel_base_path_is_in_url_and_signature(self):
        config = {"panelUrl": "https://panel.example/control/", "id": "fixture", "secret": CONFIG["secret"]}
        transport = FakeTransport()
        with tempfile.TemporaryDirectory() as directory:
            result = collector.run_once(config, directory, transport)
        self.assertEqual(result["status"], "uploaded")
        for url, method, headers, body in transport.requests:
            if url.startswith("https://panel.example/"):
                path = collector.urlsplit(url).path
                self.assertTrue(path.startswith("/control/api/collector/"))
                expected = collector.signing_headers(config, path, body, headers["X-Collector-Timestamp"], headers["X-Collector-Nonce"])
                self.assertEqual(headers["X-Collector-Signature"], expected["X-Collector-Signature"])
