#!/bin/bash -p
# Record the real-inference Lumen design-review demo.
#
# The recording uses authenticated Claude and Codex CLIs, tmux-backed Gas City
# sessions, and the default managed bd+Dolt backend. Inference time is retained
# in full and rendered with one uniform speed multiplier.

set -euo pipefail

if ! shopt -qo privileged; then
  builtin printf 'record-lumen-review: execute this file directly so its privileged-shell guard is active\n' >&2
  exit 1
fi

if [[ -n "${BASH_ENV:-}" || -n "${ENV:-}" || -n "${LD_PRELOAD:-}" ||
  -n "${LD_LIBRARY_PATH:-}" || -n "${LD_AUDIT:-}" ]]; then
  builtin printf 'record-lumen-review: refusing shell or dynamic-loader injection\n' >&2
  exit 1
fi
if [[ -n "$(builtin compgen -A function)" ]]; then
  builtin printf 'record-lumen-review: refusing inherited shell functions\n' >&2
  exit 1
fi
unset CDPATH PYTHONHOME PYTHONPATH PYTHONSTARTUP PYTHONWARNINGS PYTHONINSPECT
unset TAR_OPTIONS RIPGREP_CONFIG_PATH GREP_OPTIONS POSIXLY_CORRECT
unset NODE_OPTIONS BUN_OPTIONS BUN_INSTALL CODEX_CI CODEX_MANAGED_BY_BUN
unset CODEX_MANAGED_PACKAGE_ROOT CODEX_THREAD_ID
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY NO_PROXY
unset http_proxy https_proxy all_proxy no_proxy
unset SSL_CERT_FILE SSL_CERT_DIR CURL_CA_BUNDLE REQUESTS_CA_BUNDLE NODE_EXTRA_CA_CERTS
unset NODE_TLS_REJECT_UNAUTHORIZED SSLKEYLOGFILE
unset ASCIINEMA_CONFIG_HOME ASCIINEMA_API_URL ASCIINEMA_SERVER_URL
export PYTHONNOUSERSITE=1
export PYTHONSAFEPATH=1
umask 077
host_path="${PATH:-}"
trusted_system_path="/usr/local/go/bin:/usr/bin:/bin"
export PATH="$trusted_system_path"

script_dir="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$script_dir/../.." && pwd)"
# shellcheck source=contrib/demo/lumen-review-common.sh
source "$script_dir/lumen-review-common.sh"

die() {
  printf 'record-lumen-review: %s\n' "$*" >&2
  exit 1
}

require_clean_tracked_tree() {
  git -C "$repo" diff --quiet -- ||
    die "tracked worktree has unstaged changes; commit before recording"
  git -C "$repo" diff --cached --quiet -- ||
    die "tracked worktree has staged changes; commit before recording"
}

gc_bin_source="${GC_BIN:-$repo/.bin/gc}"
root_base_input="${LUMEN_DEMO_ROOT:-/data/tmp/lumen-review-real}"
speed="${LUMEN_DEMO_SPEED:-15}"
run_id="$(date -u +%Y%m%dT%H%M%SZ)-${$}"
host_home="${HOME:?HOME must be set}"
host_claude_config_dir="${CLAUDE_CONFIG_DIR:-$host_home/.claude}"
host_codex_home="${CODEX_HOME:-$host_home/.codex}"
codex_gateway_input="${LUMEN_DEMO_CODEX_BASE_URL:-}"
codex_gateway_url=""
codex_gateway_secret=""
if [[ -n "$codex_gateway_input" ]]; then
  codex_gateway_url="$(lumen_demo_codex_gateway_url "$codex_gateway_input")" ||
    die "LUMEN_DEMO_CODEX_BASE_URL is not a safe HTTPS gateway URL"
  [[ -n "${OPENAI_API_KEY:-}" ]] ||
    die "LUMEN_DEMO_CODEX_BASE_URL requires OPENAI_API_KEY"
  codex_gateway_secret="$OPENAI_API_KEY"
fi
unset LUMEN_DEMO_CODEX_BASE_URL

# Git's ambient redirect/config variables can override even an explicit -C.
# Provenance must come from this repository and its ordinary tracked index.
while IFS= read -r env_name; do
  case "$env_name" in
    GIT_*) unset "$env_name" ;;
  esac
done < <(compgen -e)
export GIT_OPTIONAL_LOCKS=0

for command in "$gc_bin_source" jq tmux asciinema git python3 realpath tar \
  sha256sum patch timeout ps readlink find sed awk head zsh bash tee cp chmod \
  cmp cat date mkdir mktemp mv rm sleep sort seq uname; do
  if [[ "$command" == *"/"* ]]; then
    [[ -x "$command" ]] || die "missing executable: $command"
  else
    command -v "$command" >/dev/null || die "missing command: $command"
  fi
done
[[ "$speed" =~ ^[1-9][0-9]*$ ]] ||
  die "LUMEN_DEMO_SPEED must be a positive integer"

resolve_executable() {
  local name="$1"
  local search_path="${2:-$PATH}"
  local candidate
  candidate="$(PATH="$search_path" type -P "$name")" || return 1
  [[ -n "$candidate" ]] || return 1
  realpath -e -- "$candidate"
}

claude_bin_source="$(resolve_executable claude "$host_path")" || die "could not resolve Claude executable"
codex_launcher_source="$(resolve_executable codex "$host_path")" || die "could not resolve Codex launcher"
codex_native_candidate="${LUMEN_DEMO_CODEX_NATIVE:-$codex_launcher_source}"
codex_bin_source="$(lumen_demo_resolve_codex_native "$codex_native_candidate" "$host_home")" ||
  die "could not resolve the native Codex executable behind $codex_launcher_source"
bd_bin_source="$(resolve_executable bd "$host_path")" || die "could not resolve bd executable"
dolt_bin_source="$(resolve_executable dolt "$host_path")" || die "could not resolve Dolt executable"
rg_bin_source="$(resolve_executable rg "$host_path")" || die "could not resolve ripgrep executable"
agg_bin_source="$(resolve_executable agg "$host_path")" || die "could not resolve agg executable"
claude_bin_source_sha256="$(sha256sum -- "$claude_bin_source" | awk '{print $1}')"
codex_launcher_source_sha256="$(sha256sum -- "$codex_launcher_source" | awk '{print $1}')"
codex_bin_source_sha256="$(sha256sum -- "$codex_bin_source" | awk '{print $1}')"
bd_bin_source_sha256="$(sha256sum -- "$bd_bin_source" | awk '{print $1}')"
dolt_bin_source_sha256="$(sha256sum -- "$dolt_bin_source" | awk '{print $1}')"
rg_bin_source_sha256="$(sha256sum -- "$rg_bin_source" | awk '{print $1}')"
agg_bin_source_sha256="$(sha256sum -- "$agg_bin_source" | awk '{print $1}')"

gc_bin_source="$(realpath -e -- "$gc_bin_source")"
root_base="$(lumen_demo_canonical_base "$root_base_input")" ||
  die "LUMEN_DEMO_ROOT must resolve to a strict child of /data/tmp or /tmp"
mkdir -p "$root_base"
root="$(mktemp -d "$root_base/run-$run_id-XXXXXX")"
city="$root/city"
# The demo template leaves workspace.name and session.socket unset, so gc uses
# the isolated City directory basename for its per-City tmux socket.
city_tmux_socket="${city##*/}"
repo_snapshot="$root/repository"
init_template="$root/init-template"
gc_bin="$root/gc"
evidence="$repo/reports/lumen-demo/real-review/$run_id"
evidence_parent="$repo/reports/lumen-demo/real-review"
cast="$evidence/review-quorum.cast"
gif="$evidence/review-quorum-${speed}x.gif"
recorder_socket="recorder"
recorder_session="lumen-review"
supervisor_pid=""
driver_pid=""
runtime_stopped=0
claude_credential_link=""
codex_credential_link=""
codex_gateway_token_file=""
claude_credentials_source=""
codex_credentials_source=""
managed_dolt_pid=""

require_clean_tracked_tree
repo_commit="$(git -C "$repo" rev-parse HEAD)"
source_binary_commit="$(lumen_demo_binary_commit "$gc_bin_source" "$repo_commit")" ||
  die "GC_BIN was not built cleanly from current commit $repo_commit"
source_binary_sha256="$(sha256sum "$gc_bin_source" | awk '{print $1}')"
for tracked in \
  contrib/demo/record-lumen-review.sh \
  contrib/demo/lumen-review-common.sh \
  examples/lumen/review-quorum.lumen \
  examples/lumen/review-quorum.lumen.json \
  examples/lumen/review-quorum-live/city.toml \
  examples/lumen/review-quorum-live/pack.toml \
  examples/lumen/review-quorum-live/agents/laneOneAgent/agent.toml \
  examples/lumen/review-quorum-live/agents/laneTwoAgent/agent.toml \
  examples/lumen/review-quorum-live/agents/synthesisAgent/agent.toml \
  examples/lumen/review-quorum-live/agents/verifierAgent/agent.toml \
  examples/lumen/review-quorum-live/prompts/lumen-worker.md \
  engdocs/design/gc-reload-design.md; do
  git -C "$repo" ls-files --error-unmatch "$tracked" >/dev/null ||
    die "required demo asset is not tracked: $tracked"
done

cp -f "$gc_bin_source" "$gc_bin"
chmod 0555 "$gc_bin"
binary_commit="$(lumen_demo_binary_commit "$gc_bin" "$repo_commit")" ||
  die "staged GC_BIN does not identify current commit $repo_commit"
binary_sha256="$(sha256sum "$gc_bin" | awk '{print $1}')"
[[ "$binary_sha256" == "$source_binary_sha256" ]] ||
  die "staged GC_BIN differs from its validated source binary"
timeout --kill-after=2s 15s "$gc_bin" run --help | rg -q -- '--route' ||
  die "GC_BIN does not expose the required gc run --route flag"

mkdir -p "$repo_snapshot"
git -C "$repo" archive --format=tar "$repo_commit" | tar -xf - -C "$repo_snapshot"
chmod -R a-w "$repo_snapshot"
if [[ -n "$(find "$repo_snapshot" \( -type f -o -type d \) -perm /222 -print -quit)" ]]; then
  die "repository snapshot contains writable files"
fi
repo_snapshot_sha256="$(lumen_demo_tree_sha256 "$repo_snapshot")" ||
  die "could not hash repository snapshot"
source_design="$repo_snapshot/engdocs/design/gc-reload-design.md"
[[ -f "$source_design" && ! -L "$source_design" ]] ||
  die "committed source design is not a regular file"
source_design_sha256="$(sha256sum -- "$source_design" | awk '{print $1}')"

provider_home="$root/provider-home"
toolchain_dir="$root/toolchain"
mkdir -p \
  "$root/gc-home/.dolt" \
  "$root/runtime/tmux" \
  "$toolchain_dir" \
  "$provider_home/.claude" \
  "$provider_home/.codex" \
  "$provider_home/.config" \
  "$provider_home/.local/state"
mkdir -p -- "$evidence_parent"
[[ -d "$evidence_parent" && ! -L "$evidence_parent" ]] ||
  die "evidence parent is not a real directory"
[[ "$(realpath -e -- "$evidence_parent")" == "$evidence_parent" ]] ||
  die "evidence parent escapes through a symlink"
mkdir -- "$evidence"
chmod 0700 "$evidence"
chmod 700 "$root/runtime/tmux"

