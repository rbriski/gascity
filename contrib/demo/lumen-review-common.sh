#!/usr/bin/env bash

# Pure validation helpers shared by the real-inference recorder and its tests.
# This file is sourced; it must not mutate process or filesystem state.

lumen_demo_canonical_base() {
  local candidate="${1:-}"
  local canonical
  [[ -n "$candidate" ]] || return 1
  canonical="$(realpath -m -- "$candidate")" || return 1
  case "$canonical" in
    /tmp/* | /data/tmp/*) ;;
    *) return 1 ;;
  esac
  printf '%s\n' "$canonical"
}

lumen_demo_codex_gateway_url() {
  local candidate="${1:-}"
  python3 -I - "$candidate" <<'PY'
import sys
import urllib.parse

candidate = sys.argv[1].strip()
if not candidate or any(character.isspace() for character in candidate):
    raise SystemExit("gateway URL is empty or contains whitespace")
try:
    parsed = urllib.parse.urlsplit(candidate)
    parsed.port
except ValueError as exc:
    raise SystemExit(f"invalid gateway URL: {exc}")
if (
    parsed.scheme != "https"
    or not parsed.hostname
    or parsed.username is not None
    or parsed.password is not None
    or parsed.query
    or parsed.fragment
):
    raise SystemExit("gateway URL must be HTTPS without credentials, query, or fragment")
path = parsed.path.rstrip("/")
if not path.endswith("/v1"):
    path += "/v1"
print(urllib.parse.urlunsplit((parsed.scheme, parsed.netloc, path, "", "")))
PY
}

lumen_demo_reviewers_concurrent() {
  local snapshot="$1"
  jq -ce '
    def lifecycle_active:
      (.state == "active" or .state == "awake") and .closed != true;
    [.sessions[]? |
      select((.template == "laneOneAgent" or .template == "laneTwoAgent") and
             lifecycle_active and
             ((.id // "") | length) > 0 and
             ((.session_name // "") | length) > 0 and
             ((.created_at // "") | length) > 0)] as $rows |
    ($rows | map(select(.template == "laneOneAgent")) | max_by(.created_at) // null) as $one |
    ($rows | map(select(.template == "laneTwoAgent")) | max_by(.created_at) // null) as $two |
    select($one != null and $two != null and
           $one.id != $two.id and $one.session_name != $two.session_name) |
    {laneOneAgent: $one, laneTwoAgent: $two}
  ' "$snapshot"
}

lumen_demo_phase_row() {
  local snapshot="$1"
  local template="$2"
  jq -ce --arg template "$template" '
    [.sessions[]? |
      select(.template == $template and
             (.state == "active" or .state == "awake") and .closed != true and
             ((.id // "") | length) > 0 and
             ((.session_name // "") | length) > 0 and
             ((.created_at // "") | length) > 0)] |
    max_by(.created_at) // empty
  ' "$snapshot"
}

lumen_demo_sessions_returned() {
  local snapshot="$1"
  local session_ids="$2"
  jq -e --argjson ids "$session_ids" '
    def returned:
      .closed == true or
      (.state == "asleep" or .state == "drained" or
       .state == "archived" or .state == "suspended" or .state == "closed");
    select(($ids | type) == "array" and ($ids | length) == 4 and
           ($ids | unique | length) == 4 and
           all($ids[]; type == "string" and length > 0)) |
    [.sessions[]? | select(.id as $id | $ids | index($id) != null)] as $rows |
    select(($rows | length) == 4 and
           ($rows | map(.id) | unique | length) == 4 and
           all($rows[]; returned))
  ' "$snapshot" >/dev/null
}

lumen_demo_session_projection_returned() {
  local snapshot="$1"
  local captured="$2"
  jq -e --slurpfile captured "$captured" '
    def returned:
      .closed == true or
      (.state == "asleep" or .state == "drained" or .state == "archived" or
       .state == "suspended" or .state == "closed" or .state == "failed-create");
    select(($captured | length) == 1 and
           ($captured[0] | type) == "array" and
           ($captured[0] | length) == 4) |
    ($captured[0] | map(.id)) as $ids |
    [.sessions[]?] as $rows |
    select(($rows | map(.id) | unique | length) == ($rows | length) and
           all($rows[]; .id as $id | $ids | index($id) != null) and
           all($rows[]; returned))
  ' "$snapshot" >/dev/null
}

lumen_demo_tmux_sessions_present() {
  local snapshot="$1"
  local session_names="$2"
  jq -Rse --argjson names "$session_names" '
    (split("\n") | map(select(length > 0))) as $observed |
    select(($names | type) == "array" and ($names | length) > 0 and
           ($names | unique | length) == ($names | length) and
           all($names[]; type == "string" and length > 0) and
           all($names[]; . as $name | $observed | index($name) != null))
  ' "$snapshot" >/dev/null
}

lumen_demo_tmux_sessions_absent() {
  local snapshot="$1"
  local session_names="$2"
  jq -Rse --argjson names "$session_names" '
    (split("\n") | map(select(length > 0))) as $observed |
    select(($names | type) == "array" and ($names | length) > 0 and
           ($names | unique | length) == ($names | length) and
           all($names[]; type == "string" and length > 0) and
           all($names[]; . as $name | $observed | index($name) == null))
  ' "$snapshot" >/dev/null
}

lumen_demo_session_bead_provenance() {
  local bead_json="$1"
  local session_id="$2"
  local session_name="$3"
  local template="$4"
  local provider="$5"
  lumen_demo_single_json_array "$bead_json" >/dev/null || return 1
  jq -e \
    --arg session_id "$session_id" \
    --arg session_name "$session_name" \
    --arg template "$template" \
    --arg provider "$provider" '
    type == "array" and length == 1 and
    .[0].id == $session_id and .[0].issue_type == "session" and
    .[0].metadata.template == $template and
    .[0].metadata.provider == $provider and
    .[0].metadata.session_name == $session_name and
    ((.[0].metadata.transport? // "") as $transport |
      ($transport | type) == "string" and
      ($transport == "" or $transport == "tmux"))
  ' "$bead_json" >/dev/null
}

lumen_demo_session_set() {
  local rows="$1"
  jq -e '
    select(type == "array" and length == 4) |
    [.[].template] as $templates |
    [.[].id] as $ids |
    [.[].session_name] as $names |
    select(($templates | sort) ==
             (["laneOneAgent","laneTwoAgent","synthesisAgent","verifierAgent"] | sort) and
           ($ids | all(type == "string" and length > 0)) and
           ($ids | unique | length) == 4 and
           ($names | all(type == "string" and length > 0)) and
           ($names | unique | length) == 4)
  ' "$rows" >/dev/null
}

lumen_demo_session_beads_returned() {
  local snapshot="$1"
  local captured="$2"
  lumen_demo_single_json_array "$snapshot" >/dev/null || return 1
  jq -e --slurpfile captured "$captured" '
    def returned:
      .status == "closed" or
      (.metadata.state == "asleep" or .metadata.state == "drained" or
       .metadata.state == "archived" or .metadata.state == "suspended" or
       .metadata.state == "closed" or .metadata.state == "failed-create");
    select(type == "array" and length == 4 and
           ($captured | length) == 1 and
           ($captured[0] | type) == "array" and
           ($captured[0] | length) == 4) |
    ($captured[0] | map({id, template, session_name}) | sort_by(.id)) as $want |
    (map({id, template:.metadata.template, session_name:.metadata.session_name}) |
      sort_by(.id)) as $got |
    select($got == $want and
           all(.[]; .issue_type == "session" and returned))
  ' "$snapshot" >/dev/null
}

lumen_demo_single_json_value() {
  local path="$1"
  local expected="$2"
  local mode="${3:-any}"
  [[ "$expected" == "object" || "$expected" == "array" ]] || return 1
  [[ "$mode" == "any" || "$mode" == "compact" ]] || return 1
  python3 -I - "$path" "$expected" "$mode" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
expected = sys.argv[2]
mode = sys.argv[3]

def reject_constant(value):
    raise ValueError(f"non-finite JSON number {value}")

def reject_duplicates(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON key {key!r}")
        result[key] = value
    return result

try:
    raw = path.read_text(encoding="utf-8")
    value = json.loads(
        raw,
        object_pairs_hook=reject_duplicates,
        parse_constant=reject_constant,
    )
except (OSError, UnicodeError, ValueError, json.JSONDecodeError) as exc:
    raise SystemExit(f"{path}: {exc}")
expected_type = dict if expected == "object" else list
if not isinstance(value, expected_type):
    raise SystemExit(f"{path}: expected one JSON {expected}")
compact = json.dumps(
    value,
    ensure_ascii=False,
    allow_nan=False,
    separators=(",", ":"),
)
if mode == "compact":
    payload = raw[:-1] if raw.endswith("\n") else raw
    if not payload or "\n" in payload or "\r" in payload:
        raise SystemExit(f"{path}: JSON {expected} is not compact")
    in_string = False
    escaped = False
    for character in payload:
        if in_string:
            if escaped:
                escaped = False
            elif character == "\\":
                escaped = True
            elif character == '"':
                in_string = False
        elif character == '"':
            in_string = True
        elif character.isspace():
            raise SystemExit(f"{path}: JSON {expected} is not compact")
    sys.stdout.write(payload + "\n")
else:
    sys.stdout.write(compact + "\n")
PY
}

lumen_demo_single_json_object() {
  lumen_demo_single_json_value "$1" object "${2:-any}"
}

lumen_demo_single_json_array() {
  lumen_demo_single_json_value "$1" array "${2:-any}"
}

lumen_demo_is_elf_executable() {
  local path="$1"
  local magic
  [[ -x "$path" && -f "$path" && ! -L "$path" ]] || return 1
  magic="$(LC_ALL=C head -c 4 -- "$path")" || return 1
  [[ "$magic" == $'\x7fELF' ]]
}

lumen_demo_resolve_codex_native() {
  local launcher="$1"
  local host_home="$2"
  local architecture="${3:-$(uname -m)}"
  local platform_package
  local target_triple
  local entrypoint
  local resolved
  local package_root
  local namespace_root
  local candidate
  local -a entrypoints
  local -a candidates

  launcher="$(realpath -e -- "$launcher")" || return 1
  if lumen_demo_is_elf_executable "$launcher"; then
    printf '%s\n' "$launcher"
    return 0
  fi

  case "$architecture" in
    x86_64 | amd64)
      platform_package="codex-linux-x64"
      target_triple="x86_64-unknown-linux-musl"
      ;;
    aarch64 | arm64)
      platform_package="codex-linux-arm64"
      target_triple="aarch64-unknown-linux-musl"
      ;;
    *) return 1 ;;
  esac

  entrypoints=("$launcher" "$host_home/.bun/bin/codex")
  for entrypoint in "${entrypoints[@]}"; do
    [[ -e "$entrypoint" ]] || continue
    resolved="$(realpath -e -- "$entrypoint")" || continue
    case "$resolved" in
      */@openai/codex/bin/codex.js)
        package_root="${resolved%/bin/codex.js}"
        namespace_root="${package_root%/codex}"
        candidates+=(
          "$namespace_root/$platform_package/vendor/$target_triple/bin/codex"
          "$package_root/node_modules/@openai/$platform_package/vendor/$target_triple/bin/codex"
        )
        ;;
    esac
  done

  for candidate in "${candidates[@]}"; do
    [[ -e "$candidate" ]] || continue
    resolved="$(realpath -e -- "$candidate")" || continue
    if lumen_demo_is_elf_executable "$resolved"; then
      printf '%s\n' "$resolved"
      return 0
    fi
  done
  return 1
}

