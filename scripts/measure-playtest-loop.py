#!/usr/bin/env python3
"""Measure the tester loop's per-action cost through `playtest mcp`.

Runs one fixed short Doom mission (collect the nearest bullets pickup, then
fire once with a capture) and reports, per action, the MCP calls, the answer
size a model reads (text bytes, and image tokens estimated as w*h/750), and
wall time. Each configuration starts and stops its own hosted session.

usage: scripts/measure-playtest-loop.py PROFILE[:full|compact] ...
"""
import json
import math
import struct
import subprocess
import sys
import time
import base64


class MCP:
    def __init__(self):
        self.process = subprocess.Popen(["playtest", "mcp"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
        self.next_id = 0
        self.call("initialize", {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "measure", "version": "1"}})

    def call(self, method, params):
        self.next_id += 1
        self.process.stdin.write(json.dumps({"jsonrpc": "2.0", "id": self.next_id, "method": method, "params": params}) + "\n")
        self.process.stdin.flush()
        while True:
            message = json.loads(self.process.stdout.readline())
            if message.get("id") == self.next_id:
                if "error" in message:
                    raise RuntimeError(message["error"])
                return message["result"]

    def close(self):
        self.process.stdin.close()
        self.process.wait(timeout=10)


class Meter:
    def __init__(self, mcp, full):
        self.mcp, self.full = mcp, full
        self.calls = self.text_bytes = self.image_tokens = 0
        self.seconds = 0.0

    def tool(self, name, arguments):
        if name == "assist" and self.full:
            arguments = {**arguments, "data": {**arguments["data"], "detail": "full"}}
        started = time.monotonic()
        result = self.mcp.call("tools/call", {"name": name, "arguments": arguments})
        self.seconds += time.monotonic() - started
        self.calls += 1
        value = None
        for block in result["content"]:
            if block["type"] == "text":
                self.text_bytes += len(block["text"].encode())
                if value is None:
                    try:
                        value = json.loads(block["text"])
                    except ValueError:
                        value = block["text"]
            elif block["type"] == "image":
                png = base64.b64decode(block["data"])
                width, height = struct.unpack(">II", png[16:24])
                self.image_tokens += math.ceil(width * height / 750)
        if result.get("isError"):
            raise RuntimeError(value)
        return value


def bearing(observation, x, z):
    position, axes = observation["player"]["position"], observation["axes"]
    dx, dz = x - position["x"], z - position["z"]
    right, forward = axes["right"], axes["forward"]
    return math.degrees(math.atan2(dx * right["x"] + dz * right["z"], dx * forward["x"] + dz * forward["z"]))


def mission(profile, full):
    mcp = MCP()
    meter = Meter(mcp, full)
    start = mcp.call("tools/call", {"name": "start", "arguments": {"game": profile}})
    session = json.loads(start["content"][0]["text"])["id"]
    actions = 0
    try:
        assist = lambda data: meter.tool("assist", {"session_id": session, "data": data})
        # A browser page installs its playtest hook after loading; not measured.
        for _ in range(120):
            ready = mcp.call("tools/call", {"name": "assist", "arguments": {"session_id": session, "data": {"op": "discover"}}})
            if not ready.get("isError"):
                break
            time.sleep(1)
        assist({"op": "time", "mode": "manual"})
        # Observations are read in full: steering needs the pickup positions.
        observe = lambda: json.loads(mcp.call("tools/call", {"name": "assist", "arguments": {"session_id": session, "data": {"op": "observe", "detail": "full"}}})["content"][0]["text"])
        meter.tool("assist", {"session_id": session, "data": {"op": "observe"}})
        observation = observe()
        pickup = min((p for p in observation["pickups"] if p["kind"] == "Bullets" and not p["collected"]), key=lambda p: p["distance"])
        for _ in range(6):
            turn = bearing(observation, pickup["position"]["x"], pickup["position"]["z"])
            if abs(turn) > 3:
                assist({"op": "look", "yaw": round(turn, 2)})
            assist({"op": "act", "id": "forward", "ms": min(600, max(150, int(pickup["distance"] * 120))), "capture": True})
            actions += 1
            observation = observe()
            pickup = next(p for p in observation["pickups"] if p["id"] == pickup["id"])
            if pickup["collected"]:
                break
        assist({"op": "act", "id": "attack", "capture": True})
        actions += 1
        collected = pickup["collected"]
    finally:
        mcp.call("tools/call", {"name": "stop", "arguments": {"session_id": session}})
        mcp.close()
    return {"profile": profile, "answers": "full" if full else "compact", "actions": actions, "collected": collected,
            "calls_per_action": round(meter.calls / actions, 2), "text_bytes_per_action": meter.text_bytes // actions,
            "approx_text_tokens_per_action": meter.text_bytes // 4 // actions, "image_tokens_per_action": meter.image_tokens // actions,
            "wall_ms_per_action": round(meter.seconds * 1000 / actions)}


if __name__ == "__main__":
    for spec in sys.argv[1:]:
        profile, _, detail = spec.partition(":")
        print(json.dumps(mission(profile, detail == "full")), flush=True)
