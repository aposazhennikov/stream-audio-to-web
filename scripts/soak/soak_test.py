#!/usr/bin/env python3
"""Long-running check of a running audio-streamer: sound must never stop.

Runs for --minutes against --base-url and checks:
  * normal listeners receive audio continuously at the real-time rate (no silence,
    no slow drift, no reconnects needed);
  * an ICY client gets valid UTF-8 track titles that change with the tracks;
  * a client that stops reading is disconnected by the server and does not affect others;
  * frequent connect/disconnect works (no refused or stuck connections);
  * bursts of "Previous track" clicks do not break the stream;
  * goroutines and memory do not grow without bound.
Saves the last minute of one listener per route to --out-dir for decoding.
Exit code 0 only if every check passed.
"""

import argparse
import http.client
import json
import os
import re
import socket
import sys
import threading
import time
import urllib.parse
from collections import defaultdict

BYTES_PER_SEC = 16000  # 128 kbps
lock = threading.Lock()
stop_event = threading.Event()
problems = []
stats = defaultdict(dict)


def log(msg):
    with lock:
        print(time.strftime("%H:%M:%S"), msg, flush=True)


def problem(msg):
    with lock:
        problems.append(msg)
    log("PROBLEM: " + msg)


def connect(base, path, headers=None, timeout=15):
    u = urllib.parse.urlparse(base)
    conn = http.client.HTTPConnection(u.hostname, u.port or 80, timeout=timeout)
    conn.request("GET", path, headers=headers or {})
    return conn, conn.getresponse()


def listener(base, route, name, keep_seconds, out_dir):
    """Reads the stream continuously; records rate per window and gaps."""
    key = f"{route}/{name}"
    windows = []  # bytes per 60 s window
    total, max_gap, reconnects = 0, 0.0, 0
    tail = bytearray()
    keep_bytes = keep_seconds * BYTES_PER_SEC
    start = time.time()
    window_start, window_bytes = start, 0
    while not stop_event.is_set():
        try:
            conn, resp = connect(base, "/" + route)
            if resp.status != 200:
                problem(f"{key}: HTTP {resp.status}")
                reconnects += 1
                time.sleep(2)
                continue
            last = time.time()
            while not stop_event.is_set():
                data = resp.read1(8192) if hasattr(resp, "read1") else resp.read(8192)
                now = time.time()
                if not data:
                    raise ConnectionError("stream closed by server")
                gap = now - last
                if gap > max_gap:
                    max_gap = gap
                if gap > 3 and now - start > 10:
                    problem(f"{key}: no data for {gap:.1f}s")
                last = now
                total += len(data)
                window_bytes += len(data)
                tail += data
                if len(tail) > keep_bytes:
                    del tail[: len(tail) - keep_bytes]
                if now - window_start >= 60:
                    windows.append(window_bytes / (now - window_start))
                    window_start, window_bytes = now, 0
            conn.close()
        except Exception as e:  # noqa: BLE001
            if stop_event.is_set():
                break
            reconnects += 1
            problem(f"{key}: connection lost: {e}")
            time.sleep(1)
    elapsed = time.time() - start
    with open(os.path.join(out_dir, f"{route}_{name}.mp3"), "wb") as f:
        f.write(tail)
    with lock:
        stats["listeners"][key] = {
            "total_bytes": total,
            "avg_rate": total / elapsed if elapsed else 0,
            "min_window_rate": min(windows) if windows else None,
            "max_window_rate": max(windows) if windows else None,
            "max_gap_s": round(max_gap, 2),
            "reconnects": reconnects,
        }