stage_tool() {
  local name="$1"
  local source="$2"
  local expected_sha256="$3"
  local destination="$toolchain_dir/$name"
  [[ -x "$source" && -f "$source" && ! -L "$source" ]] || return 1
  lumen_demo_file_matches_sha256 "$source" "$expected_sha256" || return 1
  cp -f -- "$source" "$destination" || return 1
  chmod 0555 "$destination" || return 1
  lumen_demo_file_matches_sha256 "$destination" "$expected_sha256"
}

stage_tool claude "$claude_bin_source" "$claude_bin_source_sha256" ||
  die "could not stage the pinned Claude executable"
stage_tool codex "$codex_bin_source" "$codex_bin_source_sha256" ||
  die "could not stage the pinned Codex executable"
stage_tool bd "$bd_bin_source" "$bd_bin_source_sha256" ||
  die "could not stage the pinned bd executable"
stage_tool dolt "$dolt_bin_source" "$dolt_bin_source_sha256" ||
  die "could not stage the pinned Dolt executable"
stage_tool rg "$rg_bin_source" "$rg_bin_source_sha256" ||
  die "could not stage the pinned ripgrep executable"
stage_tool agg "$agg_bin_source" "$agg_bin_source_sha256" ||
  die "could not stage the pinned agg executable"
chmod 0555 "$toolchain_dir"
jq -n \
  --arg claude_source "$claude_bin_source" \
  --arg claude_sha256 "$claude_bin_source_sha256" \
  --arg claude_staged "$toolchain_dir/claude" \
  --arg codex_launcher "$codex_launcher_source" \
  --arg codex_launcher_sha256 "$codex_launcher_source_sha256" \
  --arg codex_source "$codex_bin_source" \
  --arg codex_sha256 "$codex_bin_source_sha256" \
  --arg codex_staged "$toolchain_dir/codex" \
  --arg bd_source "$bd_bin_source" \
  --arg bd_sha256 "$bd_bin_source_sha256" \
  --arg bd_staged "$toolchain_dir/bd" \
  --arg dolt_source "$dolt_bin_source" \
  --arg dolt_sha256 "$dolt_bin_source_sha256" \
  --arg dolt_staged "$toolchain_dir/dolt" \
  --arg rg_source "$rg_bin_source" \
  --arg rg_sha256 "$rg_bin_source_sha256" \
  --arg rg_staged "$toolchain_dir/rg" \
  --arg agg_source "$agg_bin_source" \
  --arg agg_sha256 "$agg_bin_source_sha256" \
  --arg agg_staged "$toolchain_dir/agg" '
    {
      claude:{source:$claude_source,staged:$claude_staged,sha256:$claude_sha256},
      codex:{launcher:$codex_launcher,launcher_sha256:$codex_launcher_sha256,
        source:$codex_source,staged:$codex_staged,sha256:$codex_sha256},
      bd:{source:$bd_source,staged:$bd_staged,sha256:$bd_sha256},
      dolt:{source:$dolt_source,staged:$dolt_staged,sha256:$dolt_sha256},
      rg:{source:$rg_source,staged:$rg_staged,sha256:$rg_sha256},
      agg:{source:$agg_source,staged:$agg_staged,sha256:$agg_sha256}
    }
  ' >"$evidence/toolchain.json"

export GC_HOME="$root/gc-home"
export HOME="$provider_home"
export CLAUDE_CONFIG_DIR="$provider_home/.claude"
export CODEX_HOME="$provider_home/.codex"
export XDG_CONFIG_HOME="$provider_home/.config"
export XDG_STATE_HOME="$provider_home/.local/state"
export XDG_RUNTIME_DIR="$root/runtime"
export TMUX_TMPDIR="$root/runtime/tmux"
export GC_CITY_PATH="$city"
gc_bin_dir="$(dirname "$gc_bin")"
export PATH="$toolchain_dir:$gc_bin_dir:$trusted_system_path"
export TERM=xterm-256color
claude_credential_link="$CLAUDE_CONFIG_DIR/.credentials.json"
codex_credential_link="$CODEX_HOME/auth.json"

verify_pinned_tool() {
  local name="$1"
  local source="$2"
  local expected_sha256="$3"
  local staged="$toolchain_dir/$name"
  local resolved
  lumen_demo_file_matches_sha256 "$source" "$expected_sha256" || return 1
  lumen_demo_file_matches_sha256 "$staged" "$expected_sha256" || return 1
  resolved="$(resolve_executable "$name")" || return 1
  [[ "$resolved" == "$staged" ]]
}

verify_pinned_tool claude "$claude_bin_source" "$claude_bin_source_sha256" ||
  die "Claude executable did not resolve to its pinned copy"
verify_pinned_tool codex "$codex_bin_source" "$codex_bin_source_sha256" ||
  die "Codex executable did not resolve to its pinned copy"
verify_pinned_tool bd "$bd_bin_source" "$bd_bin_source_sha256" ||
  die "bd executable did not resolve to its pinned copy"
verify_pinned_tool dolt "$dolt_bin_source" "$dolt_bin_source_sha256" ||
  die "Dolt executable did not resolve to its pinned copy"
verify_pinned_tool rg "$rg_bin_source" "$rg_bin_source_sha256" ||
  die "ripgrep executable did not resolve to its pinned copy"
verify_pinned_tool agg "$agg_bin_source" "$agg_bin_source_sha256" ||
  die "agg executable did not resolve to its pinned copy"

# Production defaults are selected only after every inherited test/provider
# override is removed. Enumerating exported names also catches future members
# of these override families without weakening the explicit high-risk list.
while IFS= read -r env_name; do
  case "$env_name" in
    GC_DOLT | GC_DOLT_* | GC_BEADS | GC_BEADS_* | DOLT_* | BD_* | BEADS_* | \
      OPENAI_BASE_URL | OPENAI_API_BASE | AZURE_OPENAI_* | \
      ANTHROPIC_BASE_URL | ANTHROPIC_CUSTOM_HEADERS | ANTHROPIC_MODEL | \
      ANTHROPIC_DEFAULT_*_MODEL | CLAUDE_CODE_USE_BEDROCK | \
      CLAUDE_CODE_USE_VERTEX | CLAUDE_CODE_USE_FOUNDRY) unset "$env_name" ;;
  esac
done < <(compgen -e)
unset \
  GC_SESSION \
  GC_TMUX_SESSION \
  GC_CITY \
  GC_RIG \
  GC_DIR \
  GC_CITY_ROOT \
  GC_CITY_RUNTIME_DIR \
  GC_RIG_ROOT \
  GC_DOLT_REAL_BINARY \
  GC_DOLT_MANAGED_LOCAL \
  GC_BEADS_BD_SCRIPT \
  GC_BEADS_FORCE_FALLBACK \
  BEADS_BACKEND \
  DOLT_HOST \
  DOLT_PORT \
  DOLT_USER \
  DOLT_PASSWORD \
  BEADS_DIR \
  BEADS_CREDENTIALS_FILE
unset TMUX TMUX_PANE
export DOLT_ROOT_PATH="$GC_HOME"
export BEADS_DOLT_AUTO_START=0

provider_routing_vars=(
  OPENAI_BASE_URL
  OPENAI_API_BASE
  AZURE_OPENAI_ENDPOINT
  ANTHROPIC_BASE_URL
  ANTHROPIC_CUSTOM_HEADERS
  ANTHROPIC_MODEL
  ANTHROPIC_DEFAULT_HAIKU_MODEL
  ANTHROPIC_DEFAULT_SONNET_MODEL
  ANTHROPIC_DEFAULT_OPUS_MODEL
  CLAUDE_CODE_USE_BEDROCK
  CLAUDE_CODE_USE_VERTEX
  CLAUDE_CODE_USE_FOUNDRY
  HTTP_PROXY
  HTTPS_PROXY
  ALL_PROXY
  SSL_CERT_FILE
  SSL_CERT_DIR
  CURL_CA_BUNDLE
  REQUESTS_CA_BUNDLE
  NODE_EXTRA_CA_CERTS
  NODE_TLS_REJECT_UNAUTHORIZED
  SSLKEYLOGFILE
)
for env_name in "${provider_routing_vars[@]}"; do
  [[ ! -v "$env_name" ]] || die "provider routing override survived scrub: $env_name"
done
codex_endpoint_mode="chatgpt_oauth"
if [[ -n "$codex_gateway_url" ]]; then
  codex_endpoint_mode="explicit_openai_gateway"
fi
jq -n \
  --arg codex_endpoint_mode "$codex_endpoint_mode" \
  --arg codex_base_url "$codex_gateway_url" \
  --args '
  {
    alternate_endpoint_overrides:"scrubbed",
    variables:$ARGS.positional,
    codex:{endpoint_mode:$codex_endpoint_mode,base_url:$codex_base_url}
  }
' -- "${provider_routing_vars[@]}" >"$evidence/provider-routing.json"

for claude_state in "$provider_home/.claude.json" "$CLAUDE_CONFIG_DIR/.claude.json"; do
  jq -n \
    --arg city "$city" \
    --arg repository "$repo_snapshot" '
      {
        hasCompletedOnboarding: true,
        theme: "light",
        projects: {
          ($city): {
            allowedTools: [],
            hasCompletedProjectOnboarding: true,
            hasTrustDialogAccepted: true,
            projectOnboardingSeenCount: 1
          },
          ($repository): {
            allowedTools: [],
            hasCompletedProjectOnboarding: true,
            hasTrustDialogAccepted: true,
            projectOnboardingSeenCount: 1
          }
        }
      }
    ' >"$claude_state"
  chmod 0600 "$claude_state"
done
codex_city_key="$(python3 -I -c 'import json,sys; print(json.dumps(sys.argv[1]))' "$city")"
codex_repo_key="$(python3 -I -c 'import json,sys; print(json.dumps(sys.argv[1]))' "$repo_snapshot")"
if [[ -n "$codex_gateway_url" ]]; then
  codex_gateway_token_file="$CODEX_HOME/gateway-token"
  printf '%s' "$codex_gateway_secret" >"$codex_gateway_token_file"
  chmod 0600 "$codex_gateway_token_file"
  codex_gateway_auth_command="$(resolve_executable cat)" ||
    die "could not resolve command-backed Codex gateway auth helper"
fi
{
  if [[ -n "$codex_gateway_url" ]]; then
    codex_gateway_toml="$(python3 -I -c 'import json,sys; print(json.dumps(sys.argv[1]))' "$codex_gateway_url")"
    codex_gateway_auth_toml="$(python3 -I -c 'import json,sys; print(json.dumps(sys.argv[1]))' "$codex_gateway_auth_command")"
    codex_gateway_token_toml="$(python3 -I -c 'import json,sys; print(json.dumps(sys.argv[1]))' "$codex_gateway_token_file")"
    printf 'model_provider = "gc_lumen_gateway"\n\n'
    printf '[model_providers.gc_lumen_gateway]\n'
    printf 'name = "OpenAI-compatible Lumen demo gateway"\n'
    printf 'base_url = %s\n' "$codex_gateway_toml"
    printf 'wire_api = "responses"\n\n'
    printf '[model_providers.gc_lumen_gateway.auth]\n'
    printf 'command = %s\n' "$codex_gateway_auth_toml"
    printf 'args = [%s]\n' "$codex_gateway_token_toml"
    printf 'timeout_ms = 5000\n'
    printf 'refresh_interval_ms = 0\n\n'
  fi
  printf '[projects.%s]\ntrust_level = "trusted"\n\n' "$codex_city_key"
  printf '[projects.%s]\ntrust_level = "trusted"\n' "$codex_repo_key"
} >"$CODEX_HOME/config.toml"
chmod 0600 "$CODEX_HOME/config.toml"

