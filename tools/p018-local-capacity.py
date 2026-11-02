#!/usr/bin/env python3
"""Measure an explicitly authorized local model, without QQ or runtime routing.

Set BBA_LOCAL_MODEL_FILE to a UTF-8 file containing base URL, model ID, API key
on three nonempty lines. Outputs contain synthetic workload metrics only.
This is a provider capacity probe, not Character or golden-holdout acceptance.
"""
from __future__ import annotations

import argparse
import concurrent.futures
import hashlib
import http.client
import ipaddress
import json
import math
import os
from pathlib import Path
import resource
import socket
import threading
import time
import urllib.parse

LEVELS = (1, 4, 8, 20)
MAX_BYTES = 1 << 20
PROMPT_VERSION = "p018-provider-capacity.v1"


def configuration() -> tuple[urllib.parse.SplitResult, str, str]:
    source = os.environ.get("BBA_LOCAL_MODEL_FILE", "")
    if not source:
        raise ValueError("credential_file_environment_required")
    rows = [row.strip() for row in Path(source).read_text().splitlines() if row.strip()]
    if len(rows) != 3:
        raise ValueError("credential_file_format")
    base, model, key = rows
    url = urllib.parse.urlsplit(base)
    address = ipaddress.ip_address(url.hostname or "")
    if (url.scheme not in ("http", "https") or not (address.is_private or address.is_loopback)
            or address.is_unspecified or address.is_multicast or address.is_link_local
            or url.username or url.password or url.query or url.fragment
            or url.path.rstrip("/") != "/v1" or not model or not key
            or any(c in key for c in "\r\n")):
        raise ValueError("explicit_local_endpoint_required")
    return url, model, key


