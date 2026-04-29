# F08 — Zalo Channel Adapter

**Phase:** 1 | **Wave:** 1.A | **Dependencies:** F06 (`ChannelAdapter` Protocol)

## Goal

Implement `ChannelAdapter` against the Zalo Official Account (OA) API. Distinct from openclaw's Zalo Bot Creator extension (different endpoints, different auth model).

## Scope (in)

- Webhook signature verification: `X-ZEvent-Signature` = HMAC-SHA256(app_secret, body).
- Parse Zalo OA event → `NormalizedMessage`. `channel_id = "zalo:<oa_id>:<sender_id>"`.
- Outbound via `/v3.0/oa/message/cs` (consumer-service messaging).
- OA access-token refresher: refresh ~23h after issue (Zalo's 24h validity), structured backoff alerts on failure, disable send after 1h failure window.
- 24h messaging-window awareness: surface a structured error when sending outside the window.
- Self-registration via `gateway.channel_registry.register_channel("zalo", adapter)`.

## Scope (out)

- Multi-OA fan-out (Phase 3).
- Zalo Bot Creator API (different surface — out of scope).

## Deliverables

```
adapters/channels/zalo/
├── pyproject.toml
├── README.md
├── src/channel_zalo/
│   ├── __init__.py
│   ├── config.py
│   ├── parser.py
│   ├── api.py                  # ZaloOaApi
│   ├── adapter.py
│   └── token_refresher.py
└── tests/
    ├── conftest.py
    ├── test_adapter.py
    ├── test_parser.py
    ├── test_token_refresher.py
    └── fixtures/
        ├── event_text.json
        └── event_attachment.json
```

## Acceptance criteria

1. `pytest adapters/channels/zalo/tests/` passes.
2. `event_text.json` parses to `channel_id=zalo:<oa>:<sender>`, `thread_id=<sender>`.
3. Forged signature rejected.
4. Token refresher: 23h advance fires refresh; failure path emits backoff + structured alert; recovery emits `recovered` alert.
5. Live test gated on `ZALO_LIVE_TEST=1`.

## Reference implementations

- `~/Workspace/open-source/openclaw/extensions/zalo/` (Bot API; F08 ports patterns, not endpoints)