for claude_state in "$provider_home/.claude.json" "$CLAUDE_CONFIG_DIR/.claude.json"; do
  jq -e --arg city "$city" --arg repository "$repo_snapshot" '
    .hasCompletedOnboarding == true and
    .projects[$city].hasCompletedProjectOnboarding == true and
    .projects[$city].hasTrustDialogAccepted == true and
    .projects[$repository].hasCompletedProjectOnboarding == true and
    .projects[$repository].hasTrustDialogAccepted == true
  ' "$claude_state" >/dev/null || die "Claude trust state was not seeded"
done
if ! rg -F -q "[projects.$codex_city_key]" "$CODEX_HOME/config.toml" ||
  ! rg -F -q "[projects.$codex_repo_key]" "$CODEX_HOME/config.toml" ||
  [[ "$(rg -F -c 'trust_level = "trusted"' "$CODEX_HOME/config.toml")" -ne 2 ]]; then
  die "Codex trust state was not seeded"
fi
jq -n \
  --arg claude_config_dir "$CLAUDE_CONFIG_DIR" \
  --arg codex_home "$CODEX_HOME" \
  --arg city "$city" \
  --arg repository "$repo_snapshot" '
  {
    claude_config_dir:$claude_config_dir,
    codex_home:$codex_home,
    trusted_projects:[$city,$repository],
    hasCompletedOnboarding:true,
    hasCompletedProjectOnboarding:true,
    hasTrustDialogAccepted:true,
    codex_trust_level:"trusted"
  }
' >"$evidence/provider-trust.json"

remove_credential_links() {
  if [[ -n "$claude_credential_link" ]]; then
    rm -f -- "$claude_credential_link"
  fi
  if [[ -n "$codex_credential_link" ]]; then
    rm -f -- "$codex_credential_link"
  fi
  if [[ -n "$codex_gateway_token_file" ]]; then
    rm -f -- "$codex_gateway_token_file"
  fi
}
trap remove_credential_links EXIT

claude_credentials_source="$host_claude_config_dir/.credentials.json"
if [[ -f "$claude_credentials_source" ]]; then
  ln -s -- "$(realpath -e -- "$claude_credentials_source")" "$claude_credential_link"
  unset ANTHROPIC_AUTH_TOKEN ANTHROPIC_API_KEY CLAUDE_CODE_OAUTH_TOKEN
elif [[ -z "${ANTHROPIC_AUTH_TOKEN:-}${ANTHROPIC_API_KEY:-}${CLAUDE_CODE_OAUTH_TOKEN:-}" ]]; then
  die "Claude auth requires an environment token or $claude_credentials_source"
else
  claude_credentials_source=""
fi
codex_credentials_source="$host_codex_home/auth.json"
if [[ -n "$codex_gateway_url" ]]; then
  codex_credentials_source=""
  unset OPENAI_API_KEY
elif [[ -f "$codex_credentials_source" ]]; then
  ln -s -- "$(realpath -e -- "$codex_credentials_source")" "$codex_credential_link"
  unset OPENAI_API_KEY
elif [[ -z "${OPENAI_API_KEY:-}" ]]; then
  die "Codex auth requires OPENAI_API_KEY or $codex_credentials_source"
else
  codex_credentials_source=""
fi

timeout --kill-after=5s 30s claude auth status </dev/null >/dev/null 2>&1 ||
  die "Claude authentication preflight failed"
codex_preflight_message="$root/codex-auth-preflight.txt"
codex_preflight_stderr="$root/codex-auth-preflight.stderr"
if ! (cd "$root" && timeout --kill-after=5s 2m codex exec \
  --dangerously-bypass-approvals-and-sandbox \
  --skip-git-repo-check \
  --color never \
  --model gpt-5.5 \
  --output-last-message "$codex_preflight_message" \
  'Reply with exactly GC_CODEX_INFERENCE_AUTH_OK and nothing else.') \
  >/dev/null 2>"$codex_preflight_stderr"; then
  sed -n '1,30p' "$codex_preflight_stderr" >&2
  die "Codex real-inference authentication preflight failed"
fi
[[ "$(<"$codex_preflight_message")" == "GC_CODEX_INFERENCE_AUTH_OK" ]] ||
  die "Codex real-inference authentication preflight returned the wrong marker"
rm -f -- "$codex_preflight_message" "$codex_preflight_stderr"
[[ "$HOME" == "$provider_home" && "$HOME" != "$host_home" ]] ||
  die "provider HOME is not isolated from the host user"
timeout --kill-after=2s 15s "$gc_bin" supervisor run --help >/dev/null ||
  die "isolated supervisor environment preflight failed"

supervisor_port="$(python3 -I -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')"
printf '[supervisor]\nport = %s\nbind = "127.0.0.1"\n' "$supervisor_port" >"$GC_HOME/supervisor.toml"
printf '%s\n' '{"user.name":"gc-demo","user.email":"gc-demo@example.invalid"}' >"$GC_HOME/.dolt/config_global.json"

stop_pid_bounded() {
  local pid="$1"
  local label="$2"
  [[ "$pid" =~ ^[1-9][0-9]*$ ]] || return 0
  if ! lumen_demo_pid_running "$pid"; then
    wait "$pid" 2>/dev/null || true
    return 0
  fi
  kill -TERM "$pid" 2>/dev/null || true
  for _ in $(seq 1 50); do
    lumen_demo_pid_running "$pid" || break
    sleep 0.1
  done
  if lumen_demo_pid_running "$pid"; then
    kill -KILL "$pid" 2>/dev/null || true
  fi
  for _ in $(seq 1 50); do
    lumen_demo_pid_running "$pid" || break
    sleep 0.1
  done
  if lumen_demo_pid_running "$pid"; then
    printf 'could not terminate %s pid %s\n' "$label" "$pid" >&2
    return 1
  fi
  wait "$pid" 2>/dev/null || true
}

wait_managed_dolt_gone() {
  [[ "$managed_dolt_pid" =~ ^[1-9][0-9]*$ ]] || return 1
  lumen_demo_wait_pid_gone "$managed_dolt_pid" 100 0.1
}

tmux_socket_absent() {
  local socket="$1"
  local output
  if output="$(timeout --kill-after=2s 5s tmux -L "$socket" list-sessions \
    -F '#{session_name}' 2>&1)"; then
    return 1
  fi
  output="${output,,}"
  [[ "$output" == *"no server running"* ||
    "$output" == *"failed to connect to server"* ||
    "$output" == *"error connecting to"* ]]
}

stop_tmux_socket_bounded() {
  local socket="$1"
  local label="$2"
  if tmux_socket_absent "$socket"; then
    return 0
  fi
  timeout --kill-after=2s 10s tmux -L "$socket" kill-server >/dev/null 2>&1 || true
  for _ in $(seq 1 50); do
    tmux_socket_absent "$socket" && return 0
    sleep 0.1
  done
  printf 'could not terminate %s tmux socket %s\n' "$label" "$socket" >&2
  return 1
}

shutdown_runtime_checked() {
  if ((runtime_stopped != 0)); then
    return 0
  fi
  if [[ -n "$driver_pid" ]]; then
    stop_pid_bounded "$driver_pid" driver || return 1
    driver_pid=""
  fi
  stop_tmux_socket_bounded "$recorder_socket" recorder || return 1
  if [[ -d "$city" ]]; then
    timeout --kill-after=5s 45s "$gc_bin" stop "$city" >/dev/null || return 1
  fi
  stop_tmux_socket_bounded "$city_tmux_socket" City || return 1
  timeout --kill-after=5s 20s "$gc_bin" supervisor stop --wait >/dev/null || return 1
  if [[ -n "$supervisor_pid" ]]; then
    stop_pid_bounded "$supervisor_pid" supervisor || return 1
    supervisor_pid=""
  fi
  wait_managed_dolt_gone || return 1
  jq -n \
    --argjson pid "$managed_dolt_pid" \
    --arg checked_at "$(date -u +%FT%TZ)" \
    '{pid:$pid,stopped:true,checked_at:$checked_at}' >"$evidence/dolt-process-stopped.json"
  remove_credential_links
  rm -rf -- "$provider_home" || return 1
  [[ ! -e "$provider_home" ]] || return 1
  runtime_stopped=1
}

cleanup() {
  local status=$?
  local cleanup_failed=0
  local evidence_scan_failed=0
  trap - EXIT
  set +e
  if ((runtime_stopped == 0)); then
    if [[ -n "$driver_pid" ]]; then
      if ! stop_pid_bounded "$driver_pid" driver; then
        cleanup_failed=1
      fi
      driver_pid=""
    fi
    if ! stop_tmux_socket_bounded "$recorder_socket" recorder; then
      cleanup_failed=1
    fi
    if [[ -d "$city" ]]; then
      if ! timeout --kill-after=5s 45s "$gc_bin" stop "$city" >/dev/null 2>&1; then
        printf 'gc stop failed during failed-run cleanup\n' >&2
        cleanup_failed=1
      fi
    fi
    if ! stop_tmux_socket_bounded "$city_tmux_socket" City; then
      cleanup_failed=1
    fi
    if ! timeout --kill-after=5s 20s "$gc_bin" supervisor stop --wait >/dev/null 2>&1; then
      printf 'supervisor stop failed during failed-run cleanup\n' >&2
      cleanup_failed=1
    fi
    if [[ -n "$supervisor_pid" ]]; then
      if ! stop_pid_bounded "$supervisor_pid" supervisor; then
        cleanup_failed=1
      fi
      supervisor_pid=""
    fi
    if [[ "$managed_dolt_pid" =~ ^[1-9][0-9]*$ ]] && ! wait_managed_dolt_gone; then
      printf 'managed Dolt pid %s remained live after failed-run cleanup\n' "$managed_dolt_pid" >&2
      if ! stop_pid_bounded "$managed_dolt_pid" "managed Dolt" || ! wait_managed_dolt_gone; then
        cleanup_failed=1
      fi
    fi
  fi
  remove_credential_links
  if ! rm -rf -- "$provider_home"; then
    printf 'could not remove isolated provider state during cleanup\n' >&2
    cleanup_failed=1
  fi
  if ((status != 0)) && [[ -d "$evidence" ]]; then
    if [[ -n "$codex_gateway_secret" ]]; then
      if ! OPENAI_API_KEY="$codex_gateway_secret" \
        lumen_demo_scan_evidence_secrets \
        "$evidence" "$claude_credentials_source" "$codex_credentials_source" \
        >/dev/null 2>&1; then
        evidence_scan_failed=1
      fi
    else
      if ! lumen_demo_scan_evidence_secrets \
        "$evidence" "$claude_credentials_source" "$codex_credentials_source" \
        >/dev/null 2>&1; then
        evidence_scan_failed=1
      fi
    fi
    if ((evidence_scan_failed != 0)); then
      printf 'removing failed-run evidence because its credential scan failed\n' >&2
      if ! rm -rf -- "$evidence"; then
        cleanup_failed=1
      fi
    fi
  fi
  if ((status == 0 && cleanup_failed != 0)); then
    status=1
  fi
  exit "$status"
}
trap cleanup EXIT

init_template_source="$repo_snapshot/examples/lumen/review-quorum-live"
init_template_source_sha256="$(lumen_demo_tree_sha256 "$init_template_source")" ||
  die "could not hash the committed init template"
