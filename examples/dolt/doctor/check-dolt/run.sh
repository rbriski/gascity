#!/usr/bin/env bash
# Pack doctor check: verify Dolt binary and required tools.
#
# Exit codes: 0=OK, 1=Warning, 2=Error
# stdout: first line=message, rest=details

if ! command -v dolt >/dev/null 2>&1; then
    echo "dolt binary not found"
    echo "install dolt: https://docs.dolthub.com/introduction/installation"
    exit 2
fi

# Check flock (required for concurrent start prevention).
if ! command -v flock >/dev/null 2>&1; then
    echo "flock not found (needed for Dolt server locking)"
    echo "Install: apt install util-linux (Linux) or brew install flock (macOS)"
    exit 2
fi

# Check lsof (required for port conflict detection).
if ! command -v lsof >/dev/null 2>&1; then
    echo "lsof not found (needed for port conflict detection)"
    echo "Install: apt install lsof (Linux) or available by default (macOS)"
    exit 2
fi

version=$(dolt version 2>/dev/null | head -1)

# Require dolt >= 1.86.2 due to upstream GC/writer deadlock fix.
# Older versions hang sql-server during dolt_backup('sync', ...) under
# heavy concurrent write load; the watchdog then force-kills the server.
# See dolthub/dolt commit ccf7bde206 (PR #10876).
required="1.86.2"
ver_str=$(printf '%s' "$version" | sed -E 's/.*dolt version //; s/[^0-9.].*//')
if [ -n "$ver_str" ]; then
    lowest=$(printf '%s\n%s\n' "$required" "$ver_str" | sort -V | head -1)
    if [ "$lowest" != "$required" ]; then
        echo "dolt $ver_str is too old (need >= $required) — upgrade required"
        echo "Reason: <1.86.2 has a GC/writer deadlock that hangs sql-server during dolt_backup sync under heavy commit load. See dolthub/dolt commit ccf7bde206."
        echo "Install: https://github.com/dolthub/dolt/releases"
        exit 2
    fi
fi

echo "dolt available ($version), flock ok, lsof ok"
exit 0
