# sandbox.Dockerfile — image baked into every Daytona sandbox.
#
# This is the user-facing sandbox: the per-user container that the F01
# orchestrator boots, hibernates, and resumes. The image bakes the three
# provider CLIs (codex, gemini, claude) plus Python 3.12 + the
# google-adk runtime so the F05 agent-runtime daemon can start without
# any extra installation step on first boot.
#
# Volume mount paths (FROZEN — see services/orchestrator/README.md):
#
#   /home/user/.codex      Codex CLI auth + chat history
#   /home/user/.gemini      Gemini CLI auth + chat history
#   /home/user/.claude     Claude Code auth + plugin marketplace cache
#   /home/user/workspace   Per-user workspace (agent.yaml, persona files,
#                          run-scoped plugin dirs, MCP loopback temp files)
#
# Each of these is backed by a per-user Daytona Volume so destroy + recreate
# keeps auth + workspace intact.
#
# Build (from repo root):
#   docker build -f infra/docker/sandbox.Dockerfile -t agent-platform/sandbox:dev .

FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    NODE_VERSION=22

# --- Base system ---------------------------------------------------------
#
# tini is the entrypoint to reap zombie subprocesses (each provider CLI
# spawns helpers; without tini those leak as defunct processes for the
# life of the sandbox).
#
# ca-certificates is required for the provider CLIs to verify TLS to
# their own APIs.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        gnupg \
        git \
        tini \
        sudo \
 && rm -rf /var/lib/apt/lists/*

# --- Python 3.12 ---------------------------------------------------------
#
# Ubuntu 24.04 ships with Python 3.12 in the default archive; we install
# python3.12, the venv module, and pip explicitly so the sandbox image is
# self-contained without relying on update-alternatives.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        python3.12 \
        python3.12-venv \
        python3-pip \
 && ln -sf /usr/bin/python3.12 /usr/local/bin/python \
 && ln -sf /usr/bin/python3.12 /usr/local/bin/python3 \
 && rm -rf /var/lib/apt/lists/*

# --- Node.js 22 + npm ----------------------------------------------------
#
# All three provider CLIs (codex, gemini, claude) ship as npm packages.
# We use the NodeSource APT repo so the version stays pinned.
RUN curl -fsSL https://deb.nodesource.com/setup_${NODE_VERSION}.x \
        -o /tmp/nodesource_setup.sh \
 && bash /tmp/nodesource_setup.sh \
 && apt-get install -y --no-install-recommends nodejs \
 && rm -rf /var/lib/apt/lists/* /tmp/nodesource_setup.sh

# --- Provider CLIs (global npm install) ----------------------------------
#
# Pinned globally so they're available on PATH for any user inside the
# sandbox. F02/F03/F04 backends invoke them by basename.
RUN npm install -g \
        @openai/codex \
        @google/gemini-cli \
        @anthropic-ai/claude-code

# --- Google ADK + agent-runtime dependencies -----------------------------
#
# The F05 agent-runtime daemon imports the google-adk harness when
# RUNTIME_USE_ADK is unset (the default). pyyaml + httpx + opentelemetry
# round out the daemon's runtime deps so the sandbox's first turn does
# not have to download wheels.
RUN pip install --no-cache-dir --break-system-packages \
        google-adk \
        pyyaml \
        httpx \
        cryptography \
        asyncpg \
        opentelemetry-api \
        opentelemetry-sdk

# --- Per-user account ----------------------------------------------------
#
# Daytona's runtime expects a non-root user named "user" with UID 1000.
# Volume mount paths are absolute under /home/user.
RUN useradd --create-home --uid 1000 --shell /bin/bash user \
 && mkdir -p /home/user/.codex /home/user/.gemini /home/user/.claude \
              /home/user/workspace \
 && chown -R user:user /home/user

USER user
WORKDIR /home/user

# Volumes are declared so docker / tooling knows which paths are intended
# to be backed by Daytona Volumes. Daytona itself mounts them by ID at
# runtime; the VOLUME directive here is documentation + a hint to local
# dev tooling.
VOLUME ["/home/user/.codex", "/home/user/.gemini", "/home/user/.claude", "/home/user/workspace"]

# tini reaps grandchild processes spawned by the provider CLIs.
ENTRYPOINT ["/usr/bin/tini", "--"]

# Default command keeps the container alive — the F01 orchestrator
# starts the agent-runtime daemon on demand via Daytona's exec API.
CMD ["sleep", "infinity"]