cp -a -- "$repo_snapshot/examples/lumen/review-quorum-live" "$init_template"
[[ "$(lumen_demo_tree_sha256 "$init_template")" == "$init_template_source_sha256" ]] ||
  die "staged init template does not match the committed snapshot"
chmod -R u+w "$init_template"
timeout --kill-after=10s 5m "$gc_bin" init \
  --from "$init_template" \
  --skip-provider-readiness \
  --no-start \
  "$city"
cp -f "$repo_snapshot/examples/lumen/review-quorum.lumen" "$city/review-quorum.lumen"
cp -f "$repo_snapshot/examples/lumen/review-quorum.lumen.json" "$city/review-quorum.lumen.json"
mkdir -p "$city/work" "$city/review-artifacts"
cp -f "$source_design" "$city/work/design.before.md"
cp -f "$source_design" "$city/work/design.md"
chmod 0400 "$city/work/design.before.md"
chmod 0600 "$city/work/design.md"
lumen_demo_file_matches_sha256 "$city/work/design.before.md" "$source_design_sha256" ||
  die "pristine design copy does not match the committed source"
lumen_demo_file_matches_sha256 "$city/work/design.md" "$source_design_sha256" ||
  die "writable design copy does not match the committed source"
(cd "$city" && timeout --kill-after=5s 2m "$gc_bin" migrate graph-journal init)

GC_SUPERVISOR_LOG_TEE=0 "$gc_bin" supervisor run >"$evidence/supervisor.log" 2>&1 &
supervisor_pid=$!
for _ in $(seq 1 120); do
  if timeout --kill-after=2s 10s "$gc_bin" supervisor status >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
timeout --kill-after=2s 10s "$gc_bin" supervisor status >/dev/null 2>&1 ||
  die "isolated supervisor did not become ready"
timeout --kill-after=10s 5m "$gc_bin" start "$city"

beads_metadata="$city/.beads/metadata.json"
lumen_demo_single_json_object "$beads_metadata" any >/dev/null ||
  die "City beads metadata is not one JSON object"
jq -e '
  (.backend | ascii_downcase) == "dolt" and
  (.dolt_mode | ascii_downcase) == "server" and
  ((tostring | ascii_downcase | contains("doltlite")) | not)
' "$beads_metadata" >/dev/null ||
  die "City did not start on default managed bd+Dolt"
cp -f "$beads_metadata" "$evidence/beads-metadata.json"
if rg -ni 'doltlite' "$city/city.toml" "$city/pack.toml" "$beads_metadata" >/dev/null; then
  die "DoltLite appeared in the live City configuration"
fi

capture_managed_dolt_proof() {
  local label="${1:-}"
  local state_dir="$city/.gc/runtime/packs/dolt"
  local provider_state="$state_dir/dolt-provider-state.json"
  local published_state="$state_dir/dolt-state.json"
  local config_file="$state_dir/dolt-config.yaml"
  local expected_data_dir="$city/.beads/dolt"
  local process_proof="$evidence/dolt-process.txt"
  local executable_proof="$evidence/dolt-process-executable.json"
  local watchdog_proof="$evidence/dolt-watchdog-executable.json"
  local deadline=$((SECONDS + 60))
  local executable_path
  local executable_sha256
  local executable_start_ticks
  local pid
  local port
  while ((SECONDS < deadline)); do
    if [[ -f "$provider_state" && -f "$published_state" ]] &&
      lumen_demo_single_json_object "$provider_state" any >/dev/null 2>&1 &&
      lumen_demo_single_json_object "$published_state" any >/dev/null 2>&1 &&
      jq -e -s --arg data_dir "$expected_data_dir" '
        length == 2 and
        all(.[];
          .running == true and
          (.pid | type) == "number" and .pid > 0 and
          (.port | type) == "number" and .port > 0 and
          .data_dir == $data_dir) and
        .[0].pid == .[1].pid and .[0].port == .[1].port
      ' "$provider_state" "$published_state" >/dev/null 2>&1; then
      pid="$(jq -er '.pid' "$provider_state")"
      if kill -0 "$pid" 2>/dev/null; then
        cp -f "$provider_state" "$evidence/dolt-provider-state.json"
        cp -f "$published_state" "$evidence/dolt-state.json"
        if jq -e -s --arg data_dir "$expected_data_dir" '
          length == 2 and
          all(.[];
            .running == true and .pid > 0 and .port > 0 and
            .data_dir == $data_dir) and
          .[0].pid == .[1].pid and .[0].port == .[1].port
        ' "$evidence/dolt-provider-state.json" "$evidence/dolt-state.json" >/dev/null 2>&1; then
          pid="$(jq -er '.pid' "$evidence/dolt-provider-state.json")"
          if kill -0 "$pid" 2>/dev/null; then
            port="$(jq -er '.port' "$evidence/dolt-provider-state.json")"
            if ! lumen_demo_process_executable_matches \
              "$pid" "$toolchain_dir/dolt" "$dolt_bin_source_sha256"; then
              sleep 0.25
              continue
            fi
            executable_path="$(readlink -e -- "/proc/$pid/exe")" || continue
            executable_sha256="$(sha256sum -- "/proc/$pid/exe" | awk '{print $1}')" || continue
            executable_start_ticks="$(python3 -I -c '
import pathlib,sys
raw = pathlib.Path(f"/proc/{sys.argv[1]}/stat").read_text(encoding="utf-8")
tail = raw[raw.rfind(")") + 2:].split()
if len(tail) < 20:
    raise SystemExit(1)
print(int(tail[19]))
' "$pid")" || continue
            jq -n \
              --argjson pid "$pid" \
              --arg path "$executable_path" \
              --arg sha256 "$executable_sha256" \
              --argjson start_time_ticks "$executable_start_ticks" \
              '{pid:$pid,path:$path,sha256:$sha256,start_time_ticks:$start_time_ticks}' >"$executable_proof"
            if [[ -f "$config_file" ]] &&
              ps -ww -p "$pid" -o pid=,ppid=,args= >"$process_proof" &&
              python3 -I - \
                "$process_proof" "$config_file" "$pid" "$port" "$expected_data_dir" \
                "$gc_bin" "$binary_sha256" "$city" "$watchdog_proof" <<'PY'
import hashlib
import json
import os
import pathlib
import re
import socket
import sys

proof_path, config_path = map(pathlib.Path, sys.argv[1:3])
expected_pid = int(sys.argv[3])
expected_port = int(sys.argv[4])
expected_data_dir = sys.argv[5]
expected_gc = pathlib.Path(sys.argv[6]).resolve(strict=True)
expected_gc_sha256 = sys.argv[7]
expected_city = pathlib.Path(sys.argv[8]).resolve(strict=True)
watchdog_proof = pathlib.Path(sys.argv[9])

parts = proof_path.read_text(encoding="utf-8").strip().split(None, 2)
if len(parts) != 3 or int(parts[0]) != expected_pid or int(parts[1]) <= 0:
    raise SystemExit("ps proof does not identify the expected live PID and parent")
watchdog_pid = int(parts[1])
raw_argv = pathlib.Path(f"/proc/{expected_pid}/cmdline").read_bytes().split(b"\0")
argv = [os.fsdecode(value) for value in raw_argv if value]
if len(argv) < 2 or pathlib.Path(argv[0]).name != "dolt" or argv[1] != "sql-server":
    raise SystemExit("state PID is not a Dolt SQL server")
config_values = []
for index, value in enumerate(argv):
    if value == "--config" and index + 1 < len(argv):
        config_values.append(argv[index + 1])
    elif value.startswith("--config="):
        config_values.append(value.removeprefix("--config="))
if config_values != [str(config_path)]:
    raise SystemExit("Dolt SQL server does not use the City's managed config")

watchdog_exe = pathlib.Path(f"/proc/{watchdog_pid}/exe").resolve(strict=True)
if watchdog_exe != expected_gc:
    raise SystemExit("Dolt parent is not the staged gc scope watchdog")
digest = hashlib.sha256()
with pathlib.Path(f"/proc/{watchdog_pid}/exe").open("rb") as stream:
    for chunk in iter(lambda: stream.read(1024 * 1024), b""):
        digest.update(chunk)
if digest.hexdigest() != expected_gc_sha256:
    raise SystemExit("Dolt scope watchdog bytes do not match the staged gc binary")
watchdog_argv = [
    os.fsdecode(value)
    for value in pathlib.Path(f"/proc/{watchdog_pid}/cmdline").read_bytes().split(b"\0")
    if value
]
if (
    len(watchdog_argv) != 5
    or pathlib.Path(watchdog_argv[0]).resolve(strict=True) != expected_gc
    or watchdog_argv[1] != "__gc-managed-dolt-scope-watchdog"
    or pathlib.Path(watchdog_argv[2]).resolve(strict=True) != config_path.resolve(strict=True)
    or pathlib.Path(watchdog_argv[4]).resolve(strict=True) != expected_city
):
    raise SystemExit("Dolt parent argv is not the City's managed scope watchdog")
log_path = pathlib.Path(watchdog_argv[3]).resolve(strict=True)
if log_path.parent != config_path.resolve(strict=True).parent:
    raise SystemExit("Dolt scope watchdog log is outside the City's managed runtime")
watchdog_stat = pathlib.Path(f"/proc/{watchdog_pid}/stat").read_text(encoding="utf-8")
watchdog_tail = watchdog_stat[watchdog_stat.rfind(")") + 2:].split()
if len(watchdog_tail) < 20:
    raise SystemExit("Dolt scope watchdog has an invalid proc stat record")
watchdog_proof.write_text(
    json.dumps(
        {
            "pid": watchdog_pid,
            "path": str(watchdog_exe),
            "sha256": digest.hexdigest(),
            "start_time_ticks": int(watchdog_tail[19]),
            "argv": watchdog_argv,
        },
        separators=(",", ":"),
    )
    + "\n",
    encoding="utf-8",
)

config = config_path.read_text(encoding="utf-8")
port_match = re.search(r"(?m)^\s*port:\s*(\d+)\s*$", config)
data_match = re.search(r'(?m)^data_dir:\s*"([^"]+)"\s*$', config)
if port_match is None or int(port_match.group(1)) != expected_port:
    raise SystemExit("managed Dolt config port does not match runtime state")
if data_match is None or data_match.group(1) != expected_data_dir:
    raise SystemExit("managed Dolt config data_dir does not match the City")
with socket.create_connection(("127.0.0.1", expected_port), timeout=1):
    pass
PY
            then
              cp -f "$config_file" "$evidence/dolt-config.yaml"
              if [[ -e "/proc/$pid/cwd" ]]; then
                readlink -e -- "/proc/$pid/cwd" >"$evidence/dolt-process.cwd" || continue
              fi
              if rg -ni 'doltlite' \
                "$evidence/dolt-provider-state.json" \
                "$evidence/dolt-state.json" \
                "$evidence/dolt-config.yaml" \
                "$process_proof" \
                "$executable_proof" \
                "$watchdog_proof" >/dev/null; then
                printf 'DoltLite appeared in managed Dolt runtime proof\n' >&2
                return 1
              fi
              managed_dolt_pid="$pid"
              if [[ -n "$label" ]]; then
                if [[ "$label" == "final" ]]; then
                  jq -e -s '
                    length == 2 and
                    .[0].pid == .[1].pid and
                    .[0].path == .[1].path and
                    .[0].sha256 == .[1].sha256 and
                    .[0].start_time_ticks == .[1].start_time_ticks
                  ' "$evidence/dolt-process-executable.initial.json" "$executable_proof" >/dev/null ||
                    return 1
                  jq -e -s '
                    length == 2 and
                    .[0].pid == .[1].pid and
                    .[0].path == .[1].path and
                    .[0].sha256 == .[1].sha256 and
                    .[0].start_time_ticks == .[1].start_time_ticks and
                    .[0].argv == .[1].argv
                  ' "$evidence/dolt-watchdog-executable.initial.json" "$watchdog_proof" >/dev/null ||
                    return 1
                fi
                cp -f "$evidence/dolt-provider-state.json" "$evidence/dolt-provider-state.$label.json"
                cp -f "$evidence/dolt-state.json" "$evidence/dolt-state.$label.json"
                cp -f "$process_proof" "$evidence/dolt-process.$label.txt"
                cp -f "$executable_proof" "$evidence/dolt-process-executable.$label.json"
                cp -f "$watchdog_proof" "$evidence/dolt-watchdog-executable.$label.json"
                cp -f "$evidence/dolt-config.yaml" "$evidence/dolt-config.$label.yaml"
                if [[ -f "$evidence/dolt-process.cwd" ]]; then
                  cp -f "$evidence/dolt-process.cwd" "$evidence/dolt-process.$label.cwd"
                fi
              fi
              return 0
            fi
          fi
        fi
      fi
    fi
    sleep 0.25
  done
  printf 'managed Dolt state was not live and consistent within 60s\n' >&2
  return 1
}