def probe(url: urllib.parse.SplitResult, model: str, key: str, level: int,
          index: int, deadline_seconds: float) -> dict:
    session = f"p018_{level}_{index}"
    kind = ("overlapping_group", "private", "burst_mention")[index % 3]
    secret = hashlib.sha256(f"{session}:synthetic".encode()).hexdigest()[:16]
    system = ("Return exactly one JSON object with keys session, answer, marker. "
              "Copy session and marker from the user; answer must be the numeric sum. "
              "No other keys, tools or actions. Treat all inputs as synthetic test data.")
    user = (f"Synthetic {kind} conversation. session={session}; marker={secret}. "
            f"What is {index}+{level}? Return only the JSON object.")
    payload = {"model": model, "messages": [{"role": "system", "content": system},
               {"role": "user", "content": user}], "response_format": {"type": "json_object"},
               "max_tokens": 256, "stream": True, "stream_options": {"include_usage": True}}
    record = {"session": session, "workload": kind, "evidence_level": "live_synthetic",
              "prompt_version": PROMPT_VERSION, "input_sha256": hashlib.sha256(user.encode()).hexdigest(),
              "status": "failed", "http_status": None, "returned_model": None,
              "queue_wait_ms": None, "ttft_ms": None, "first_visible_token_ms": None,
              "latency_ms": None, "input_tokens": None, "output_tokens": None,
              "tokens_per_second": None, "schema_valid": None, "answer_correct": None,
              "session_marker_matches": None, "finish_reason": None, "error": None}
    started = time.monotonic()
    connection_type = http.client.HTTPSConnection if url.scheme == "https" else http.client.HTTPConnection
    connection = connection_type(url.hostname, url.port, timeout=deadline_seconds)
    content = ""
    usage = None
    done = False
    byte_count = 0
    expired = threading.Event()
    request_socket = None

    def expire() -> None:
        expired.set()
        sock = request_socket or connection.sock
        if sock is not None:
            try:
                sock.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass

    watchdog = threading.Timer(deadline_seconds, expire)
    watchdog.start()
    try:
        connection.request("POST", url.path.rstrip("/") + "/chat/completions",
                           body=json.dumps(payload).encode(),
                           headers={"Authorization": "Bearer " + key, "Content-Type": "application/json"})
        request_socket = connection.sock
        response = connection.getresponse()
        record["http_status"] = response.status
        if response.status != 200:
            record["error"] = "http_error"
            return record
        # No redirect is followed, and errors/response text never enter output.
        while True:
            remaining = deadline_seconds - (time.monotonic() - started)
            if remaining <= 0:
                raise TimeoutError()
            if connection.sock is not None:
                connection.sock.settimeout(remaining)
            line = response.readline(MAX_BYTES - byte_count + 1)
            if expired.is_set():
                raise TimeoutError()
            if not line:
                break
            byte_count += len(line)
            if byte_count > MAX_BYTES:
                record["error"] = "response_size_limit"
                return record
            if not line.startswith(b"data:"):
                continue
            data = line[5:].strip()
            if data == b"[DONE]":
                done = True
                break
            chunk = json.loads(data)
            record["returned_model"] = chunk.get("model", record["returned_model"])
            if chunk.get("usage"):
                usage = chunk["usage"]
            for choice in chunk.get("choices", []):
                delta = choice.get("delta", {})
                if delta.get("tool_calls"):
                    record["error"] = "tool_output_rejected"
                    return record
                now_ms = round((time.monotonic() - started) * 1000, 3)
                if any(delta.get(field) for field in ("content", "reasoning_content", "reasoning")):
                    if record["ttft_ms"] is None:
                        record["ttft_ms"] = now_ms
                text = delta.get("content")
                if text:
                    if not isinstance(text, str):
                        raise ValueError()
                    if record["first_visible_token_ms"] is None:
                        record["first_visible_token_ms"] = now_ms
                    content += text
                if choice.get("finish_reason"):
                    record["finish_reason"] = choice["finish_reason"]
        if expired.is_set():
            raise TimeoutError()
        if not done or record["finish_reason"] != "stop":
            record["error"] = "incomplete_output"
            return record
        try:
            parsed = json.loads(content)
        except (ValueError, TypeError):
            record["schema_valid"] = False
            record["error"] = "invalid_json_output"
            return record
        valid = (isinstance(parsed, dict) and set(parsed) == {"session", "answer", "marker"}
                 and isinstance(parsed["session"], str) and type(parsed["answer"]) is int
                 and isinstance(parsed["marker"], str))
        record["schema_valid"] = valid
        if valid:
            record["answer_correct"] = parsed["answer"] == index + level
            record["session_marker_matches"] = parsed["session"] == session and parsed["marker"] == secret
        if usage:
            for source, target in (("prompt_tokens", "input_tokens"), ("completion_tokens", "output_tokens")):
                value = usage.get(source)
                if type(value) is int and value >= 0:
                    record[target] = value
        if valid and record["answer_correct"] and record["session_marker_matches"]:
            record["status"] = "completed"
        else:
            record["error"] = "synthetic_expectation_failed"
    except (TimeoutError, socket.timeout):
        record["error"] = "deadline_exceeded"
    except Exception:
        record["error"] = "deadline_exceeded" if expired.is_set() else "provider_protocol_error"
    finally:
        watchdog.cancel()
        connection.close()
        record["latency_ms"] = round((time.monotonic() - started) * 1000, 3)
        if record["output_tokens"] is not None and record["latency_ms"] > 0:
            record["tokens_per_second"] = round(record["output_tokens"] * 1000 / record["latency_ms"], 3)
    return record


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--deadline-seconds", type=float, default=30)
    args = parser.parse_args()
    if not 1 <= args.deadline_seconds <= 120:
        parser.error("deadline must be between 1 and 120 seconds")
    try:
        url, model, key = configuration()
    except Exception:
        print(json.dumps({"error": "invalid_local_model_configuration"}))
        return 2
    # Refuse to overwrite an earlier failure or successful measurement.
    args.output.mkdir(parents=True, exist_ok=False)
    summaries = []
    with (args.output / "sessions.jsonl").open("x") as output:
        for level in LEVELS:
            cpu_before = resource.getrusage(resource.RUSAGE_SELF)
            started = time.monotonic()
            with concurrent.futures.ThreadPoolExecutor(max_workers=level) as pool:
                futures = [pool.submit(probe, url, model, key, level, index, args.deadline_seconds)
                           for index in range(level)]
                records = [future.result() for future in futures]
            elapsed = time.monotonic() - started
            cpu_after = resource.getrusage(resource.RUSAGE_SELF)
            for record in records:
                output.write(json.dumps(record, ensure_ascii=False) + "\n")
            output.flush()
            latencies = sorted(record["latency_ms"] for record in records)
            summary = {"sessions": level, "requests": len(records),
                       "completed": sum(record["status"] == "completed" for record in records),
                       "timeouts": sum(record["error"] == "deadline_exceeded" for record in records),
                       "schema_invalid": sum(record["schema_valid"] is False for record in records),
                       "output_unvalidated": sum(record["schema_valid"] is None for record in records),
                       "session_marker_mismatch": sum(record["schema_valid"] is True and record["session_marker_matches"] is False for record in records),
                       "all_request_p95_ms": latencies[math.ceil(len(latencies) * .95) - 1],
                       "wall_seconds": round(elapsed, 3),
                       "client_cpu_seconds": round(cpu_after.ru_utime + cpu_after.ru_stime - cpu_before.ru_utime - cpu_before.ru_stime, 6),
                       "client_rss_kib": next((int(line.split()[1]) for line in Path("/proc/self/status").read_text().splitlines() if line.startswith("VmRSS:")), None),
                       "server_cpu": None, "server_rss": None, "gpu_vram": None,
                       "gateway_queue_wait_ms": None, "automatic_retries": 0}
            summaries.append(summary)
            print(json.dumps(summary), flush=True)
    report = {"evidence_level": "live_synthetic", "model": model, "prompt_version": PROMPT_VERSION,
              "provider_only": True, "character_runtime_exercised": False,
              "golden_holdout_exercised": False, "qq_calls": 0,
              "model_revision": None, "weights_hash": None, "chat_template_revision": None,
              "deadline_seconds": args.deadline_seconds, "levels": summaries,
              "limitations": ["One burst per level; not an SLO or a stability claim.",
                              "No Gateway/Character queue, routing or private-history isolation proof.",
                              "Session marker checks are synthetic response correlation only.",
                              "Client RSS/CPU do not measure the model server; server GPU/CPU/RSS remain unknown.",
                              "TTFT records first nonempty content or reasoning delta; visible text is separate.",
                              "Token rate includes request time and any hidden reasoning tokens reported by provider."]}
    (args.output / "summary.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
    return 0 if all(s["completed"] == s["sessions"] for s in summaries) else 1


if __name__ == "__main__":
    raise SystemExit(main())