lumen_demo_validate_lane() {
  local path="$1"
  local lane="$2"
  local provider="$3"
  local objective="$4"
  local document="$5"
  local repository="$6"
  local artifact="$7"
  lumen_demo_single_json_object "$path" compact >/dev/null || return 1
  jq -e \
    --arg lane "$lane" \
    --arg provider "$provider" \
    --arg objective "$objective" \
    --arg document "$document" \
    --arg repository "$repository" \
    --arg artifact "$artifact" '
      def substantive($minimum):
        if type == "string"
        then (gsub("^\\s+|\\s+$"; "") | length) >= $minimum
        else false
        end;
      .schema == "review-quorum.lane.v1" and
      .lane == $lane and .provider == $provider and
      (.verdict == "approve" or .verdict == "revise" or .verdict == "block") and
      (.summary | substantive(60)) and
      .objective == $objective and .document_path == $document and
      .repository_path == $repository and .artifact_path == $artifact and
      has("failure_class") and .failure_class == null and
      (.findings | type) == "array" and
      (.findings | length) >= 3 and (.findings | length) <= 7 and
      (([.findings[].id] | unique | length) == (.findings | length)) and
      all(.findings[];
        type == "object" and
        (.id | substantive(1)) and .id == (.id | gsub("^\\s+|\\s+$"; "")) and
        (.severity == "critical" or .severity == "high" or
         .severity == "medium" or .severity == "low") and
        (.title | substantive(10)) and
        (.evidence | type) == "array" and (.evidence | length) > 0 and
        all(.evidence[]; substantive(10)) and
        ((.evidence | join(" ") | gsub("^\\s+|\\s+$"; "")) | length) >= 20 and
        (.impact | substantive(20)) and
        (.recommendation | substantive(20)))
  ' "$path" >/dev/null
}