capture_managed_dolt_proof initial ||
  die "City did not publish matching live managed-Dolt state"

input="$(jq -cn \
  --arg document_path "$city/work/design.md" \
  --arg repository_path "$repo_snapshot" \
  --arg artifact_dir "$city/review-artifacts" \
  --arg objective "Make the gc reload design implementation-ready" \
  --arg lane_one_id "implementation-realism" \
  --arg lane_two_id "test-operability" \
  '{document_path:$document_path,repository_path:$repository_path,artifact_dir:$artifact_dir,objective:$objective,lane_one_id:$lane_one_id,lane_two_id:$lane_two_id}')"
printf -v quoted_input '%q' "$input"
printf -v quoted_gc '%q' "$gc_bin"

tmux -L "$recorder_socket" new-session -d -s "$recorder_session" -x 160 -y 44 -c "$city" zsh -f -i
run_pane="$(tmux -L "$recorder_socket" display-message -p -t "$recorder_session:" '#{pane_id}')"
list_pane="$(tmux -L "$recorder_socket" split-window -h -l 43% -t "$run_pane" -c "$city" -P -F '#{pane_id}' zsh -f -i)"
peek_pane="$(tmux -L "$recorder_socket" split-window -v -l 54% -t "$list_pane" -c "$city" -P -F '#{pane_id}' zsh -f -i)"

tmux -L "$recorder_socket" set-option -t "$recorder_session" status off
tmux -L "$recorder_socket" set-option -t "$recorder_session" pane-border-status top
tmux -L "$recorder_socket" set-option -t "$recorder_session" pane-border-format ' #{pane_title} '
tmux -L "$recorder_socket" select-pane -t "$run_pane" -T 'gc run · real inference'
tmux -L "$recorder_socket" select-pane -t "$list_pane" -T 'gc session list'
tmux -L "$recorder_socket" select-pane -t "$peek_pane" -T 'gc session peek'

send_command() {
  local pane="$1"
  local command="$2"
  tmux -L "$recorder_socket" send-keys -t "$pane" -l "$command"
  tmux -L "$recorder_socket" send-keys -t "$pane" Enter
}

for pane in "$run_pane" "$list_pane" "$peek_pane"; do
  send_command "$pane" "export PS1='$ '; clear"
done

session_snapshot_to() {
  local destination="$1"
  local tmp
  local err_file
  tmp="$(mktemp "$evidence/.sessions.XXXXXX")"
  err_file="$tmp.err"
  if ! (cd "$city" && timeout --kill-after=2s 15s "$gc_bin" session list --state all --json) \
    >"$tmp" 2>"$err_file"; then
    {
      printf '[%s] session list failed\n' "$(date -u +%FT%TZ)"
      sed -n '1,20p' "$err_file"
    } >>"$evidence/session-observer-errors.log"
    rm -f "$tmp" "$err_file"
    return 1
  fi
  if ! lumen_demo_single_json_object "$tmp" any >/dev/null 2>&1 ||
    ! jq -e '.sessions | type == "array"' "$tmp" >/dev/null 2>&1; then
    {
      printf '[%s] session list returned invalid JSON\n' "$(date -u +%FT%TZ)"
      sed -n '1,20p' "$tmp"
    } >>"$evidence/session-observer-errors.log"
    rm -f "$tmp" "$err_file"
    return 1
  fi
  mv -f "$tmp" "$destination"
  rm -f "$err_file"
}

session_bead_snapshot_to() {
  local destination="$1"
  local tmp
  local err_file
  tmp="$(mktemp "$evidence/.session-beads.XXXXXX")"
  err_file="$tmp.err"
  if ! (cd "$city" && timeout --kill-after=2s 30s "$gc_bin" bd list \
    --all --include-infra --json --limit=0 --type=session) >"$tmp" 2>"$err_file"; then
    {
      printf '[%s] session bead list failed\n' "$(date -u +%FT%TZ)"
      sed -n '1,20p' "$err_file"
    } >>"$evidence/session-observer-errors.log"
    rm -f "$tmp" "$err_file"
    return 1
  fi
  if ! lumen_demo_single_json_array "$tmp" >/dev/null 2>&1; then
    {
      printf '[%s] session bead list returned invalid JSON\n' "$(date -u +%FT%TZ)"
      sed -n '1,20p' "$tmp"
    } >>"$evidence/session-observer-errors.log"
    rm -f "$tmp" "$err_file"
    return 1
  fi
  mv -f "$tmp" "$destination"
  rm -f "$err_file"
}

tmux_snapshot_to() {
  local destination="$1"
  local tmp
  local err_file
  local error_text
  tmp="$(mktemp "$evidence/.tmux-sessions.XXXXXX")"
  err_file="$tmp.err"
  rm -f -- "$destination.stderr"
  if timeout --kill-after=2s 10s tmux -L "$city_tmux_socket" list-sessions \
    -F '#{session_name}' >"$tmp" 2>"$err_file"; then
    rm -f "$err_file"
  else
    error_text="$(<"$err_file")"
    error_text="${error_text,,}"
    if [[ "$error_text" == *"no server running"* ||
      "$error_text" == *"failed to connect to server"* ||
      "$error_text" == *"error connecting to"* ]]; then
      : >"$tmp"
      mv -f "$err_file" "$destination.stderr"
    else
      {
        printf '[%s] City tmux session list failed\n' "$(date -u +%FT%TZ)"
        sed -n '1,20p' "$err_file"
      } >>"$evidence/session-observer-errors.log"
      rm -f "$tmp" "$err_file"
      return 1
    fi
  fi
  mv -f "$tmp" "$destination"
}

run_output() {
  timeout --kill-after=2s 10s tmux -L "$recorder_socket" capture-pane -p -S -400 -t "$run_pane"
}

run_has_terminal() {
  local output
  output="$(run_output)" || return 1
  [[ "$output" == *"outcome: pass"* || "$output" == *"outcome: fail"* ]]
}

wait_reviewers_concurrent() {
  local deadline=$((SECONDS + 1800))
  local latest="$evidence/sessions-latest.json"
  local tmux_snapshot="$evidence/tmux-sessions-reviewers.txt"
  local rows
  local session_names
  while ((SECONDS < deadline)); do
    if session_snapshot_to "$latest"; then
      if rows="$(lumen_demo_reviewers_concurrent "$latest")"; then
        session_names="$(jq -c '[.laneOneAgent.session_name,.laneTwoAgent.session_name]' <<<"$rows")"
        if tmux_snapshot_to "$tmux_snapshot" &&
          lumen_demo_tmux_sessions_present "$tmux_snapshot" "$session_names"; then
          cp -f "$latest" "$evidence/sessions-reviewers.json"
          printf '%s\n' "$rows" >"$evidence/reviewer-session-rows.json"
          return 0
        fi
      fi
    fi
    if run_has_terminal; then
      printf 'gc run became terminal before concurrent reviewers were observed\n' >&2
      return 1
    fi
    sleep 0.25
  done
  printf 'two reviewers were not concurrently observable within 30m\n' >&2
  return 1
}

wait_phase_seen() {
  local template="$1"
  local snapshot_path="$2"
  local row_path="$3"
  local tmux_snapshot_path="$4"
  local deadline=$((SECONDS + 1800))
  local latest="$evidence/sessions-latest.json"
  local row
  local session_names
  while ((SECONDS < deadline)); do
    if session_snapshot_to "$latest"; then
      if row="$(lumen_demo_phase_row "$latest" "$template")"; then
        session_names="$(jq -c '[.session_name]' <<<"$row")"
        if tmux_snapshot_to "$tmux_snapshot_path" &&
          lumen_demo_tmux_sessions_present "$tmux_snapshot_path" "$session_names"; then
          cp -f "$latest" "$snapshot_path"
          printf '%s\n' "$row" >"$row_path"
          return 0
        fi
      fi
    fi
    if run_has_terminal; then
      printf 'gc run became terminal before %s appeared in session history\n' "$template" >&2
      return 1
    fi
    sleep 0.25
  done
  printf '%s was not observable within 30m\n' "$template" >&2
  return 1
}

wait_run_terminal() {
  local deadline=$((SECONDS + 1800))
  local output=""
  while ((SECONDS < deadline)); do
    if ! output="$(run_output)"; then
      printf '[%s] timed out capturing gc run pane\n' "$(date -u +%FT%TZ)" \
        >>"$evidence/session-observer-errors.log"
      sleep 0.5
      continue
    fi
    if [[ "$output" == *"outcome: pass"* || "$output" == *"outcome: fail"* ]]; then
      printf '%s\n' "$output" >"$evidence/run.stdout"
      [[ "$output" == *"outcome: pass"* ]]
      return
    fi
    sleep 0.5
  done
  printf '%s\n' "$output" >"$evidence/run.stdout"
  printf 'gc run did not become terminal within 30m\n' >&2
  return 1
}

wait_sessions_returned() {
  local session_set="$evidence/session-set.json"
  local session_ids
  local deadline=$((SECONDS + 300))
  local stable=0
  local latest="$evidence/session-beads-latest.json"
  local tmux_snapshot="$evidence/tmux-sessions-final.txt"
  local session_names
  lumen_demo_session_set "$session_set" ||
    die "captured workflow sessions are not four distinct identities"
  session_ids="$(jq -c '[.[].id]' "$session_set")"
  session_names="$(jq -c '[.[].session_name]' "$session_set")"
  printf '%s\n' "$session_ids" >"$evidence/session-ids.json"
  while ((SECONDS < deadline)); do
    if session_bead_snapshot_to "$latest" &&
      lumen_demo_session_beads_returned "$latest" "$session_set" &&
      tmux_snapshot_to "$tmux_snapshot" &&
      lumen_demo_tmux_sessions_absent "$tmux_snapshot" "$session_names"; then
      stable=$((stable + 1))
      if ((stable >= 3)); then
        cp -f "$latest" "$evidence/session-beads-final.json"
        session_snapshot_to "$evidence/sessions-final.json" || return 1
        return 0
      fi
    else
      stable=0
    fi
    sleep 1
  done
  printf 'workflow sessions did not remain returned for three snapshots within 5m\n' >&2
  return 1
}

