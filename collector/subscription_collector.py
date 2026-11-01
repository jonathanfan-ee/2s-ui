#!/usr/bin/env python3
"""One-shot fixed-IPv4 collector for 2S-UI and the MiSub wire protocol.

Adapted from MiSub, copyright (c) 2024 MiSub contributors, MIT.
See LICENSES/MiSub.txt. Python 3.9+, standard library only.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
import email.utils
import fcntl
import hashlib
import hmac
import http.client
import ipaddress
import json
import os
from pathlib import Path
import queue
import re
import socket
import ssl
import sys
import threading
import time
from urllib.parse import urljoin, urlsplit
import uuid

MAX_BYTES = 512 * 1024
UA = {"clash": "ClashMeta/1.19.32", "universal": "SubscriptionFetcher/1.0"}


class CollectorError(Exception):
    """Only fixed codes, never URLs, response bodies or credentials, reach logs."""


def remaining(deadline):
    seconds = deadline - time.monotonic()
    if seconds <= 0:
        raise CollectorError("timeout")
    return seconds


def validate_url(url):
    parts = urlsplit(url)
    if (parts.scheme != "https" or not parts.hostname or parts.username is not None
            or parts.password is not None or parts.fragment or parts.port not in (None, 443)):
        raise CollectorError("unsafe-target")
    return parts


def resolve_ipv4(host, deadline):
    # Bound DNS as part of the same deadline. A stuck resolver cannot hold up exit.
    result = queue.Queue(maxsize=1)

    def resolve():
        try:
            result.put(socket.getaddrinfo(host, 443, socket.AF_INET, socket.SOCK_STREAM))
        except Exception:
            result.put(None)

    threading.Thread(target=resolve, daemon=True).start()
    try:
        addresses = result.get(timeout=remaining(deadline))
    except queue.Empty:
        raise CollectorError("timeout") from None
    if not addresses:
        raise CollectorError("fetch-failed")
    ips = list(dict.fromkeys(item[4][0] for item in addresses))
    if not all(ipaddress.ip_address(ip).is_global for ip in ips):
        raise CollectorError("unsafe-target")
    return ips


class IPv4HTTPSConnection(http.client.HTTPSConnection):
    def __init__(self, host, deadline):
        self.deadline = deadline
        super().__init__(host, 443, timeout=remaining(deadline), context=ssl.create_default_context())

    def connect(self):
        last_error = None
        for ip in resolve_ipv4(self.host, self.deadline):
            sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            try:
                sock.settimeout(remaining(self.deadline))
                sock.connect((ip, 443))
                sock.settimeout(remaining(self.deadline))
                # Pin the resolved address, retain the ORIGINAL TLS verification name.
                self.sock = self._context.wrap_socket(sock, server_hostname=self.host)
                return
            except Exception as error:
                sock.close()
                last_error = error
        if isinstance(last_error, (TimeoutError, socket.timeout)):
            raise CollectorError("timeout")
        raise CollectorError("fetch-failed")


class Transport:
    """Direct IPv4 TLS; intentionally independent of HTTP(S)_PROXY environment."""
    def request(self, url, method="GET", headers=None, body=None, timeout=10, limit=MAX_BYTES):
        parts = validate_url(url)
        deadline = time.monotonic() + timeout
        connection = IPv4HTTPSConnection(parts.hostname, deadline)
        response = None
        timer = None
        expired = threading.Event()
        try:
            connection.connect()
            channel = connection.sock
            channel.settimeout(remaining(deadline))

            def abort():
                expired.set()
                try:
                    channel.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass

            # getresponse() can make progress byte by byte forever. A socket read
            # timeout alone does not impose an absolute header/body deadline.
            timer = threading.Timer(remaining(deadline), abort)
            timer.daemon = True
            timer.start()
            path = parts.path or "/"
            if parts.query:
                path += "?" + parts.query
            connection.request(method, path, body=body, headers=headers or {})
            channel.settimeout(remaining(deadline))
            response = connection.getresponse()
            declared_length = None
            if response.getheader("Content-Length") is not None and not response.getheader("Transfer-Encoding"):
                try:
                    declared_length = int(response.getheader("Content-Length"))
                except ValueError:
                    raise CollectorError("invalid-response") from None
                if declared_length < 0:
                    raise CollectorError("invalid-response")
                if declared_length > limit:
                    raise CollectorError("response-too-large")
            data = bytearray()
            while True:
                channel.settimeout(remaining(deadline))
                chunk = response.read1(min(65536, limit + 1 - len(data)))
                if not chunk:
                    break
                data.extend(chunk)
                if len(data) > limit:
                    raise CollectorError("response-too-large")
            remaining(deadline)
            if declared_length is not None and len(data) != declared_length:
                raise CollectorError("invalid-response")
            return response.status, {key.lower(): value for key, value in response.getheaders()}, bytes(data)
        except CollectorError:
            raise
        except (TimeoutError, socket.timeout):
            raise CollectorError("timeout") from None
        except Exception:
            if expired.is_set() or time.monotonic() >= deadline:
                raise CollectorError("timeout") from None
            raise CollectorError("fetch-failed") from None
        finally:
            if timer is not None:
                timer.cancel()
            if response is not None:
                response.close()
            connection.close()


class RateLimit:
    """Persist attempts BEFORE sending; four upstream requests per rolling minute."""
    def __init__(self, path, clock=time.time):
        self.path, self.clock, self.lock = Path(path), clock, threading.Lock()

    def take(self):
        with self.lock:
            now = self.clock()
            if self.path.exists():
                try:
                    timestamps = json.loads(self.path.read_text())
                    if not isinstance(timestamps, list) or not all(isinstance(t, (int, float)) for t in timestamps):
                        raise ValueError()
                except Exception:
                    # Corrupt state must not silently reset an airport's rate budget.
                    raise CollectorError("rate-limited") from None
            else:
                timestamps = []
            timestamps = [t for t in timestamps if t > now - 60]
            if len(timestamps) >= 4:
                raise CollectorError("rate-limited")
            timestamps.append(now)
            temporary = self.path.with_suffix(".tmp")
            with open(temporary, "w", encoding="utf-8") as file:
                os.chmod(temporary, 0o600)
                json.dump(timestamps, file)
                file.flush()
                os.fsync(file.fileno())
            os.replace(temporary, self.path)


def retry_seconds(header):
    try:
        seconds = int(header)
    except (TypeError, ValueError):
        try:
            seconds = int(email.utils.parsedate_to_datetime(header).timestamp() - time.time())
        except Exception:
            return 0
    return min(7 * 86400, max(0, seconds))


def fetch_format(url, format_name, transport, limiter, timeout=10):
    deadline = time.monotonic() + min(10, timeout)
    try:
        for redirects in range(4):
            validate_url(url)
            limiter.take()
            status, headers, raw = transport.request(url, headers={"User-Agent": UA[format_name], "Accept-Encoding": "identity"},
                                                     timeout=remaining(deadline))
            if status in (301, 302, 303, 307, 308):
                if redirects == 3 or not headers.get("location"):
                    raise CollectorError("invalid-response")
                url = urljoin(url, headers["location"])
                continue
            if status in (401, 403):
                return {"failure": "provider-blocked"}
            if status == 429:
                return {"failure": "http-429", "retryAfterSeconds": retry_seconds(headers.get("retry-after"))}
            if not 200 <= status < 300:
                return {"failure": "http-" + str(status)}
            if headers.get("content-encoding", "identity").lower() not in ("", "identity"):
                raise CollectorError("invalid-response")
            text = raw.decode("utf-8-sig")
            if any(marker in text[:8192] for marker in ("订阅更新已被暂停", "因24小时内不同IP超限已被封禁", "subscription updates suspended")):
                return {"failure": "provider-blocked"}
            info = {}
            for part in headers.get("subscription-userinfo", "").split(";"):
                key, _, value = part.strip().partition("=")
                if key in ("upload", "download", "total", "expire") and value.isdigit():
                    info[key] = int(value)
            return {"text": text, "userInfo": info or None}
        raise CollectorError("invalid-response")
    except CollectorError as error:
        return {"failure": str(error)}
    except Exception:
        return {"failure": "invalid-response"}


def signing_headers(config, path, body, timestamp=None, nonce=None):
    timestamp = str(timestamp if timestamp is not None else int(time.time() * 1000))
    nonce = nonce or uuid.uuid4().hex
    canonical = "POST\n" + path + "\n" + config["id"] + "\n" + timestamp + "\n" + nonce + "\n" + hashlib.sha256(body).hexdigest()
    signature = hmac.new(config["secret"].encode(), canonical.encode(), hashlib.sha256).hexdigest()
    return {"Content-Type": "application/json", "Accept-Encoding": "identity", "X-Collector-Id": config["id"],
            "X-Collector-Timestamp": timestamp, "X-Collector-Nonce": nonce, "X-Collector-Signature": signature}


def api(config, path, value, transport):
    body = json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode()
    base = config.get("panelUrl") or config.get("misubUrl")
    url = base.rstrip("/") + path
    status, _, response = transport.request(url, "POST", signing_headers(config, urlsplit(url).path, body), body,
                                            timeout=5, limit=128 * 1024)
    if status != 200:
        raise CollectorError("misub-http-" + str(status))
    try:
        return json.loads(response)
    except Exception:
        raise CollectorError("invalid-misub-response") from None


def run_once(config, state_dir, transport=None):
    transport = transport or Transport()
    state_dir = Path(state_dir)
    state_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    with open(state_dir / "run.lock", "a") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return {"status": "already-running"}
        limiter = RateLimit(state_dir / "rate.json")
        result = api(config, "/api/collector/tasks", {}, transport)
        tasks = result.get("tasks")
        if not isinstance(tasks, list) or len(tasks) > 1:
            raise CollectorError("invalid-misub-response")
        if not tasks:
            return {"status": "idle"}
        task = tasks[0]
        if (not isinstance(task, dict) or task.get("formats") != ["clash", "universal"]
                or not all(isinstance(task.get(key), str) for key in ("sourceId", "url", "urlHash", "leaseToken"))
                or not isinstance(task.get("leaseUntil"), (int, float)) or task["leaseUntil"] <= 0):
            raise CollectorError("invalid-misub-response")
        # MiSub validates the lease at publication. Comparing its epoch deadline
        # to the VM clock would incorrectly reject the allowed HMAC clock skew.
        with ThreadPoolExecutor(max_workers=2) as pool:
            futures = {format_name: pool.submit(fetch_format, task["url"], format_name, transport, limiter) for format_name in UA}
            snapshots = {format_name: future.result() for format_name, future in futures.items()}
        payload = {key: task[key] for key in ("sourceId", "urlHash", "leaseToken")}
        payload["snapshots"] = snapshots
        uploaded = api(config, "/api/collector/results", payload, transport)
        return {"status": "paused" if uploaded.get("paused") else "uploaded"}


def load_config(path):
    file = Path(path)
    if file.stat().st_mode & 0o077:
        raise CollectorError("config-permissions-must-be-0600")
    config = json.loads(file.read_text())
    base = config.get("panelUrl") or config.get("misubUrl", "")
    parts = validate_url(base)
    if (parts.query or "%" in parts.path or any(part in (".", "..") for part in parts.path.split("/"))
            or (config.get("panelUrl") and config.get("misubUrl") and config["panelUrl"] != config["misubUrl"])
            or not re.fullmatch(r"[A-Za-z0-9_-]{1,64}", config.get("id", ""))):
        raise CollectorError("invalid-config")
    if not isinstance(config.get("secret"), str) or len(config["secret"]) < 32 or config["secret"].startswith("REPLACE_"):
        raise CollectorError("invalid-config")
    return config


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", default="/etc/subscription-collector/config.json")
    parser.add_argument("--state-dir", default="/var/lib/subscription-collector")
    parser.add_argument("--check", action="store_true", help="validate local configuration without any network request")
    args = parser.parse_args()
    try:
        config = load_config(args.config)
        result = {"status": "config-ok"} if args.check else run_once(config, args.state_dir)
        print(json.dumps(result))
        return 0
    except CollectorError as error:
        print(json.dumps({"error": str(error)}), file=sys.stderr)
        return 1
    except Exception:
        print('{"error":"collector-failed"}', file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