lumen_demo_validate_finding_coverage() {
  local lane_one="$1"
  local lane_two="$2"
  local synthesis="$3"
  jq -e -s '
    (.[0].findings + .[1].findings | map(.id)) as $reviewer_ids |
    (.[2].incorporated_findings +
      (.[2].deferred_findings | map(.id))) as $classified_ids |
    ($reviewer_ids | length) > 0 and
    all($reviewer_ids[];
      type == "string" and length > 0 and . == (gsub("^\\s+|\\s+$"; ""))) and
    all($classified_ids[];
      type == "string" and length > 0 and . == (gsub("^\\s+|\\s+$"; ""))) and
    ($reviewer_ids | length) == ($reviewer_ids | unique | length) and
    ($classified_ids | length) == ($classified_ids | unique | length) and
    ($classified_ids | sort) == ($reviewer_ids | sort)
  ' "$lane_one" "$lane_two" "$synthesis" >/dev/null
}

lumen_demo_file_matches_sha256() {
  local path="$1"
  local expected="$2"
  local actual
  [[ "$expected" =~ ^[0-9a-f]{64}$ ]] || return 1
  [[ -f "$path" && ! -L "$path" ]] || return 1
  actual="$(sha256sum -- "$path" | awk '{print $1}')" || return 1
  [[ "$actual" == "$expected" ]]
}

