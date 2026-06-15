#!/usr/bin/env bash
# check-gomod-replace.sh [go.mod-path]
#
# Fails if go.mod contains any replace directive targeting an unreleased
# version: pseudo-version, local filesystem path, or git branch/ref.
#
# Policy: gascity is a public project. It must only pin released semver tags.
# The only override is an explicit human-operator decision (e.g. an emergency
# security fix from an unreleased commit). That override is a manual admin
# bypass of this required CI check — automated workers may NEVER self-authorize
# an unreleased dependency.
#
# Released: v1.2.3 or vX.Y.Z with only digits (e.g. v0.0.0)
# Unreleased: pseudo-version (vX.Y.Z-0.YYYYMMDDHHMMSS-<sha>), local path (./..  ../.. /...), git ref
set -euo pipefail

gomod="${1:-go.mod}"

if [[ ! -f "$gomod" ]]; then
	echo "check-gomod-replace: $gomod not found" >&2
	exit 1
fi

# Extract replace targets (the part after "=>").
# go.mod replace syntax: replace <old> [<version>] => <new> [<new-version>]
# We only care about the replacement target, which follows "=>".
failed=0
while IFS= read -r line; do
	# Strip leading whitespace and skip non-replace lines.
	stripped="${line#"${line%%[! ]*}"}"
	[[ "$stripped" == replace* ]] || continue

	# Extract the replacement target: everything after "=>".
	rhs="${stripped#*=>}"
	rhs="${rhs#"${rhs%%[! ]*}"}"  # strip leading whitespace

	# Split into path and optional version (the last space-separated token).
	# If rhs is "github.com/foo/bar v1.0.0-pseudo", path=github.com/foo/bar version=v1.0.0-pseudo
	# If rhs is "./local/path", path=./local/path version=""
	version=""
	path_part="$rhs"
	if [[ "$rhs" =~ ^([^ ]+)[[:space:]]+([^ ]+)$ ]]; then
		path_part="${BASH_REMATCH[1]}"
		version="${BASH_REMATCH[2]}"
	fi

	# Local filesystem paths are always unreleased.
	if [[ "$path_part" == ./* || "$path_part" == ../* || "$path_part" == /* ]]; then
		echo "check-gomod-replace: BLOCKED — replace directive targets a local path:" >&2
		echo "  $stripped" >&2
		echo "" >&2
		echo "  Policy: gascity is a public project that must only pin released semver deps." >&2
		echo "  Local-path replaces (./  ../  /) may not appear in committed go.mod." >&2
		echo "  Override: human operator must manually bypass this required CI check." >&2
		failed=1
		continue
	fi

	# If there's no version, the replace is a path-only redirect (unusual but valid if
	# it resolves to a released tag). Nothing to check here without a version.
	[[ -n "$version" ]] || continue

	# Pseudo-version pattern: vX.Y.Z-0.YYYYMMDDHHMMSS-<12hexsha>
	#                      or vX.Y.Z-<pre>.0.YYYYMMDDHHMMSS-<sha>
	# Both contain a 14-digit timestamp and a hex sha separated by hyphens.
	if [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-.+)?\.[0-9]{14}-[0-9a-f]+ ]]; then
		echo "check-gomod-replace: BLOCKED — replace directive targets a pseudo-version (unreleased commit):" >&2
		echo "  $stripped" >&2
		echo "" >&2
		echo "  Policy: gascity is a public project that must only pin released semver deps." >&2
		echo "  Pseudo-versions pin to unreleased commits; only real vX.Y.Z tags are allowed." >&2
		echo "  Override: human operator must manually bypass this required CI check." >&2
		failed=1
	fi
done < "$gomod"

if [[ $failed -ne 0 ]]; then
	exit 1
fi

echo "check-gomod-replace: OK (no unreleased replace directives)"