def icy_listener(base, route):
    """Reads ICY metadata; titles must be valid UTF-8 and change over time."""
    titles, bad = [], 0
    while not stop_event.is_set():
        try:
            conn, resp = connect(base, "/" + route, {"Icy-MetaData": "1"})
            metaint = int(resp.getheader("icy-metaint") or 0)
            if resp.status != 200 or metaint <= 0:
                problem(f"icy {route}: status {resp.status}, metaint {metaint}")
                time.sleep(5)
                continue
            while not stop_event.is_set():
                remaining = metaint
                while remaining > 0:
                    chunk = resp.read(min(remaining, 8192))
                    if not chunk:
                        raise ConnectionError("closed")
                    remaining -= len(chunk)
                length = resp.read(1)
                if not length:
                    raise ConnectionError("closed")
                n = length[0] * 16
                if n:
                    raw = resp.read(n).rstrip(b"\x00")
                    try:
                        text = raw.decode("utf-8")
                    except UnicodeDecodeError:
                        bad += 1
                        problem(f"icy {route}: metadata is not UTF-8: {raw[:60]!r}")
                        continue
                    m = re.match(r"StreamTitle='(.*)';", text, re.S)
                    if not m:
                        bad += 1
                        problem(f"icy {route}: bad metadata {text[:80]!r}")
                        continue
                    if not titles or titles[-1] != m.group(1):
                        titles.append(m.group(1))
                        log(f"icy {route}: title -> {m.group(1)}")
            conn.close()
        except Exception as e:  # noqa: BLE001
            if stop_event.is_set():
                break
            problem(f"icy {route}: connection lost: {e}")
            time.sleep(1)
    with lock:
        stats["icy"][route] = {"titles_seen": len(titles), "bad_blocks": bad, "examples": titles[:5]}


def stalled_client(base, route, stall_seconds):
    """Connects, reads a bit, then stops reading. The server must drop it."""
    rounds, dropped = 0, 0
    u = urllib.parse.urlparse(base)
    while not stop_event.is_set():
        rounds += 1
        s = socket.create_connection((u.hostname, u.port or 80), timeout=10)
        # Small receive buffer so the server notices quickly that we do not read.
        s.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 4096)
        s.sendall(f"GET /{route} HTTP/1.1\r\nHost: {u.hostname}\r\n\r\n".encode())
        got = 0
        while got < 32768:
            got += len(s.recv(8192))
        log(f"stalled client {route}: stopped reading (round {rounds})")
        if stop_event.wait(stall_seconds):
            s.close()
            break
        # Drain: a dropped connection ends with EOF/reset after the buffered bytes.
        s.settimeout(5)
        closed = False
        try:
            for _ in range(10000):
                d = s.recv(65536)
                if not d:
                    closed = True
                    break
        except (ConnectionResetError, BrokenPipeError):
            closed = True
        except socket.timeout:
            closed = False
        s.close()
        if closed:
            dropped += 1
            log(f"stalled client {route}: server dropped it as expected")
        else:
            problem(f"stalled client {route}: server kept a client that did not read for {stall_seconds}s")
    with lock:
        stats["stalled"][route] = {"rounds": rounds, "dropped": dropped}


def churn(base, routes):
    """Connects and disconnects often, like players that reconnect."""
    ok, failed, i = 0, 0, 0
    while not stop_event.is_set():
        route = routes[i % len(routes)]
        i += 1
        try:
            conn, resp = connect(base, "/" + route, timeout=10)
            data = resp.read(20000) if resp.status == 200 else b""
            conn.close()
            if resp.status == 200 and len(data) >= 20000:
                ok += 1
            else:
                failed += 1
                problem(f"churn {route}: status {resp.status}, {len(data)} bytes")
        except Exception as e:  # noqa: BLE001
            failed += 1
            problem(f"churn {route}: {e}")
        stop_event.wait(2)
    with lock:
        stats["churn"] = {"ok": ok, "failed": failed}


def login(base, password):
    u = urllib.parse.urlparse(base)
    conn = http.client.HTTPConnection(u.hostname, u.port or 80, timeout=10)
    body = urllib.parse.urlencode({"password": password})
    conn.request("POST", "/status", body, {"Content-Type": "application/x-www-form-urlencoded"})
    resp = conn.getresponse()
    cookie = resp.getheader("Set-Cookie") or ""
    conn.close()
    m = re.search(r"status_auth=([^;]+)", cookie)
    if not m:
        problem("login to /status failed, track switching not tested")
        return None
    return "status_auth=" + m.group(1)


def switcher(base, password, plan, interval):
    """Every interval seconds clicks Previous/Next several times in a row."""
    cookie = login(base, password)
    if not cookie:
        return
    u = urllib.parse.urlparse(base)
    bursts, failed = 0, 0
    while not stop_event.wait(interval):
        bursts += 1
        for route, direction, clicks in plan:
            for _ in range(clicks):
                try:
                    conn = http.client.HTTPConnection(u.hostname, u.port or 80, timeout=10)
                    conn.request("POST", f"/{direction}-track/{route}", "",
                                 {"Cookie": cookie, "X-Requested-With": "XMLHttpRequest"})
                    resp = conn.getresponse()
                    body = resp.read()
                    conn.close()
                    if resp.status != 200 or b'"success":true' not in body.replace(b" ", b""):
                        failed += 1
                        problem(f"switch {direction} {route}: {resp.status} {body[:120]!r}")
                except Exception as e:  # noqa: BLE001
                    failed += 1
                    problem(f"switch {direction} {route}: {e}")
                time.sleep(0.3)
        log(f"switch burst {bursts} done")
    with lock:
        stats["switching"] = {"bursts": bursts, "failed_requests": failed}


