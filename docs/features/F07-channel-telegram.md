# F07 — Telegram Channel Adapter

**Phase:** 0 | **Wave:** 0.B | **Dependencies:** F06 (`ChannelAdapter` Protocol)

## Goal

Implement the `ChannelAdapter` Protocol against the Telegram Bot API.

## Scope (in)

- Webhook signature verification via `X-Telegram-Bot-Api-Secret-Token` header (constant-time compare).
- Parse Telegram `Update` JSON → `NormalizedMessage`. `channel_id = "tg:<bot_id>:<chat_id>"`. Forum topics: `thread_id = "<chat_id>:<topic_id>"`.
- Outbound via `sendMessage` (text), `sendPhoto`, `sendDocument` (long-text overflow).
- Long-text chunking (paragraph > newline > sentence > raw) at 4096 char limit.
- Self-registration via `gateway.channel_registry.register_channel("telegram", adapter)`.

## Scope (out)

- Bot creation / `setWebhook` automation (operator runs this once via curl).
- Inline keyboards, callback queries (Phase 3).

## Deliverables

```
adapters/channels/telegram/
├── pyproject.toml
├── README.md
├── src/channel_telegram/
│   ├── __init__.py
│   ├── config.py
│   ├── parser.py
│   ├── api.py                  # TelegramBotApi (httpx)
│   └── adapter.py              # TelegramAdapter
└── tests/
    ├── conftest.py
    ├── test_adapter.py
    ├── test_parser.py
    └── fixtures/
        ├── update_text.json
        ├── update_photo.json
        └── update_forum_topic.json
```

## Acceptance criteria

1. `pytest adapters/channels/telegram/tests/` passes.
2. Text fixture parses to `channel_id="tg:<bot_id>:<chat_id>"`, `thread_id="<chat_id>"`.
3. Forum topic fixture appends topic id to thread.
4. Signature mismatch → adapter rejects.
5. Live test gated on `TELEGRAM_LIVE_TEST=1` + bot token.
6. Long text (8000 chars) splits into chunks each ≤ 4096.

## Reference implementations

- `~/Workspace/open-source/openclaw/extensions/telegram/`