validate_displayed_session_list() {
  local label="$1"
  local display_file="$2"
  local displayed
  local expected_row
  case "$label" in
    reviewers)
      displayed="$(lumen_demo_reviewers_concurrent "$display_file")" || return 1
      jq -e -n \
        --argjson displayed "$displayed" \
        --slurpfile expected "$evidence/reviewer-session-rows.json" '
        ($expected | length) == 1 and
        $displayed.laneOneAgent.id == $expected[0].laneOneAgent.id and
        $displayed.laneOneAgent.session_name == $expected[0].laneOneAgent.session_name and
        $displayed.laneTwoAgent.id == $expected[0].laneTwoAgent.id and
        $displayed.laneTwoAgent.session_name == $expected[0].laneTwoAgent.session_name
      ' >/dev/null
      ;;
    synthesis | verifier)
      if [[ "$label" == "synthesis" ]]; then
        displayed="$(lumen_demo_phase_row "$display_file" synthesisAgent)" || return 1
        expected_row="$evidence/synthesis-session-row.json"
      else
        displayed="$(lumen_demo_phase_row "$display_file" verifierAgent)" || return 1
        expected_row="$evidence/verifier-session-row.json"
      fi
      jq -e -n \
        --argjson displayed "$displayed" \
        --slurpfile expected "$expected_row" '
        ($expected | length) == 1 and
        $displayed.id == $expected[0].id and
        $displayed.session_name == $expected[0].session_name
      ' >/dev/null
      ;;
    returned)
      lumen_demo_session_projection_returned \
        "$display_file" "$evidence/session-set.json"
      ;;
    *) return 1 ;;
  esac
}

show_list() {
  local label="$1"
  local display_file="$evidence/display-session-list-$label.json"
  local quoted_display_file
  local quoted_display_line
  local deadline=$((SECONDS + 30))
  rm -f -- "$display_file"
  printf -v quoted_display_file '%q' "$display_file"
  printf -v quoted_display_line '%q' '$ gc session list --state all --json'
  send_command "$list_pane" "clear; printf '%s\n' $quoted_display_line; timeout --kill-after=2s 15s $quoted_gc session list --state all --json | tee $quoted_display_file | jq -r 'if (.sessions|length)==0 then \"(no live sessions)\" else .sessions[] | [.template,.state,.session_name] | @tsv end'"
  while ((SECONDS < deadline)); do
    if [[ -f "$display_file" ]] &&
      lumen_demo_single_json_object "$display_file" any >/dev/null 2>&1 &&
      validate_displayed_session_list "$label" "$display_file" >/dev/null 2>&1; then
      sleep 2
      return 0
    fi
    sleep 0.25
  done
  printf 'displayed session list did not produce valid non-empty JSON within 30s\n' >&2
  return 1
}

show_peek() {
  local template="$1"
  local row_file="$2"
  local session_id
  local quoted_id
  local peek_file="$evidence/peek-$template.json"
  local display_file="$evidence/peek-display-$template.json"
  local quoted_display_file
  local quoted_display_line
  local tmp
  local err_file
  local deadline=$((SECONDS + 120))
  session_id="$(jq -er '.id' "$row_file")"
  printf -v quoted_id '%q' "$session_id"
  while ((SECONDS < deadline)); do
    tmp="$(mktemp "$evidence/.peek-$template.XXXXXX")"
    err_file="$tmp.err"
    if (cd "$city" && timeout --kill-after=2s 15s "$gc_bin" session peek "$session_id" --lines 80 --json) \
      >"$tmp" 2>"$err_file" &&
      lumen_demo_single_json_object "$tmp" compact >/dev/null 2>&1 &&
      jq -e --arg session_id "$session_id" '
        .schema_version == "1" and
        .session_id == $session_id and .target == $session_id and
        .lines == 80 and .line_count > 0 and
        (.output | type) == "string" and (.output | length) > 0
    ' "$tmp" >/dev/null 2>&1; then
      mv -f "$tmp" "$peek_file"
      rm -f "$err_file"
      break
    fi
    {
      printf '[%s] checked peek for %s was not ready\n' "$(date -u +%FT%TZ)" "$template"
      sed -n '1,20p' "$err_file"
    } >>"$evidence/session-observer-errors.log"
    rm -f "$tmp" "$err_file"
    sleep 0.25
  done
  if [[ ! -f "$peek_file" ]]; then
    printf 'checked non-empty session peek for %s was not available within 2m\n' "$template" >&2
    return 1
  fi

  rm -f -- "$display_file"
  printf -v quoted_display_file '%q' "$display_file"
  printf -v quoted_display_line '%q' "$ gc session peek $session_id --lines 80 --json"
  send_command "$peek_pane" "clear; printf '%s\n' $quoted_display_line; timeout --kill-after=2s 15s $quoted_gc session peek $quoted_id --lines 80 --json | tee $quoted_display_file | jq -r .output"
  deadline=$((SECONDS + 30))
  while ((SECONDS < deadline)); do
    if [[ -f "$display_file" ]] &&
      lumen_demo_single_json_object "$display_file" compact >/dev/null 2>&1 &&
      jq -e --arg session_id "$session_id" '
        .schema_version == "1" and
        .session_id == $session_id and .target == $session_id and
        .lines == 80 and .line_count > 0 and
        (.output | type) == "string" and (.output | length) > 0
      ' "$display_file" >/dev/null 2>&1; then
      sleep 5
      return 0
    fi
    sleep 0.25
  done
  printf 'displayed session peek for %s did not produce the validated transcript within 30s\n' "$template" >&2
  return 1
}

drive_demo() {
  trap 'tmux -L "$recorder_socket" detach-client -s "$recorder_session" 2>/dev/null || true' EXIT
  wait_recording_attached
  send_command "$run_pane" "printf '\nReal Claude/Codex inference · default managed bd+Dolt · ${speed}x continuous time-lapse\n\n'"
  send_command "$list_pane" "echo 'Waiting for one snapshot with two concurrent reviewer Agents...'"
  send_command "$peek_pane" "echo 'Live provider transcripts will appear here.'"
  sleep 3

  send_command "$run_pane" "$quoted_gc run review-quorum.lumen --route synthesisAgent --input $quoted_input"

  wait_reviewers_concurrent
  jq -c '.laneOneAgent' "$evidence/reviewer-session-rows.json" >"$evidence/lane-one-session-row.json"
  jq -c '.laneTwoAgent' "$evidence/reviewer-session-rows.json" >"$evidence/lane-two-session-row.json"
  show_list reviewers
  show_peek laneOneAgent "$evidence/lane-one-session-row.json"
  show_peek laneTwoAgent "$evidence/lane-two-session-row.json"

  wait_phase_seen \
    synthesisAgent \
    "$evidence/sessions-synthesis.json" \
    "$evidence/synthesis-session-row.json" \
    "$evidence/tmux-sessions-synthesis.txt"
  show_list synthesis
  show_peek synthesisAgent "$evidence/synthesis-session-row.json"

  wait_phase_seen \
    verifierAgent \
    "$evidence/sessions-verifier.json" \
    "$evidence/verifier-session-row.json" \
    "$evidence/tmux-sessions-verifier.txt"
  show_list verifier
  show_peek verifierAgent "$evidence/verifier-session-row.json"

  jq -cs '.' \
    "$evidence/lane-one-session-row.json" \
    "$evidence/lane-two-session-row.json" \
    "$evidence/synthesis-session-row.json" \
    "$evidence/verifier-session-row.json" >"$evidence/session-set.json"
  lumen_demo_session_set "$evidence/session-set.json" ||
    die "workflow did not use four distinct session IDs, names, and templates"

  wait_run_terminal
  send_command "$run_pane" "echo; echo '$ jq review summaries'; jq '{lane,provider,verdict,summary,findings:(.findings|length)}' review-artifacts/lane-one.json review-artifacts/lane-two.json"
  sleep 7
  send_command "$run_pane" "echo; echo '$ jq verification'; jq '{provider,verdict,checks,summary}' review-artifacts/verification.json"
  sleep 6
  send_command "$run_pane" "echo; echo '$ git diff --no-index --stat work/design.before.md work/design.md'; git diff --no-index --stat work/design.before.md work/design.md || true"
  sleep 5

  wait_sessions_returned
  show_list returned
  send_command "$peek_pane" "clear; echo 'PASS: real reviews revised the document; all four workflow Agents returned.'"
  sleep 8
}

wait_recording_attached() {
  local deadline=$((SECONDS + 30))
  local clients
  while ((SECONDS < deadline)); do
    clients="$(timeout --kill-after=2s 5s tmux -L "$recorder_socket" list-clients -F '#{client_session}' 2>/dev/null || true)"
    if rg -Fx -q "$recorder_session" <<<"$clients"; then
      return 0
    fi
    sleep 0.1
  done
  printf 'asciinema did not attach to the demo tmux session within 30s\n' >&2
  return 1
}