lumen_demo_process_executable_matches() {
  local pid="$1"
  local expected_path="$2"
  local expected_sha256="$3"
  local expected_real
  local actual_real
  local actual_sha256
  [[ "$pid" =~ ^[1-9][0-9]*$ ]] || return 1
  [[ "$expected_sha256" =~ ^[0-9a-f]{64}$ ]] || return 1
  [[ -f "$expected_path" && ! -L "$expected_path" ]] || return 1
  kill -0 "$pid" 2>/dev/null || return 1
  expected_real="$(realpath -e -- "$expected_path")" || return 1
  actual_real="$(readlink -e -- "/proc/$pid/exe")" || return 1
  [[ "$actual_real" == "$expected_real" ]] || return 1
  actual_sha256="$(sha256sum -- "/proc/$pid/exe" | awk '{print $1}')" || return 1
  [[ "$actual_sha256" == "$expected_sha256" ]]
}

lumen_demo_pid_running() {
  local pid="$1"
  local state
  [[ "$pid" =~ ^[1-9][0-9]*$ ]] || return 1
  kill -0 "$pid" 2>/dev/null || return 1
  state="$(ps -p "$pid" -o stat= 2>/dev/null | awk '{$1=$1; print}')" || return 1
  [[ -n "$state" && "$state" != Z* ]]
}

