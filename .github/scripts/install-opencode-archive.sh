#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: install-opencode-archive.sh VERSION [--cache]

Downloads an sst/opencode release archive, verifies its pinned SHA-256, and
installs the opencode binary. Use --cache on self-hosted runners to install
under RUNNER_TOOL_CACHE/HOME and add that bin directory to GITHUB_PATH.
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
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64) arch=x64 ;;
  *)
    echo "Unsupported architecture: $(uname -m)" >&2
    exit 1
    ;;
esac

version_no_v="${version#v}"
tag="v${version_no_v}"
# Linux ships a gzip tarball; macOS ships a zip. Both extract a single
# `opencode` binary.
case "$os" in
  linux) archive="opencode-linux-${arch}.tar.gz" ;;
  darwin) archive="opencode-darwin-${arch}.zip" ;;
esac
platform="${os}-${arch}"
expected_sha=""
case "${tag}:${platform}" in
  v1.17.18:linux-x64) expected_sha="e149d32ee5667c0cd5fb84d0bf8393b312e93782eeb4d74d29bbb0392de7133c" ;;
  v1.17.18:linux-arm64) expected_sha="db9b53eae485da969a0a855bca465f9901dd84676384f724f320e3ccc5a9b107" ;;
  v1.17.18:darwin-x64) expected_sha="cebf209aad2c0bd998fbac3f8dd1b45eef35da1af18cd698e78b111b73c5fbb0" ;;
  v1.17.18:darwin-arm64) expected_sha="24327f89c103526c0518fc9b797767f318ab85ef3cee8636e722d6138f33aa3d" ;;
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
  expected_sha="$(github_release_asset_sha "sst/opencode" "$tag" "$archive")"
  if [[ -z "$expected_sha" ]]; then
    echo "No opencode checksum found for ${tag}/${platform}" >&2
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
  bin_dir="${cache_root}/gascity-opencode/${tag}/${platform}/bin"
else
  bin_dir="${OPENCODE_INSTALL_BIN_DIR:-/usr/local/bin}"
fi

target="${bin_dir}/opencode"
if [[ -x "$target" ]]; then
  echo "Reusing cached opencode ${tag} at ${target}"
else
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  curl -fsSL --retry 5 --retry-delay 2 --retry-all-errors --retry-connrefused -o "${tmp}/${archive}" \
    "https://github.com/sst/opencode/releases/download/${tag}/${archive}"
  actual_sha="$(sha256_file "${tmp}/${archive}")"
  if [[ "$actual_sha" != "$expected_sha" ]]; then
    echo "opencode checksum mismatch for ${tag}/${platform}" >&2
    echo "expected: $expected_sha" >&2
    echo "actual:   $actual_sha" >&2
    exit 1
  fi
  case "$archive" in
    *.tar.gz) tar -xzf "${tmp}/${archive}" -C "$tmp" ;;
    *.zip) unzip -q -o "${tmp}/${archive}" -d "$tmp" ;;
  esac
  src="${tmp}/opencode"
  if [[ ! -x "$src" ]]; then
    src="$(find "$tmp" -type f -name opencode -perm -111 | head -n 1)"
  fi
  if [[ -z "${src:-}" || ! -x "$src" ]]; then
    echo "opencode binary not found in ${archive}" >&2
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