validate_artifacts() {
  local artifacts="$city/review-artifacts"
  local lane_one="$artifacts/lane-one.json"
  local lane_two="$artifacts/lane-two.json"
  local synthesis="$artifacts/synthesis.json"
  local verification="$artifacts/verification.json"
  local retargeted_diff="$evidence/revision-retargeted.diff"
  local apply_dir
  local applied_document
  local -a applied_entries
  local changed_lines
  local json_artifact

  for json_artifact in "$lane_one" "$lane_two" "$synthesis" "$verification"; do
    lumen_demo_single_json_object "$json_artifact" compact >/dev/null ||
      die "artifact is not exactly one compact JSON object: $json_artifact"
  done

  lumen_demo_validate_lane \
    "$lane_one" implementation-realism claude \
    "Make the gc reload design implementation-ready" \
    "$city/work/design.md" "$repo_snapshot" "$lane_one"
  lumen_demo_validate_lane \
    "$lane_two" test-operability codex \
    "Make the gc reload design implementation-ready" \
    "$city/work/design.md" "$repo_snapshot" "$lane_two"
  jq -e \
    --arg objective "Make the gc reload design implementation-ready" \
    --arg document "$city/work/design.md" \
    --arg lane_one "$lane_one" \
    --arg lane_two "$lane_two" \
    --arg original "$artifacts/original.md" \
    --arg diff "$artifacts/revision.diff" \
    --arg report "$artifacts/synthesis-report.md" \
    --arg synthesis "$synthesis" '
      def substantive($minimum):
        if type == "string"
        then (gsub("^\\s+|\\s+$"; "") | length) >= $minimum
        else false
        end;
      .schema == "review-quorum.synthesis.v1" and .role == "synthesis" and
      .provider == "claude" and .verdict == "revised" and
      (.summary | substantive(60)) and .objective == $objective and
      (.source_reviews | sort) == ([$lane_one,$lane_two] | sort) and
      (.incorporated_findings | type) == "array" and
      (.incorporated_findings | length) > 0 and
      all(.incorporated_findings[]; substantive(1)) and
      (.changed_sections | type) == "array" and
      (.changed_sections | length) > 0 and
      all(.changed_sections[]; substantive(3)) and
      (.deferred_findings | type) == "array" and
      all(.deferred_findings[];
        type == "object" and
        (.id | substantive(1)) and (.reason | substantive(20))) and
      .artifacts.document == $document and .artifacts.original == $original and
      .artifacts.diff == $diff and .artifacts.report == $report and
      .artifacts.synthesis == $synthesis and
      has("failure_class") and .failure_class == null
    ' "$synthesis" >/dev/null
  lumen_demo_validate_finding_coverage "$lane_one" "$lane_two" "$synthesis"
  python3 -I - "$artifacts/synthesis-report.md" <<'PY'
import pathlib
import re
import sys

report = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8")
if len(report.encode("utf-8")) < 500 or len(re.findall(r"\S+", report)) < 80:
    raise SystemExit("synthesis report is not substantive")
PY

  jq -e \
    --arg document "$city/work/design.md" \
    --arg diff "$artifacts/revision.diff" \
    --arg verification "$verification" '
      def substantive($minimum):
        if type == "string"
        then (gsub("^\\s+|\\s+$"; "") | length) >= $minimum
        else false
        end;
      .schema == "review-quorum.verification.v1" and
      .role == "verification" and .provider == "codex" and
      .verdict == "pass" and (.summary | substantive(60)) and
      (.checks | type) == "object" and
      (.checks | keys) == ([
        "lane_artifacts_valid",
        "synthesis_valid",
        "propagated_output_matches",
        "report_substantive",
        "document_changed",
        "revision_meaningful"
      ] | sort) and
      all(.checks[]; . == true) and
      (.evidence | type) == "array" and (.evidence | length) >= 3 and
      all(.evidence[]; substantive(20)) and
      .artifacts.verification == $verification and
      .artifacts.document == $document and .artifacts.diff == $diff and
      has("failure_class") and .failure_class == null
    ' "$verification" >/dev/null

  lumen_demo_file_matches_sha256 "$source_design" "$source_design_sha256" ||
    die "read-only repository source design changed during inference"
  lumen_demo_file_matches_sha256 "$city/work/design.before.md" "$source_design_sha256" ||
    die "pristine design baseline no longer matches the committed source"
  lumen_demo_file_matches_sha256 "$artifacts/original.md" "$source_design_sha256" ||
    die "synthesis original.md does not match the committed source design"
  cmp -s "$source_design" "$city/work/design.before.md" ||
    die "pristine design baseline differs from the repository snapshot"
  cmp -s "$source_design" "$artifacts/original.md" ||
    die "synthesis original.md differs from the repository snapshot"
  cp -f "$artifacts/original.md" "$evidence/original.md"
  apply_dir="$(mktemp -d "$root/revision-apply.XXXXXX")"
  applied_document="$apply_dir/document.md"
  cp -f "$artifacts/original.md" "$applied_document"
  lumen_demo_retarget_diff "$artifacts/revision.diff" >"$retargeted_diff" ||
    die "revision.diff is not one well-formed unified-file patch"
  (cd "$apply_dir" &&
    patch --batch --silent --forward --no-backup-if-mismatch \
      document.md <"$retargeted_diff") ||
    die "revision.diff cannot be applied cleanly to original.md"
  mapfile -t applied_entries < <(find "$apply_dir" -mindepth 1 -printf '%P\n' | sort)
  [[ "${#applied_entries[@]}" -eq 1 && "${applied_entries[0]}" == "document.md" ]] ||
    die "revision.diff targeted files other than the isolated document"
  cmp -s "$applied_document" "$city/work/design.md" ||
    die "applying revision.diff did not reproduce the revised document"
  cp -f "$applied_document" "$evidence/revision-applied.md"
  rm -rf -- "$apply_dir"
  python3 -I - "$artifacts/original.md" "$city/work/design.md" <<'PY'
import pathlib
import sys

original = pathlib.Path(sys.argv[1]).read_text()
revised = pathlib.Path(sys.argv[2]).read_text()
if original == revised:
    raise SystemExit("revised document is byte-identical to original")
if original.split() == revised.split():
    raise SystemExit("revised document changed only whitespace")
if len(revised) <= len(original) // 2:
    raise SystemExit("revised document discarded more than half the original")
PY
  changed_lines="$(awk '
    /^\+\+\+|^---/ { next }
    /^[+-]/ {
      line=substr($0,2)
      gsub(/[[:space:]]/, "", line)
      if (length(line) > 0) count++
    }
    END { print count+0 }
  ' "$retargeted_diff")"
  [[ "$changed_lines" -ge 8 ]] ||
    die "revision changed only $changed_lines substantive lines; expected at least 8"
}

validate_journal_and_outputs() {
  local stream_id="$1"
  local journal="$city/.gc/graph/journal.db"
  local proof="$evidence/journal-validation.json"
  local activation
  local artifact
  local bead_id
  local safe
  local bead_json
  local stamped
  local compact
  mkdir -p "$evidence/work-beads"
  python3 -I - "$journal" "$stream_id" >"$proof" <<'PY'
import json
import sqlite3
import sys

journal, stream_id = sys.argv[1:]
db = sqlite3.connect(f"file:{journal}?mode=ro", uri=True)
rows = db.execute(
    "SELECT seq, type, payload FROM journal WHERE stream_id = ? ORDER BY seq",
    (stream_id,),
).fetchall()
if not rows:
    raise SystemExit("journal stream is empty")
events = [(seq, typ, json.loads(payload)) for seq, typ, payload in rows]
if any(not isinstance(payload, dict) for _, _, payload in events):
    raise SystemExit("journal contains a non-object payload")

work = ("reviewLaneOne:0", "reviewLaneTwo:0", "synthesize:0", "verify:0")
outcomes = work[:2] + ("lanes:0",) + work[2:]
admission_events = [
    (seq, payload)
    for seq, typ, payload in events
    if typ == "lumen.owned.admitted"
]
settlement_events = [
    (seq, payload)
    for seq, typ, payload in events
    if typ == "lumen.outcome.settled"
]
if len(admission_events) != len(work):
    raise SystemExit(f"owned.admitted count is {len(admission_events)}, expected four")
if len(settlement_events) != len(outcomes):
    raise SystemExit(f"outcome.settled count is {len(settlement_events)}, expected five")

admitted = {}
for seq, payload in admission_events:
    activation = payload.get("activation")
    if activation not in work or activation in admitted:
        raise SystemExit(f"duplicate or unexpected work admission {activation!r}")
    if payload.get("kind") != "work_bead":
        raise SystemExit(f"admission {activation} is not kind=work_bead")
    bead_id = payload.get("bead_id")
    if not isinstance(bead_id, str) or not bead_id.strip():
        raise SystemExit(f"admission {activation} has no work bead id")
    if payload.get("handle") != activation:
        raise SystemExit(f"admission {activation} has a mismatched handle")
    admitted[activation] = {"seq": seq, "bead_id": bead_id, "kind": "work_bead"}
if len({entry["bead_id"] for entry in admitted.values()}) != len(work):
    raise SystemExit("work admissions did not mint four distinct bead ids")

settled = {}
for seq, payload in settlement_events:
    activation = payload.get("activation")
    if activation not in outcomes or activation in settled:
        raise SystemExit(f"duplicate or unexpected settlement {activation!r}")
    if payload.get("outcome") != "pass":
        raise SystemExit(f"settlement {activation} is not pass")
    settled[activation] = {"seq": seq, "outcome": "pass"}
if set(admitted) != set(work) or set(settled) != set(outcomes):
    raise SystemExit("journal activation set does not match the review workflow")

for activation in work:
    if admitted[activation]["seq"] >= settled[activation]["seq"]:
        raise SystemExit(f"{activation} settled before its work admission")
review_settles = [settled["reviewLaneOne:0"]["seq"], settled["reviewLaneTwo:0"]["seq"]]
lanes_settled = settled["lanes:0"]["seq"]
if lanes_settled <= max(review_settles):
    raise SystemExit("lanes:0 settled before both reviewers settled")
if admitted["synthesize:0"]["seq"] <= lanes_settled:
    raise SystemExit("synthesis was admitted before lanes:0 settled")
if admitted["verify:0"]["seq"] <= settled["synthesize:0"]["seq"]:
    raise SystemExit("verification was admitted before synthesis settled")

closed = [(seq, payload) for seq, typ, payload in events if typ == "lumen.run.closed"]
if len(closed) != 1 or closed[0][1].get("outcome") != "pass":
    raise SystemExit("journal has no unique terminal pass")
if closed[0][0] <= settled["verify:0"]["seq"]:
    raise SystemExit("run closed before verification settled")
if events[-1][0] != closed[0][0] or events[-1][1] != "lumen.run.closed":
    raise SystemExit("the unique run.closed pass is not the journal's final event")
json.dump(
    {
        "stream_id": stream_id,
        "event_count": len(events),
        "beads": {key: admitted[key]["bead_id"] for key in work},
        "ordering": {
            "reviewer_settled": review_settles,
            "lanes_settled": lanes_settled,
            "synthesis_admitted": admitted["synthesize:0"]["seq"],
            "synthesis_settled": settled["synthesize:0"]["seq"],
            "verification_admitted": admitted["verify:0"]["seq"],
            "verification_settled": settled["verify:0"]["seq"],
            "run_closed": closed[0][0],
        },
        "outcome": "pass",
    },
    sys.stdout,
    indent=2,
    sort_keys=True,
)
sys.stdout.write("\n")
PY

  for activation in reviewLaneOne:0 reviewLaneTwo:0 synthesize:0 verify:0; do
    case "$activation" in
      reviewLaneOne:0) artifact="$city/review-artifacts/lane-one.json" ;;
      reviewLaneTwo:0) artifact="$city/review-artifacts/lane-two.json" ;;
      synthesize:0) artifact="$city/review-artifacts/synthesis.json" ;;
      verify:0) artifact="$city/review-artifacts/verification.json" ;;
    esac
    bead_id="$(jq -er --arg activation "$activation" '.beads[$activation]' "$proof")"
    safe="${activation//:/-}"
    bead_json="$evidence/work-beads/$safe.json"
    (cd "$city" && timeout --kill-after=2s 30s "$gc_bin" bd show "$bead_id" --json) >"$bead_json"
    jq -e -s --arg bead_id "$bead_id" '
      length == 1 and
      (.[0] | type == "array" and length == 1 and
        .[0].id == $bead_id and .[0].issue_type == "task" and
        .[0].status == "closed" and
        .[0].metadata["gc.outcome"] == "pass")
    ' "$bead_json" >/dev/null ||
      die "$activation bd show was not a strict one-element closed work-bead array"
    stamped="$(jq -er '.[0].metadata["gc.output_json"]' "$bead_json")"
    compact="$(lumen_demo_single_json_object "$artifact" compact)"
    [[ "$stamped" == "$compact" ]] ||
      die "$activation work bead gc.output_json does not exactly match its artifact"
  done
}

validate_session_providers() {
  local template
  local provider
  local row_file
  local session_id
  local session_name
  local bead_json
  mkdir -p "$evidence/session-beads"
  lumen_demo_session_set "$evidence/session-set.json" ||
    die "session provider validation received an invalid four-session set"
  while IFS='|' read -r template provider row_file; do
    lumen_demo_single_json_object "$row_file" compact >/dev/null ||
      die "$template captured row is not one compact JSON object"
    session_id="$(jq -er '.id' "$row_file")"
    session_name="$(jq -er '.session_name' "$row_file")"
    bead_json="$evidence/session-beads/$template.json"
    (cd "$city" && timeout --kill-after=2s 30s "$gc_bin" bd show "$session_id" --json) >"$bead_json"
    lumen_demo_session_bead_provenance \
      "$bead_json" "$session_id" "$session_name" "$template" "$provider" ||
      die "$template session bead did not persist its exact ID, name, template, provider, and non-contradictory transport"
  done <<EOF
laneOneAgent|claude|$evidence/lane-one-session-row.json
laneTwoAgent|codex|$evidence/lane-two-session-row.json
synthesisAgent|claude|$evidence/synthesis-session-row.json
verifierAgent|codex|$evidence/verifier-session-row.json
EOF
}

