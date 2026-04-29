#!/usr/bin/env bash
# F03 fake gemini CLI used in Go tests.
#
# Driven entirely by env vars so tests can pin the exact behaviour they
# need without conditionals on argv:
#
#   FIXTURE        — basename of a JSON file under FIXTURES_DIR
#                    (default $(dirname $0)), no extension. Mutually
#                    exclusive with RAW_STDOUT.
#   FIXTURES_DIR   — override the directory the fixture is loaded from
#                    (defaults to "$(dirname "$0")").
#   RAW_STDOUT     — literal stdout to emit, verbatim. Use for shapes
#                    that don't fit a JSON file (truncated output, log
#                    banners, etc.).
#   STDERR_TEXT    — literal stderr to emit. Empty by default.
#   EXIT_CODE      — exit status. Defaults to 0.
#   ARGV_LOG       — if set to a file path, write the argv this script
#                    received (NUL-separated) to that file before
#                    exiting. Tests use this to assert argv assembly.
#   VERSION_OUTPUT — if `--version` is the only argument, print this
#                    string to stdout and exit 0 (probe handler).
#                    Defaults to "gemini 0.0.0-test\n".
#   SLEEP_SECONDS  — if set, sleep that many seconds before emitting
#                    output. Used by the cancellation test.

set -u

# Special-case the --version probe so the backend's init-time check
# doesn't pollute the per-turn argv log.
if [[ "$#" -eq 1 && "$1" == "--version" ]]; then
    printf '%s' "${VERSION_OUTPUT:-gemini 0.0.0-test
}"
    exit 0
fi

if [[ -n "${ARGV_LOG:-}" ]]; then
    : > "$ARGV_LOG"
    # NUL-separated so multi-line args (e.g. a --prompt that contains a
    # composed [SYSTEM]/[USER] block) round-trip cleanly.
    for arg in "$@"; do
        printf '%s\0' "$arg" >> "$ARGV_LOG"
    done
fi

# Gemini reads the prompt from --prompt; nothing on stdin. Drain anyway
# in case the runner pipes anything by accident.
cat - >/dev/null 2>&1 || true

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
    fixture_path="${fixtures_dir}/${FIXTURE}.json"
    if [[ -f "$fixture_path" ]]; then
        cat "$fixture_path"
    else
        printf 'fake_gemini: fixture not found: %s\n' "$fixture_path" 1>&2
        exit 99
    fi
fi

exit "${EXIT_CODE:-0}"
