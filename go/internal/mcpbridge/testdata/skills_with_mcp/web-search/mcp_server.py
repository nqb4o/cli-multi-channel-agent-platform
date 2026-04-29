#!/usr/bin/env python3
"""Stub MCP server for the web-search skill — used by F11 bridge tests.

Speaks the minimal JSON-RPC 2.0 subset that McpBridge exercises:
  initialize  → {protocolVersion, capabilities, serverInfo}
  tools/list  → {tools: [{name, description, inputSchema}]}
  tools/call  → {content: [{type, text}]}

Each request arrives as one newline-terminated JSON line on stdin;
each response is one newline-terminated JSON line written to stdout.
"""
import json
import sys

TOOLS = [
    {
        "name": "search",
        "description": "Search the web for a query.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "query": {"type": "string", "description": "Search query"},
            },
            "required": ["query"],
        },
    }
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
                "serverInfo": {"name": "web-search-mcp", "version": "0.1.0"},
            },
        }

    if method == "tools/list":
        return {"jsonrpc": "2.0", "id": req_id, "result": {"tools": TOOLS}}

    if method == "tools/call":
        params = request.get("params", {})
        args = params.get("arguments", {})
        query = args.get("query", "")
        return {
            "jsonrpc": "2.0",
            "id": req_id,
            "result": {
                "content": [
                    {"type": "text", "text": f"stub result for query={query!r}"}
                ]
            },
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