printf -v attach_command 'tmux -L %q attach-session -t %q' "$recorder_socket" "$recorder_session"
drive_demo &
driver_pid=$!
asciinema rec \
  --overwrite \
  --env SHELL,TERM \
  --idle-time-limit 86400 \
  --cols 160 \
  --rows 44 \
  --title "Real Claude/Codex inference · default managed bd+Dolt" \
  --command "$attach_command" \
  "$cast"
wait "$driver_pid"
driver_pid=""

validate_artifacts
validate_session_providers
stream_id="$(rg -o 'gcg-run-[a-z0-9-]+' "$evidence/run.stdout" | head -n 1 || true)"
[[ -n "$stream_id" ]] || die "could not recover the Lumen stream id from gc run output"
validate_journal_and_outputs "$stream_id"

cp -rf "$city/review-artifacts" "$evidence/artifacts"
cp -f "$city/work/design.before.md" "$evidence/document.before.md"
cp -f "$city/work/design.md" "$evidence/document.revised.md"
capture_managed_dolt_proof final ||
  die "managed Dolt proof did not remain live through artifact validation"

claude_version="$(timeout --kill-after=2s 15s claude --version 2>&1 | head -n 1)"
codex_version="$(timeout --kill-after=2s 15s codex --version 2>&1 | head -n 1)"
session_snapshot_to "$evidence/sessions-pre-shutdown.json" ||
  die "could not capture the final pre-shutdown session history"
pre_shutdown_names="$(jq -c '[.[].session_name]' "$evidence/session-set.json")"
session_bead_snapshot_to "$evidence/session-beads-pre-shutdown.json" ||
  die "could not capture the final pre-shutdown session bead history"
lumen_demo_session_beads_returned \
  "$evidence/session-beads-pre-shutdown.json" "$evidence/session-set.json" ||
  die "the exact four workflow session beads did not remain returned before shutdown"
lumen_demo_session_projection_returned \
  "$evidence/sessions-pre-shutdown.json" "$evidence/session-set.json" ||
  die "an active replacement or extra session appeared before shutdown"
tmux_snapshot_to "$evidence/tmux-sessions-pre-shutdown.txt" ||
  die "could not capture the final pre-shutdown City tmux snapshot"
lumen_demo_tmux_sessions_absent \
  "$evidence/tmux-sessions-pre-shutdown.txt" "$pre_shutdown_names" ||
  die "one of the four workflow runtime sessions reappeared before shutdown"
shutdown_runtime_checked || die "could not stop the isolated demo runtime cleanly"

# Re-check every immutable input after the last use of the runtime. The
# recording is rejected if the source, staged executable, or archive moved.
require_clean_tracked_tree
[[ "$(git -C "$repo" rev-parse HEAD)" == "$repo_commit" ]] ||
  die "source repository HEAD changed during the demo"
[[ "$(lumen_demo_binary_commit "$gc_bin_source" "$repo_commit")" == "$source_binary_commit" ]] ||
  die "source GC_BIN commit changed during the demo"
[[ "$(sha256sum "$gc_bin_source" | awk '{print $1}')" == "$source_binary_sha256" ]] ||
  die "source GC_BIN bytes changed during the demo"
[[ "$(lumen_demo_binary_commit "$gc_bin" "$repo_commit")" == "$binary_commit" ]] ||
  die "staged GC_BIN commit changed during the demo"
[[ "$(sha256sum "$gc_bin" | awk '{print $1}')" == "$binary_sha256" ]] ||
  die "staged GC_BIN bytes changed during the demo"
[[ -z "$(find "$gc_bin" -perm /222 -print -quit)" ]] ||
  die "staged GC_BIN became writable during the demo"
verify_pinned_tool claude "$claude_bin_source" "$claude_bin_source_sha256" ||
  die "pinned Claude executable changed during the demo"
lumen_demo_file_matches_sha256 "$codex_launcher_source" "$codex_launcher_source_sha256" ||
  die "recorded Codex launcher changed during the demo"
verify_pinned_tool codex "$codex_bin_source" "$codex_bin_source_sha256" ||
  die "pinned Codex executable changed during the demo"
verify_pinned_tool bd "$bd_bin_source" "$bd_bin_source_sha256" ||
  die "pinned bd executable changed during the demo"
verify_pinned_tool dolt "$dolt_bin_source" "$dolt_bin_source_sha256" ||
  die "pinned Dolt executable changed during the demo"
verify_pinned_tool rg "$rg_bin_source" "$rg_bin_source_sha256" ||
  die "pinned ripgrep executable changed during the demo"
verify_pinned_tool agg "$agg_bin_source" "$agg_bin_source_sha256" ||
  die "pinned agg executable changed during the demo"
if [[ -n "$(find "$toolchain_dir" \( -type f -o -type d \) -perm /222 -print -quit)" ]]; then
  die "pinned toolchain became writable during the demo"
fi
[[ "$(lumen_demo_tree_sha256 "$repo_snapshot")" == "$repo_snapshot_sha256" ]] ||
  die "read-only repository snapshot changed during the demo"
lumen_demo_file_matches_sha256 "$source_design" "$source_design_sha256" ||
  die "committed source design changed during the demo"
lumen_demo_file_matches_sha256 "$city/work/design.before.md" "$source_design_sha256" ||
  die "pristine design baseline changed during the demo"
lumen_demo_file_matches_sha256 "$evidence/original.md" "$source_design_sha256" ||
  die "retained synthesis original does not match the committed design"
lumen_demo_file_matches_sha256 "$evidence/document.before.md" "$source_design_sha256" ||
  die "retained before document does not match the committed design"
if [[ -n "$(find "$repo_snapshot" \( -type f -o -type d \) -perm /222 -print -quit)" ]]; then
  die "repository snapshot became writable during the demo"
fi
[[ -z "$claude_credential_link" || (! -e "$claude_credential_link" && ! -L "$claude_credential_link") ]] ||
  die "Claude credential link survived runtime shutdown"
[[ -z "$codex_credential_link" || (! -e "$codex_credential_link" && ! -L "$codex_credential_link") ]] ||
  die "Codex credential link survived runtime shutdown"

timeout --kill-after=10s 10m agg \
  --theme github-dark \
  --font-size 14 \
  --fps-cap 20 \
  --speed "$speed" \
  --idle-time-limit 86400 \
  --last-frame-duration 4 \
  "$cast" "$gif"

if [[ -n "$codex_gateway_secret" ]]; then
  secret_scan_summary="$(OPENAI_API_KEY="$codex_gateway_secret" \
    lumen_demo_scan_evidence_secrets \
    "$evidence" "$claude_credentials_source" "$codex_credentials_source")" ||
    die "retained evidence failed the credential-material scan"
else
  secret_scan_summary="$(lumen_demo_scan_evidence_secrets \
    "$evidence" "$claude_credentials_source" "$codex_credentials_source")" ||
    die "retained evidence failed the credential-material scan"
fi
printf '%s\n' "$secret_scan_summary" >"$evidence/secret-scan.json"

evidence_manifest="$evidence/evidence-sha256.json"
python3 -I - "$evidence" "$evidence_manifest" <<'PY'
import hashlib
import json
import pathlib
import sys

evidence = pathlib.Path(sys.argv[1])
output = pathlib.Path(sys.argv[2])
files = []
for path in sorted(evidence.rglob("*"), key=lambda item: item.relative_to(evidence).as_posix()):
    if path in (output, evidence / "manifest.json"):
        continue
    if path.is_symlink():
        raise SystemExit(f"stable evidence contains a symlink: {path}")
    if path.is_dir():
        continue
    if not path.is_file():
        raise SystemExit(f"stable evidence contains an unsupported filesystem entry: {path}")
    payload = path.read_bytes()
    files.append({
        "path": path.relative_to(evidence).as_posix(),
        "bytes": len(payload),
        "sha256": hashlib.sha256(payload).hexdigest(),
    })
if not files:
    raise SystemExit("stable evidence set is empty")
document = {"algorithm": "sha256", "file_count": len(files), "files": files}
output.write_text(json.dumps(document, indent=2, sort_keys=True) + "\n", encoding="utf-8")
PY

cast_sha256="$(sha256sum "$cast" | awk '{print $1}')"
gif_sha256="$(sha256sum "$gif" | awk '{print $1}')"
evidence_manifest_sha256="$(sha256sum "$evidence_manifest" | awk '{print $1}')"
toolchain_manifest="$evidence/toolchain.json"
toolchain_manifest_sha256="$(sha256sum "$toolchain_manifest" | awk '{print $1}')"

# manifest.json is intentionally the last retained file written.
jq -n \
  --arg label "Real Claude/Codex inference · default managed bd+Dolt" \
  --arg run_id "$run_id" \
  --arg stream_id "$stream_id" \
  --arg repo_commit "$repo_commit" \
  --arg repo_snapshot "$repo_snapshot" \
  --arg repo_snapshot_sha256 "$repo_snapshot_sha256" \
  --arg source_design "$source_design" \
  --arg source_design_sha256 "$source_design_sha256" \
  --arg source_binary "$gc_bin_source" \
  --arg source_binary_commit "$source_binary_commit" \
  --arg source_binary_sha256 "$source_binary_sha256" \
  --arg binary "$gc_bin" \
  --arg binary_commit "$binary_commit" \
  --arg binary_sha256 "$binary_sha256" \
  --arg cast "$cast" \
  --arg cast_sha256 "$cast_sha256" \
  --arg gif "$gif" \
  --arg gif_sha256 "$gif_sha256" \
  --arg evidence_manifest "$evidence_manifest" \
  --arg evidence_manifest_sha256 "$evidence_manifest_sha256" \
  --arg toolchain_manifest "$toolchain_manifest" \
  --arg toolchain_manifest_sha256 "$toolchain_manifest_sha256" \
  --arg claude_version "$claude_version" \
  --arg codex_version "$codex_version" \
  --arg speed "${speed}x continuous" \
  --arg root "$root" '
  {
    label:$label,
    run_id:$run_id,
    stream_id:$stream_id,
    repo_commit:$repo_commit,
    repo_snapshot:$repo_snapshot,
    repo_snapshot_sha256:$repo_snapshot_sha256,
    source_design:{path:$source_design,sha256:$source_design_sha256},
    source_binary:{path:$source_binary,commit:$source_binary_commit,sha256:$source_binary_sha256},
    binary:{path:$binary,commit:$binary_commit,sha256:$binary_sha256},
    binary_commit:$binary_commit,
    binary_sha256:$binary_sha256,
    recording:{cast:$cast,cast_sha256:$cast_sha256,gif:$gif,gif_sha256:$gif_sha256},
    evidence:{manifest:$evidence_manifest,manifest_sha256:$evidence_manifest_sha256},
    toolchain:{manifest:$toolchain_manifest,manifest_sha256:$toolchain_manifest_sha256},
    outcome:"pass",
    session_count:4,
    tracked_tree_clean:true,
    backend:"managed bd+Dolt",
    inference_providers:["claude","codex"],
    claude_version:$claude_version,
    codex_version:$codex_version,
    speed:$speed,
    workspace_root:$root
  }
' >"$evidence/manifest.json"

printf 'evidence=%s\ncast=%s\ngif=%s\n' "$evidence" "$cast" "$gif"
