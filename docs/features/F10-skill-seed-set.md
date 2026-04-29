# F10 — Seed Skill Set (5 bundled skills)

**Phase:** 1 | **Wave:** 1.A | **Dependencies:** F09 (FROZEN schema)

## Goal

Ship 5 first-class skills under `skills/bundled/` that validate against F09's frozen schema and showcase the SKILL.md + scripts + optional MCP server pattern.

## Skills

1. **`web-search`** — provider-agnostic web search (Tavily / SerpAPI / Brave). `required_env: [WEB_SEARCH_API_KEY]`.
2. **`summarize-url`** — fetch URL, extract main content, summarize.
3. **`trend-analysis`** — time-series OHLCV trend indicators (uses `yfinance` + `pandas`).
4. **`regime-detection`** — HMM-based market regime detection (uses `hmmlearn` if available; falls back to a heuristic).
5. **`image-describe`** — describe an image via the active CLI's vision capability.

Each skill has:

- `SKILL.md` (YAML frontmatter conforming to F09 v1)
- `scripts/` for any Python helpers
- `mcp_server.py` exposing the skill's tools via stdio MCP
- `pyproject.toml` declaring deps (lazy-imported in the MCP server so unit tests run without heavy deps)

## Scope (out)

- F11 spawns the MCP servers; F10 just defines them.
- Sandbox Dockerfile change to `COPY skills/bundled/` (F01 owns the image).
- E2E MCP tests (`SKILL_E2E=1`) — depend on F11.

## Deliverables

```
skills/
├── README.md
├── bundled/
│   ├── web-search/{SKILL.md, scripts/search.py, mcp_server.py, pyproject.toml, env.example}
│   ├── summarize-url/{SKILL.md, scripts/extract.py, mcp_server.py, pyproject.toml}
│   ├── trend-analysis/{SKILL.md, scripts/{fetch_ohlcv,compute_indicators}.py, mcp_server.py, pyproject.toml}
│   ├── regime-detection/{SKILL.md, scripts/hmm_regime.py, mcp_server.py, pyproject.toml}
│   └── image-describe/{SKILL.md, scripts/inspect.py, mcp_server.py, pyproject.toml}
└── tests/
    ├── conftest.py
    ├── test_frontmatter.py
    └── test_<slug>.py             # per-skill behavioural tests
```

## Acceptance criteria

1. F09's loader validates all 5 manifests with zero `unknown_field` / `slug_mismatch` warnings.
2. `pytest skills/tests/` passes (uses HTTP and data-fetch fakes).
3. Each `mcp_server.py` imports cleanly and `list_tools()` returns the documented tool name.
4. Catalog renderer over the 5 skills stays under the 1500-token budget.

## Reference implementations

- `~/Workspace/open-source/openclaw/skills/summarize/SKILL.md`
- `~/Workspace/open-source/openclaw/dist-runtime/extensions/tavily/skills/tavily/SKILL.md`
