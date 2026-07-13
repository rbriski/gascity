#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: install-pi-npm.sh VERSION [--cache]

Installs the pi coding agent (@earendil-works/pi-coding-agent) globally at an
exact version via npm. npm verifies the package tarball against the registry's
Subresource Integrity (sha512) digest and fails on mismatch, so the pinned
version is integrity-checked at install time. Requires node/npm on PATH
(install-node-archive.sh). Use --cache on self-hosted runners to install under
RUNNER_TOOL_CACHE/HOME and add that bin directory to GITHUB_PATH.
USAGE
}

version="${1:-}"
if [[ -z "$version" ]]; then
  usage
  exit 2
fi
shift || true

use_cache=false
while (($#)); do
  case "$1" in
    --cache) use_cache=true ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown argument: $1" >&2
      usage
      exit 2
      ;;
  esac
  shift
done

if ! command -v npm >/dev/null 2>&1; then
  echo "npm is required to install pi; run install-node-archive.sh first" >&2
  exit 1
fi

version_no_v="${version#v}"
package="@earendil-works/pi-coding-agent@${version_no_v}"

export npm_config_fund=false
export npm_config_audit=false
export npm_config_update_notifier=false

if $use_cache; then
  cache_root="${RUNNER_TOOL_CACHE:-$HOME/.local}"
  prefix="${cache_root}/gascity-pi/${version_no_v}"
  mkdir -p "$prefix"
  npm install -g --prefix "$prefix" "$package"
  bin_dir="${prefix}/bin"
  if [[ -n "${GITHUB_PATH:-}" ]]; then
    echo "$bin_dir" >> "$GITHUB_PATH"
  fi
  target="${bin_dir}/pi"
else
  prefix="$(npm prefix -g)"
  bin_dir="${prefix}/bin"
  if [[ -w "$prefix" ]] || [[ -w "$bin_dir" ]]; then
    npm install -g "$package"
  elif command -v sudo >/dev/null 2>&1; then
    # sudo resets PATH to its secure_path, so npm's `#!/usr/bin/env node` shim
    # would resolve node from secure_path instead of the node next to the
    # selected npm. Resolve the intended npm and pass an explicit PATH through
    # env so the sudo'd install runs under that same node/npm.
    npm_bin="$(command -v npm)"
    npm_bin_dir="$(dirname "$npm_bin")"
    sudo --preserve-env=npm_config_fund,npm_config_audit,npm_config_update_notifier \
      env "PATH=${npm_bin_dir}:${PATH}" "$npm_bin" install -g "$package"
  else
    echo "Cannot write ${prefix} and sudo is unavailable" >&2
    exit 1
  fi
  target="${bin_dir}/pi"
fi

if [[ ! -x "$target" ]] && command -v pi >/dev/null 2>&1; then
  target="$(command -v pi)"
fi

"$target" --version
