#!/usr/bin/env bash
# F04 fake claude CLI used in Go tests.
#
# Driven entirely by env vars so tests can pin the exact behaviour they
# need without conditionals on argv:
#
#   FIXTURE        — basename of a JSONL file under FIXTURES_DIR
#                    (default <script-dir>), no extension. Mutually
#                    exclusive with RAW_STDOUT.
#   FIXTURES_DIR   — override the directory the fixture is loaded from.
#   RAW_STDOUT     — literal stdout to emit, verbatim.
#   STDERR_TEXT    — literal stderr to emit. Empty by default.
#   EXIT_CODE      — exit status. Defaults to 0.
#   ARGV_LOG       — if set to a file path, write the argv this script
#                    received (NUL-separated) to that file before
#                    exiting. Tests use this to assert argv assembly.
#   STDIN_LOG      — if set to a file path, copy the contents of stdin
#                    (the user prompt) to that file before exiting.
#   SLEEP_SECONDS  — if set, sleep that many seconds before emitting
#                    output. Used by the cancellation test.

set -u

if [[ -n "${ARGV_LOG:-}" ]]; then
    : > "$ARGV_LOG"
    # Use NUL-separator so multi-line args (e.g. an --append-system-prompt
    # block that contains newlines) round-trip cleanly.
    for arg in "$@"; do
        printf '%s\0' "$arg" >> "$ARGV_LOG"
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
    fixtures_dir="${FIXTURES_DIR:-$(dirname "$0")}"
    fixture_path="${fixtures_dir}/${FIXTURE}.jsonl"
    if [[ -f "$fixture_path" ]]; then
        cat "$fixture_path"
    else
        printf 'fake_claude: fixture not found: %s\n' "$fixture_path" 1>&2
        exit 99
    fi
fi

exit "${EXIT_CODE:-0}"