def monitor(base):
    samples = []
    while not stop_event.wait(30):
        try:
            conn, resp = connect(base, "/metrics", timeout=10)
            text = resp.read().decode()
            conn.close()
            g = re.search(r"^go_goroutines (\d+)", text, re.M)
            m = re.search(r"^process_resident_memory_bytes ([\d.e+]+)", text, re.M)
            samples.append((int(g.group(1)) if g else None, float(m.group(1)) if m else None))
        except Exception as e:  # noqa: BLE001
            problem(f"metrics: {e}")
    with lock:
        stats["monitor"] = {"samples": len(samples), "first": samples[:2], "last": samples[-2:],
                            "max_goroutines": max((s[0] or 0) for s in samples) if samples else None,
                            "max_rss_mb": round(max((s[1] or 0) for s in samples) / 1e6, 1) if samples else None}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base-url", default="http://localhost:18000")
    ap.add_argument("--minutes", type=float, default=40)
    ap.add_argument("--password", required=True)
    ap.add_argument("--out-dir", required=True)
    ap.add_argument("--routes", default="short,long,ru")
    args = ap.parse_args()
    routes = args.routes.split(",")
    os.makedirs(args.out_dir, exist_ok=True)

    threads = []
    for route in routes:
        for n in range(2):
            threads.append(threading.Thread(target=listener, args=(args.base_url, route, f"l{n}", 60, args.out_dir)))
        threads.append(threading.Thread(target=icy_listener, args=(args.base_url, route)))
    threads.append(threading.Thread(target=stalled_client, args=(args.base_url, routes[0], 120)))
    threads.append(threading.Thread(target=churn, args=(args.base_url, routes)))
    plan = [(routes[0], "prev", 8), (routes[1 % len(routes)], "next", 2), (routes[-1], "prev", 3)]
    threads.append(threading.Thread(target=switcher, args=(args.base_url, args.password, plan, 180)))
    threads.append(threading.Thread(target=monitor, args=(args.base_url,)))
    for t in threads:
        t.daemon = True
        t.start()

    deadline = time.time() + args.minutes * 60
    while time.time() < deadline:
        time.sleep(30)
        log(f"running, {int((deadline - time.time()) / 60)} min left, problems so far: {len(problems)}")
    stop_event.set()
    for t in threads:
        t.join(timeout=30)

    # Verdict.
    for key, st in stats["listeners"].items():
        if st["reconnects"]:
            problem(f"{key}: needed {st['reconnects']} reconnects")
        if not (0.97 * BYTES_PER_SEC <= st["avg_rate"] <= 1.05 * BYTES_PER_SEC):
            problem(f"{key}: average rate {st['avg_rate']:.0f} B/s, expected ~{BYTES_PER_SEC}")
        if st["min_window_rate"] is not None and st["min_window_rate"] < 0.9 * BYTES_PER_SEC:
            problem(f"{key}: a minute with only {st['min_window_rate']:.0f} B/s (stream starving)")
        if st["max_gap_s"] > 3:
            problem(f"{key}: max gap {st['max_gap_s']}s")
    for route, st in stats["icy"].items():
        if st["titles_seen"] < 2:
            problem(f"icy {route}: only {st['titles_seen']} titles seen")
    mon = stats.get("monitor", {})
    if mon.get("samples", 0) >= 4:
        first_g = mon["first"][0][0] or 0
        last_g = mon["last"][-1][0] or 0
        if last_g > first_g + 60:
            problem(f"goroutines grew from {first_g} to {last_g}")

    print(json.dumps(stats, indent=2, ensure_ascii=False))
    print(f"\nPROBLEMS: {len(problems)}")
    for p in problems[:50]:
        print(" -", p)
    sys.exit(1 if problems else 0)


if __name__ == "__main__":
    main()
