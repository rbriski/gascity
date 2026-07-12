#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: install-antigravity-archive.sh VERSION [--cache]

Downloads a google-antigravity/antigravity-cli release tarball, verifies its
pinned SHA-256, and installs the Antigravity CLI as `agy` (the tarball ships
the binary as `antigravity`; upstream's installer renames it to `agy`). Use
--cache on self-hosted runners to install under RUNNER_TOOL_CACHE/HOME and add
that bin directory to GITHUB_PATH.
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
  Darwin) os=mac ;;
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
tag="${version_no_v}"
platform="${os}_${arch}"
archive="agy_cli_${platform}.tar.gz"
expected_sha=""
case "${tag}:${platform}" in
  1.1.1:linux_x64) expected_sha="2ee167841cdc9a1d7dc5a624f1f15b84ee5dbb94b85af662a7299118cb4b1586" ;;
  1.1.1:linux_arm64) expected_sha="3fc542686c5c82d7a01e3796a8bfcda5ed849c6e70f07d4d0c93e51368952784" ;;
  1.1.1:mac_x64) expected_sha="f04855a9d14a9f29476b2343b5f827e897b187a7adce065201fef15c5d1a70bd" ;;
  1.1.1:mac_arm64) expected_sha="83333dd7131bebcce2dfa5f94722efce442d7b67e9ab9b240c91f100a26d4675" ;;
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
  expected_sha="$(github_release_asset_sha "google-antigravity/antigravity-cli" "$tag" "$archive")"
  if [[ -z "$expected_sha" ]]; then
    echo "No Antigravity CLI checksum found for ${tag}/${platform}" >&2
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
  bin_dir="${cache_root}/gascity-antigravity/${tag}/${platform}/bin"
else
  bin_dir="${ANTIGRAVITY_INSTALL_BIN_DIR:-/usr/local/bin}"
fi

target="${bin_dir}/agy"
if [[ -x "$target" ]]; then
  echo "Reusing cached Antigravity CLI ${tag} at ${target}"
else
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  curl -fsSL --retry 5 --retry-delay 2 --retry-all-errors --retry-connrefused -o "${tmp}/${archive}" \
    "https://github.com/google-antigravity/antigravity-cli/releases/download/${tag}/${archive}"
  actual_sha="$(sha256_file "${tmp}/${archive}")"
  if [[ "$actual_sha" != "$expected_sha" ]]; then
    echo "Antigravity CLI checksum mismatch for ${tag}/${platform}" >&2
    echo "expected: $expected_sha" >&2
    echo "actual:   $actual_sha" >&2
    exit 1
  fi
  tar -xzf "${tmp}/${archive}" -C "$tmp"
  src="${tmp}/antigravity"
  if [[ ! -x "$src" ]]; then
    echo "antigravity binary not found in ${archive}" >&2
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
