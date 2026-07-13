#!/usr/bin/env bash
# Smoke test for install-node-archive.sh --cache against an empty
# RUNNER_TOOL_CACHE. Regression guard for the cache-mode contract: a fresh cache
# prefix must not trip the sudo fallback, and the npm upgrade must run under the
# freshly installed node rather than whatever node is on the caller's PATH.
set -euo pipefail

node_version="${1:-}"
npm_version="${2:-}"
if [[ -z "$node_version" || -z "$npm_version" ]]; then
  echo "Usage: smoke-install-node-cache.sh NODE_VERSION NPM_VERSION" >&2
  exit 2
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
installer="${script_dir}/install-node-archive.sh"

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

node_version_no_v="${node_version#v}"
platform="${os}-${arch}"

workdir="$(mktemp -d)"
cache_dir="$(mktemp -d)"
github_path="$(mktemp)"
trap 'rm -rf "$workdir" "$cache_dir" "$github_path"' EXIT

# Decoy node placed first on the caller's PATH. If the installer resolves npm's
# `#!/usr/bin/env node` shim to this host node instead of the node it just
# installed, the decoy aborts and this smoke test fails — which is exactly the
# regression this test guards. It makes the check independent of whatever node
# the CI runner happens to preinstall.
decoy_bin="${workdir}/decoy-bin"
mkdir -p "$decoy_bin"
cat >"${decoy_bin}/node" <<'DECOY'
#!/usr/bin/env bash
echo "smoke failure: npm ran under the host (decoy) node, not the installed node" >&2
exit 97
DECOY
chmod +x "${decoy_bin}/node"

echo "Running install-node-archive.sh ${node_version} ${npm_version} --cache against a fresh RUNNER_TOOL_CACHE"
PATH="${decoy_bin}:${PATH}" \
  RUNNER_TOOL_CACHE="$cache_dir" \
  GITHUB_PATH="$github_path" \
  "$installer" "$node_version" "$npm_version" --cache

prefix="${cache_dir}/gascity-node/${node_version_no_v}/${platform}"
node_bin="${prefix}/bin/node"
npm_bin="${prefix}/bin/npm"

if [[ ! -x "$node_bin" ]]; then
  echo "smoke failure: node not installed at ${node_bin}" >&2
  exit 1
fi

actual_node="$("$node_bin" --version)"
if [[ "$actual_node" != "v${node_version_no_v}" ]]; then
  echo "smoke failure: node ${actual_node} != v${node_version_no_v}" >&2
  exit 1
fi

# Invoke npm through the installed node explicitly so the assertion does not
# depend on the caller's PATH resolution.
actual_npm="$("$node_bin" "$npm_bin" --version)"
if [[ "$actual_npm" != "$npm_version" ]]; then
  echo "smoke failure: npm ${actual_npm} != ${npm_version}" >&2
  exit 1
fi

if ! grep -qxF "${prefix}/bin" "$github_path"; then
  echo "smoke failure: ${prefix}/bin not appended to GITHUB_PATH" >&2
  exit 1
fi

echo "OK: cached node ${actual_node} and npm ${actual_npm} installed without a host node"
