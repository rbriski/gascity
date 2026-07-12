#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: install-node-archive.sh VERSION [NPM_VERSION] [--cache]

Downloads an official Node.js release tarball from nodejs.org, verifies its
pinned SHA-256 against the release SHASUMS256.txt, and installs node/npm/npx
into the target prefix. When NPM_VERSION is given, upgrades the bundled npm
to that exact version afterwards (npm verifies the package tarball against
the registry's sha512 integrity digest and fails on mismatch): the tarball's
bundled npm can lag on fixes for vulnerable transitive dependencies. Use
--cache on self-hosted runners to install under RUNNER_TOOL_CACHE/HOME and
add that bin directory to GITHUB_PATH.
USAGE
}

version="${1:-}"
if [[ -z "$version" ]]; then
  usage
  exit 2
fi
shift || true

npm_version=""
if (($#)) && [[ "$1" != -* ]]; then
  npm_version="${1#v}"
  shift
fi

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

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *)
    echo "Unsupported OS: $(uname -s)" >&2
    exit 1
    ;;
esac

case "$(uname -m)" in
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64) arch=x64 ;;
  *)
    echo "Unsupported architecture: $(uname -m)" >&2
    exit 1
    ;;
esac

version_no_v="${version#v}"
platform="${os}-${arch}"
archive="node-v${version_no_v}-${platform}.tar.gz"
expected_sha=""
case "${version_no_v}:${platform}" in
  22.23.1:linux-x64) expected_sha="7a8cb04b4a1df4eaf432125324b81b29a088e73570a23259a8de1c65d07fc129" ;;
  22.23.1:linux-arm64) expected_sha="543fa39e57d4c07855939459a323f4deb9a79dd1bb45e6e99458b0f2de10db8d" ;;
  22.23.1:darwin-x64) expected_sha="b8da981b8a0b1241b70249204916da76c63573ddf5814dbd2d1e41069105cb81" ;;
  22.23.1:darwin-arm64) expected_sha="ef28d8fab2c0e4314522d4bb1b7173270aa3937e93b92cb7de79c112ac1fa953" ;;
esac

if [[ -z "$expected_sha" ]]; then
  shasums_url="https://nodejs.org/dist/v${version_no_v}/SHASUMS256.txt"
  expected_sha="$(curl -fsSL --retry 5 --retry-delay 2 --retry-all-errors --retry-connrefused "$shasums_url" \
    | awk -v file="$archive" '$2 == file {print $1}')"
  if [[ -z "$expected_sha" ]]; then
    echo "No Node.js checksum found for ${version_no_v}/${platform}" >&2
    exit 1
  fi
fi

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d ' ' -f 1
  else
    shasum -a 256 "$1" | cut -d ' ' -f 1
  fi
}

install_tree_with_sudo_fallback() {
  local src_dir="$1"
  local prefix="$2"
  local sudo_cmd=()
  if [[ ! -w "$prefix" ]] && [[ -d "$prefix" || ! -w "$(dirname "$prefix")" ]]; then
    if command -v sudo >/dev/null 2>&1; then
      sudo_cmd=(sudo)
    else
      echo "Cannot write $prefix and sudo is unavailable" >&2
      exit 1
    fi
  fi
  local dir
  for dir in bin include lib share; do
    [[ -d "${src_dir}/${dir}" ]] || continue
    "${sudo_cmd[@]}" mkdir -p "${prefix}/${dir}"
    "${sudo_cmd[@]}" cp -R "${src_dir}/${dir}/." "${prefix}/${dir}/"
  done
}

if $use_cache; then
  cache_root="${RUNNER_TOOL_CACHE:-$HOME/.local}"
  prefix="${cache_root}/gascity-node/${version_no_v}/${platform}"
else
  prefix="${NODE_INSTALL_PREFIX:-/usr/local}"
fi

bin_dir="${prefix}/bin"
target="${bin_dir}/node"
if [[ -x "$target" ]] && [[ "$("$target" --version 2>/dev/null)" == "v${version_no_v}" ]]; then
  echo "Reusing cached Node.js ${version_no_v} at ${target}"
else
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  curl -fsSL --retry 5 --retry-delay 2 --retry-all-errors --retry-connrefused -o "${tmp}/${archive}" \
    "https://nodejs.org/dist/v${version_no_v}/${archive}"
  actual_sha="$(sha256_file "${tmp}/${archive}")"
  if [[ "$actual_sha" != "$expected_sha" ]]; then
    echo "Node.js checksum mismatch for ${version_no_v}/${platform}" >&2
    echo "expected: $expected_sha" >&2
    echo "actual:   $actual_sha" >&2
    exit 1
  fi
  tar -xzf "${tmp}/${archive}" -C "$tmp"
  install_tree_with_sudo_fallback "${tmp}/node-v${version_no_v}-${platform}" "$prefix"
fi

if [[ -n "$npm_version" ]]; then
  npm_bin="${bin_dir}/npm"
  if [[ "$("$npm_bin" --version 2>/dev/null)" == "$npm_version" ]]; then
    echo "Reusing npm ${npm_version} at ${npm_bin}"
  else
    export npm_config_fund=false
    export npm_config_audit=false
    export npm_config_update_notifier=false
    if [[ -w "${prefix}/lib/node_modules" ]]; then
      "$npm_bin" install -g "npm@${npm_version}"
    elif command -v sudo >/dev/null 2>&1; then
      sudo --preserve-env=npm_config_fund,npm_config_audit,npm_config_update_notifier \
        "$npm_bin" install -g "npm@${npm_version}"
    else
      echo "Cannot write ${prefix}/lib/node_modules and sudo is unavailable" >&2
      exit 1
    fi
  fi
  actual_npm="$("$npm_bin" --version)"
  if [[ "$actual_npm" != "$npm_version" ]]; then
    echo "npm version mismatch after upgrade" >&2
    echo "expected: $npm_version" >&2
    echo "actual:   $actual_npm" >&2
    exit 1
  fi
fi

if $use_cache && [[ -n "${GITHUB_PATH:-}" ]]; then
  echo "$bin_dir" >> "$GITHUB_PATH"
fi

"$target" --version
