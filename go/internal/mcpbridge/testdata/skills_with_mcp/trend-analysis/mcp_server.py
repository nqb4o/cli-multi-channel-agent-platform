#!/usr/bin/env python3
"""Stub MCP server for the trend-analysis skill — used by F11 bridge tests.

Exposes two tools: analyze_trend and summary.
"""
import json
import sys

TOOLS = [
    {
        "name": "analyze_trend",
        "description": "Analyze trend data for a topic.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "topic": {"type": "string"},
                "period": {"type": "string"},
            },
            "required": ["topic"],
        },
    },
    {
        "name": "summary",
        "description": "Return a brief trend summary.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "topic": {"type": "string"},
            },
            "required": ["topic"],
        },
    },
]


def handle(request: dict) -> dict:
    method = request.get("method")
    req_id = request.get("id")

    if method == "initialize":
        return {
            "jsonrpc": "2.0",
            "id": req_id,
            "result": {
                "protocolVersion": "2024-11-05",
                "capabilities": {},
                "serverInfo": {"name": "trend-analysis-mcp", "version": "0.1.0"},
            },
        }

    if method == "tools/list":
        return {"jsonrpc": "2.0", "id": req_id, "result": {"tools": TOOLS}}

    if method == "tools/call":
        params = request.get("params", {})
        tool_name = params.get("name", "")
        args = params.get("arguments", {})
        topic = args.get("topic", "")

        if tool_name == "analyze_trend":
            period = args.get("period", "1w")
            text = f"trend analysis for topic={topic!r}, period={period!r}"
        elif tool_name == "summary":
            text = f"trend summary for topic={topic!r}"
        else:
            text = f"unknown tool {tool_name!r}"

        return {
            "jsonrpc": "2.0",
            "id": req_id,
            "result": {"content": [{"type": "text", "text": text}]},
        }

    return {
        "jsonrpc": "2.0",
        "id": req_id,
        "error": {"code": -32601, "message": f"method not found: {method!r}"},
    }


for raw_line in sys.stdin:
    raw_line = raw_line.strip()
    if not raw_line:
        continue
    try:
        req = json.loads(raw_line)
    except json.JSONDecodeError as exc:
        resp = {"jsonrpc": "2.0", "id": None, "error": {"code": -32700, "message": str(exc)}}
    else:
        resp = handle(req)
    sys.stdout.write(json.dumps(resp) + "\n")
    sys.stdout.flush()
