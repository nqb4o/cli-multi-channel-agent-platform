#!/usr/bin/env bash
# F02 fake codex CLI used in tests.
#
# Driven entirely by env vars so tests can pin the exact behaviour they
# need without conditionals on argv:
#
#   FIXTURE        — basename of a JSONL file under FIXTURES_DIR (or
#                    $(dirname "$0")/jsonl, falling back to $(dirname "$0"))
#                    without extension. Mutually exclusive with RAW_STDOUT.
#   FIXTURES_DIR   — explicit directory holding <FIXTURE>.jsonl files.
#                    Used by the Go test harness so the same fake script
#                    drives both flat (Go) and nested (Python) layouts.
#   RAW_STDOUT     — literal stdout to emit, verbatim. Use this for the
#                    resume-mode plain-text fixture.
#   STDERR_TEXT    — literal stderr to emit. Empty by default.
#   EXIT_CODE      — exit status. Defaults to 0.
#   ARGV_LOG       — if set to a file path, write the argv this script
#                    received (one arg per line) to that file before
#                    exiting. Tests use this to assert argv assembly.
#   STDIN_LOG      — if set to a file path, copy the contents of stdin
#                    to that file before exiting.
#   SLEEP_SECONDS  — if set, sleep that many seconds before emitting
#                    output. Used by the cancellation test.

set -u

if [[ -n "${ARGV_LOG:-}" ]]; then
    : > "$ARGV_LOG"
    for arg in "$@"; do
        printf '%s\n' "$arg" >> "$ARGV_LOG"
    done
fi

if [[ -n "${STDIN_LOG:-}" ]]; then
    cat - > "$STDIN_LOG"
else
    # Drain stdin so the parent's communicate() doesn't block.
    cat - >/dev/null 2>&1 || true
fi

if [[ -n "${SLEEP_SECONDS:-}" ]]; then
    sleep "$SLEEP_SECONDS"
fi

if [[ -n "${STDERR_TEXT:-}" ]]; then
    printf '%s' "$STDERR_TEXT" 1>&2
fi

if [[ -n "${RAW_STDOUT:-}" ]]; then
    printf '%s' "$RAW_STDOUT"
elif [[ -n "${FIXTURE:-}" ]]; then
    if [[ -n "${FIXTURES_DIR:-}" ]]; then
        fixture_path="${FIXTURES_DIR}/${FIXTURE}.jsonl"
    elif [[ -f "$(dirname "$0")/jsonl/${FIXTURE}.jsonl" ]]; then
        fixture_path="$(dirname "$0")/jsonl/${FIXTURE}.jsonl"
    else
        fixture_path="$(dirname "$0")/${FIXTURE}.jsonl"
    fi
    if [[ -f "$fixture_path" ]]; then
        cat "$fixture_path"
    else
        printf 'fake_codex: fixture not found: %s\n' "$fixture_path" 1>&2
        exit 99
    fi
fi

exit "${EXIT_CODE:-0}"
