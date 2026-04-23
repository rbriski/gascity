#!/usr/bin/env bash
# compactor — flatten Dolt commit history on each user DB to reclaim storage.
#
# Replaces mol-dog-compactor formula+pool. All operations are
# deterministic: soft-reset the main branch via a temporary branch,
# swap the result back, and run DOLT_GC to reclaim unreferenced
# chunks. No LLM judgment is required — runs inline in the
# controller as an exec order (mirrors mol-dog-reaper).
#
# Safety: flatten preserves every row in every user table. The
# commit graph collapses to a single commit; row data does not
# change. Pre/post row counts are compared for integrity; any
# divergence escalates to mayor and the database is skipped.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PACK_DIR="${GC_PACK_DIR:-$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)}"
: "${GC_DOLT_USER:=root}"
# shellcheck source=./runtime.sh
. "$PACK_DIR/assets/scripts/runtime.sh"

THRESHOLD="${GC_COMPACTOR_THRESHOLD:-500}"
DRY_RUN="${GC_COMPACTOR_DRY_RUN:-}"

HOST="${GC_DOLT_HOST:-127.0.0.1}"

dolt_sql() {
    DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}" \
        dolt --host "$HOST" --port "$GC_DOLT_PORT" --user "$GC_DOLT_USER" --no-tls sql "$@"
}

# Enumerate user databases. Skip MySQL/Dolt system schemas and Gas
# City's internal health-probe DB; "dolt" is the server's own
# information database and must never be compacted.
DATABASES=$(dolt_sql -r csv -q "SHOW DATABASES" 2>/dev/null \
    | tail -n +2 \
    | grep -Ev -i '^(information_schema|mysql|dolt_cluster|__gc_probe|dolt)$' \
    || true)

if [ -z "$DATABASES" ]; then
    gc nudge deacon/ "DOG_DONE: compactor — no user databases" 2>/dev/null || true
    exit 0
fi

TOTAL_COMPACTED=0
TOTAL_SKIPPED=0
TOTAL_FAILED=0
COMMITS_BEFORE_SUM=0
COMMITS_AFTER_SUM=0

for DB in $DATABASES; do
    # Reject DB names with anything outside [A-Za-z0-9_] before
    # interpolating into a Dolt SQL identifier. The health script
    # applies the same filter (see commands/health/run.sh) — data
    # directories are server-controlled, but guarding against
    # config drift is cheap.
    case "$DB" in
        *[!A-Za-z0-9_]*|'') continue ;;
    esac

    COMMITS=$(dolt_sql -r csv -q "USE \`$DB\`; SELECT COUNT(*) FROM dolt_log" 2>/dev/null \
        | grep -E '^[0-9]+$' | head -1 || true)
    case "$COMMITS" in
        ''|*[!0-9]*)
            TOTAL_FAILED=$((TOTAL_FAILED + 1))
            continue
            ;;
    esac

    if [ "$COMMITS" -le "$THRESHOLD" ]; then
        TOTAL_SKIPPED=$((TOTAL_SKIPPED + 1))
        continue
    fi

    if [ -n "$DRY_RUN" ]; then
        echo "compactor: would flatten $DB ($COMMITS commits)"
        TOTAL_COMPACTED=$((TOTAL_COMPACTED + 1))
        continue
    fi

    # Pre-flight: snapshot row counts for every user table so we can
    # detect row-level regressions after the flatten.
    TABLES=$(dolt_sql -r csv -q "
        SELECT table_name FROM information_schema.tables
        WHERE table_schema = '$DB' AND table_name NOT LIKE 'dolt_%'
    " 2>/dev/null | tail -n +2 || true)

    pre_file=$(mktemp)
    post_file=$(mktemp)

    integrity_ok=1
    for TBL in $TABLES; do
        case "$TBL" in
            *[!A-Za-z0-9_]*|'') continue ;;
        esac
        CNT=$(dolt_sql -r csv -q "USE \`$DB\`; SELECT COUNT(*) FROM \`$TBL\`" 2>/dev/null \
            | grep -E '^[0-9]+$' | head -1 || true)
        case "$CNT" in
            ''|*[!0-9]*) CNT=0 ;;
        esac
        printf '%s|%s\n' "$TBL" "$CNT" >> "$pre_file"
    done

    # The flatten recipe runs in a single session so DOLT_BRANCH /
    # DOLT_CHECKOUT / DOLT_RESET / DOLT_COMMIT share session state
    # (branch + transaction). DOLT_GC must run in a separate session
    # (it cannot execute inside an explicit transaction).
    if ! dolt_sql <<SQL 2>/dev/null