lumen_demo_wait_pid_gone() {
  local pid="$1"
  local attempts="$2"
  local delay="$3"
  local attempt
  [[ "$attempts" =~ ^[1-9][0-9]*$ ]] || return 1
  for ((attempt = 0; attempt < attempts; attempt++)); do
    if ! lumen_demo_pid_running "$pid"; then
      return 0
    fi
    sleep "$delay" || return 1
  done
  ! lumen_demo_pid_running "$pid"
}

lumen_demo_retarget_diff() {
  local source="$1"
  python3 -I - "$source" <<'PY'
import pathlib
import re
import sys

source = pathlib.Path(sys.argv[1])
text = source.read_text(encoding="utf-8").replace("\r\n", "\n")
if "\x00" in text:
    raise SystemExit("revision.diff contains a NUL byte")
lines = text.splitlines()
if (
    len(lines) < 3
    or not lines[0].startswith("--- ")
    or len(lines[0].strip()) <= 4
    or not lines[1].startswith("+++ ")
    or len(lines[1].strip()) <= 4
):
    raise SystemExit("revision.diff must begin with exactly one unified-file header")

header = re.compile(r"^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(?: .*)?$")
retargeted = ["--- document.md", "+++ document.md"]
cursor = 2
hunks = 0
while cursor < len(lines):
    match = header.match(lines[cursor])
    if match is None:
        raise SystemExit(f"unexpected content outside a hunk: {lines[cursor]!r}")
    old_count = int(match.group(2)) if match.group(2) is not None else 1
    new_count = int(match.group(4)) if match.group(4) is not None else 1
    old_seen = 0
    new_seen = 0
    retargeted.append(lines[cursor])
    cursor += 1
    hunks += 1
    last_was_data = False
    while old_seen < old_count or new_seen < new_count:
        if cursor >= len(lines):
            raise SystemExit("revision.diff ended inside a hunk")
        line = lines[cursor]
        if line == r"\ No newline at end of file":
            if not last_was_data:
                raise SystemExit("misplaced no-newline marker in revision.diff")
            retargeted.append(line)
            cursor += 1
            last_was_data = False
            continue
        if not line or line[0] not in " +-":
            raise SystemExit(f"invalid unified-diff data line: {line!r}")
        if line[0] in " -":
            old_seen += 1
        if line[0] in " +":
            new_seen += 1
        if old_seen > old_count or new_seen > new_count:
            raise SystemExit("revision.diff hunk line counts overflow its header")
        retargeted.append(line)
        cursor += 1
        last_was_data = True
    if cursor < len(lines) and lines[cursor] == r"\ No newline at end of file":
        if not last_was_data:
            raise SystemExit("misplaced no-newline marker in revision.diff")
        retargeted.append(lines[cursor])
        cursor += 1
if hunks == 0:
    raise SystemExit("revision.diff contains no hunks")
sys.stdout.write("\n".join(retargeted) + "\n")
PY
}

lumen_demo_tree_sha256() {
  local root="$1"
  python3 -I - "$root" <<'PY'
import hashlib
import os
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
if not root.is_dir():
    raise SystemExit(f"{root}: expected directory")
digest = hashlib.sha256()
paths = [root, *sorted(root.rglob("*"), key=lambda item: item.relative_to(root).as_posix())]
for path in paths:
    relative_text = "." if path == root else path.relative_to(root).as_posix()
    relative = relative_text.encode("utf-8")
    mode = (os.lstat(path).st_mode & 0o7777).to_bytes(4, "big")
    if path.is_symlink():
        kind = b"L"
        payload = os.readlink(path).encode("utf-8")
    elif path.is_file():
        kind = b"F"
        payload = hashlib.sha256(path.read_bytes()).digest()
    elif path.is_dir():
        kind = b"D"
        payload = b""
    else:
        raise SystemExit(f"{path}: unsupported filesystem entry")
    for field in (kind, mode, relative, payload):
        digest.update(len(field).to_bytes(8, "big"))
        digest.update(field)
print(digest.hexdigest())
PY
}

