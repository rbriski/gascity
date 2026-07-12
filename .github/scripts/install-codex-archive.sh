#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: install-codex-archive.sh VERSION [--cache]

Downloads an openai/codex release tarball, verifies its pinned SHA-256, and
installs the codex binary. Use --cache on self-hosted runners to install under
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

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *)
    echo "Unsupported OS: $(uname -s)" >&2
    exit 1
    ;;
esac

case "$(uname -m)" in
  arm64|aarch64) cpu=aarch64 ;;
  x86_64|amd64) cpu=x86_64 ;;
  *)
    echo "Unsupported architecture: $(uname -m)" >&2
    exit 1
    ;;
esac

version_no_v="${version#v}"
case "$os" in
  linux) triple="${cpu}-unknown-linux-musl" ;;
  darwin) triple="${cpu}-apple-darwin" ;;
esac
tag="rust-v${version_no_v}"
archive="codex-${triple}.tar.gz"
expected_sha=""
case "${version_no_v}:${triple}" in
  0.144.1:x86_64-unknown-linux-musl) expected_sha="84091ae20c65fcc7d4120db97d1bd57d7ff8df9c7609fb781c78c2ebbd4f5a28" ;;
  0.144.1:aarch64-unknown-linux-musl) expected_sha="b9f8ef5f98e46ced4dbbd3756a4223e3ee299a457ff488a3305bea455da8b5b8" ;;
  0.144.1:x86_64-apple-darwin) expected_sha="0ea72d21c794504342d5fe0d5d057b0221c0a42f4bdf4a48b95af243af2b0c0e" ;;
  0.144.1:aarch64-apple-darwin) expected_sha="88e72ac8bd30815f7d18e62dac333dc20ce3ad1cba94be1649a1977dd9bfdbb8" ;;
esac

github_release_asset_sha() {
  local owner_repo="$1"
  local release_tag="$2"
  local asset="$3"
  if ! command -v jq >/dev/null 2>&1; then
    echo "jq is required to resolve GitHub release asset checksums" >&2
    exit 1
  fi
  local auth_header=()
  if [[ -n "${GITHUB_TOKEN:-}" ]]; then
    auth_header=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
  fi
  curl -fsSL --retry 5 --retry-delay 2 --retry-all-errors --retry-connrefused "${auth_header[@]}" \
    -H "Accept: application/vnd.github+json" \
    "https://api.github.com/repos/${owner_repo}/releases/tags/${release_tag}" \
    | jq -r --arg asset "$asset" '.assets[] | select(.name == $asset) | .digest // empty' \
    | sed 's/^sha256://'
}

if [[ -z "$expected_sha" ]]; then
  expected_sha="$(github_release_asset_sha "openai/codex" "$tag" "$archive")"
  if [[ -z "$expected_sha" ]]; then
    echo "No codex checksum found for ${version_no_v}/${triple}" >&2
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

install_binary() {
  local src="$1"
  local dst="$2"
  mkdir -p "$(dirname "$dst")"
  install -m 0755 "$src" "$dst"
}

install_binary_with_sudo_fallback() {
  local src="$1"
  local dst="$2"
  local dst_dir
  dst_dir="$(dirname "$dst")"
  mkdir -p "$dst_dir"
  if [[ -w "$dst_dir" ]]; then
    install_binary "$src" "$dst"
  elif command -v sudo >/dev/null 2>&1; then
    sudo install -m 0755 "$src" "$dst"
  else
    echo "Cannot write $dst and sudo is unavailable" >&2
    exit 1
  fi
}

if $use_cache; then
  cache_root="${RUNNER_TOOL_CACHE:-$HOME/.local}"
  bin_dir="${cache_root}/gascity-codex/${version_no_v}/${triple}/bin"
else
  bin_dir="${CODEX_INSTALL_BIN_DIR:-/usr/local/bin}"
fi

target="${bin_dir}/codex"
if [[ -x "$target" ]]; then
  echo "Reusing cached codex ${version_no_v} at ${target}"
else
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  curl -fsSL --retry 5 --retry-delay 2 --retry-all-errors --retry-connrefused -o "${tmp}/${archive}" \
    "https://github.com/openai/codex/releases/download/${tag}/${archive}"
  actual_sha="$(sha256_file "${tmp}/${archive}")"
  if [[ "$actual_sha" != "$expected_sha" ]]; then
    echo "codex checksum mismatch for ${version_no_v}/${triple}" >&2
    echo "expected: $expected_sha" >&2
    echo "actual:   $actual_sha" >&2
    exit 1
  fi
  tar -xzf "${tmp}/${archive}" -C "$tmp"
  src="${tmp}/codex-${triple}"
  if [[ ! -x "$src" ]]; then
    echo "codex binary not found in ${archive}" >&2
    exit 1
  fi
  if $use_cache; then
    install_binary "$src" "$target"
  else
    install_binary_with_sudo_fallback "$src" "$target"
  fi
fi

if $use_cache && [[ -n "${GITHUB_PATH:-}" ]]; then
  echo "$bin_dir" >> "$GITHUB_PATH"
fi

"$target" --version