USE \`$DB\`;
SET @initial := (SELECT commit_hash FROM dolt_log ORDER BY date ASC LIMIT 1);
CALL DOLT_BRANCH('flatten-tmp');
CALL DOLT_CHECKOUT('flatten-tmp');
CALL DOLT_RESET('--soft', @initial);
CALL DOLT_COMMIT('-Am', 'compaction: flatten history');
CALL DOLT_CHECKOUT('main');
CALL DOLT_RESET('--hard', 'flatten-tmp');
CALL DOLT_BRANCH('-D', 'flatten-tmp');
SQL
    then
        TOTAL_FAILED=$((TOTAL_FAILED + 1))
        rm -f "$pre_file" "$post_file"
        continue
    fi

    for TBL in $TABLES; do
        case "$TBL" in
            *[!A-Za-z0-9_]*|'') continue ;;
        esac
        CNT=$(dolt_sql -r csv -q "USE \`$DB\`; SELECT COUNT(*) FROM \`$TBL\`" 2>/dev/null \
            | grep -E '^[0-9]+$' | head -1 || true)
        case "$CNT" in
            ''|*[!0-9]*) CNT=0 ;;
        esac
        printf '%s|%s\n' "$TBL" "$CNT" >> "$post_file"
    done

    if ! diff -q "$pre_file" "$post_file" >/dev/null 2>&1; then
        MISMATCH=$(diff "$pre_file" "$post_file" 2>&1 || true)
        gc mail send mayor/ \
            -s "ESCALATION: Compactor integrity check failed [CRITICAL]" \
            -m "Database: $DB. Row counts diverged after flatten.
$MISMATCH" \
            2>/dev/null || true
        TOTAL_FAILED=$((TOTAL_FAILED + 1))
        integrity_ok=0
    fi

    rm -f "$pre_file" "$post_file"

    if [ "$integrity_ok" = 0 ]; then
        continue
    fi

    # Reclaim unreferenced chunks. DOLT_GC cannot run inside an
    # explicit transaction, so invoke it on a fresh session.
    dolt_sql -q "USE \`$DB\`; CALL DOLT_GC()" 2>/dev/null || true

    POST_COMMITS=$(dolt_sql -r csv -q "USE \`$DB\`; SELECT COUNT(*) FROM dolt_log" 2>/dev/null \
        | grep -E '^[0-9]+$' | head -1 || true)
    case "$POST_COMMITS" in
        ''|*[!0-9]*) POST_COMMITS=0 ;;
    esac

    TOTAL_COMPACTED=$((TOTAL_COMPACTED + 1))
    COMMITS_BEFORE_SUM=$((COMMITS_BEFORE_SUM + COMMITS))
    COMMITS_AFTER_SUM=$((COMMITS_AFTER_SUM + POST_COMMITS))

    echo "compactor: flattened $DB ($COMMITS -> $POST_COMMITS commits)"
done

SUMMARY="compactor — compacted:$TOTAL_COMPACTED, skipped:$TOTAL_SKIPPED, failed:$TOTAL_FAILED"
if [ "$TOTAL_COMPACTED" -gt 0 ] && [ "$COMMITS_BEFORE_SUM" -gt 0 ]; then
    SUMMARY="$SUMMARY, commits:$COMMITS_BEFORE_SUM->$COMMITS_AFTER_SUM"
fi
if [ -n "$DRY_RUN" ]; then
    SUMMARY="$SUMMARY (dry run)"
fi

gc nudge deacon/ "DOG_DONE: $SUMMARY" 2>/dev/null || true
echo "compactor: $SUMMARY"