lumen_demo_scan_evidence_secrets() {
  local root="$1"
  shift
  python3 -I - "$root" "$@" <<'PY'
import json
import os
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1])
credential_paths = [pathlib.Path(value) for value in sys.argv[2:] if value]
if not root.is_dir():
    raise SystemExit("evidence root is not a directory")
patterns = (
    re.compile(rb"sk-(?:ant-)?[A-Za-z0-9][A-Za-z0-9_-]{15,}", re.IGNORECASE),
    re.compile(rb"Bearer\s+[A-Za-z0-9._~+/-]{16,}", re.IGNORECASE),
    re.compile(rb"eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}"),
    re.compile(
        rb'"(?:access_token|refresh_token|id_token|api_key|auth_token)"\s*:\s*"(?:\\.|[^"\\]){8,}"',
        re.IGNORECASE,
    ),
    re.compile(
        rb"(?:OPENAI_API_KEY|ANTHROPIC_API_KEY|ANTHROPIC_AUTH_TOKEN|CLAUDE_CODE_OAUTH_TOKEN)"
        rb"\s*[:=]\s*[\"']?[A-Za-z0-9._~+/-]{8,}",
        re.IGNORECASE,
    ),
)
exact_secrets = {
    value.encode("utf-8")
    for name in (
        "OPENAI_API_KEY",
        "ANTHROPIC_API_KEY",
        "ANTHROPIC_AUTH_TOKEN",
        "CLAUDE_CODE_OAUTH_TOKEN",
    )
    if len(value := os.environ.get(name, "")) >= 8
}

def collect_credential_values(value, parent_key=""):
    if isinstance(value, dict):
        for key, child in value.items():
            collect_credential_values(child, str(key).lower())
    elif isinstance(value, list):
        for child in value:
            collect_credential_values(child, parent_key)
    elif isinstance(value, str) and len(value) >= 8:
        if any(marker in parent_key for marker in ("token", "secret", "api_key", "apikey")):
            exact_secrets.add(value.encode("utf-8"))

for credential_path in credential_paths:
    try:
        collect_credential_values(json.loads(credential_path.read_text(encoding="utf-8")))
    except (OSError, UnicodeError, ValueError, json.JSONDecodeError):
        raise SystemExit("could not inspect provider credential source")

scanned = 0
matched = False
for path in sorted(root.rglob("*")):
    if path.is_symlink():
        raise SystemExit("retained evidence contains a symlink")
    if path.is_dir():
        continue
    if not path.is_file():
        raise SystemExit("retained evidence contains an unsupported filesystem entry")
    scanned += 1
    payload = path.read_bytes()
    if any(pattern.search(payload) for pattern in patterns) or any(
        secret in payload for secret in exact_secrets
    ):
        matched = True
if matched:
    raise SystemExit("potential credential material found in retained evidence")
json.dump({"scanned_files": scanned, "matches": 0}, sys.stdout, separators=(",", ":"))
sys.stdout.write("\n")
PY
}

lumen_demo_binary_commit() {
  local gc_bin="$1"
  local repo_commit="$2"
  local version_json
  local binary_commit
  version_json="$(timeout --kill-after=2s 15s "$gc_bin" version --long --json)" || return 1
  binary_commit="$(jq -er '.commit | select(type == "string" and length > 0)' <<<"$version_json")" || return 1
  [[ "$binary_commit" =~ ^[0-9a-f]{7,40}$ ]] || return 1
  if [[ "$repo_commit" != "$binary_commit"* && "$binary_commit" != "$repo_commit"* ]]; then
    return 1
  fi
  printf '%s\n' "$binary_commit"
}
