# Real-inference review quorum

This City template runs the Lumen review quorum with authenticated Claude and
Codex provider CLIs. It uses tmux for visible sessions and Gas City's default
managed bd+Dolt task store.

The recorder detects false greens, persistent input/runtime drift, leaked
credentials in retained evidence, and replacement sessions. It assumes the
four local inference Agents cooperate with their prompts. They share the
operator's Unix UID, so this is not a sandbox against a malicious Agent; use
separate UIDs or containers with read-only mounts for hostile inputs.

Before starting, `claude auth status`, a real exact-output Codex inference
preflight, and the normal Dolt author identity (`dolt config --global --get
user.name` and `user.email`) must succeed. The recorder performs these checks;
complete each provider's first-project trust/onboarding once for the disposable
City path if the installed CLI requests it.

Prepare a fresh disposable workspace from the committed repository. The Agents
inspect a read-only snapshot, never the source checkout:

```bash
GC=/absolute/path/to/gc
REPO=/absolute/path/to/gascity
DEMO_ROOT="$(mktemp -d /tmp/lumen-review.XXXXXX)"
CITY="$DEMO_ROOT/city"
REPOSITORY_SNAPSHOT="$DEMO_ROOT/repository"

mkdir -p "$REPOSITORY_SNAPSHOT"
git -C "$REPO" archive HEAD | tar -x -C "$REPOSITORY_SNAPSHOT"
chmod -R a-w "$REPOSITORY_SNAPSHOT"

"$GC" init --from "$REPOSITORY_SNAPSHOT/examples/lumen/review-quorum-live" --no-start "$CITY"
cp -f "$REPOSITORY_SNAPSHOT/examples/lumen/review-quorum.lumen" "$CITY/review-quorum.lumen"
cp -f "$REPOSITORY_SNAPSHOT/examples/lumen/review-quorum.lumen.json" "$CITY/review-quorum.lumen.json"
mkdir -p "$CITY/work" "$CITY/review-artifacts"
cp -f "$REPOSITORY_SNAPSHOT/engdocs/design/gc-reload-design.md" "$CITY/work/design.before.md"
cp -f "$REPOSITORY_SNAPSHOT/engdocs/design/gc-reload-design.md" "$CITY/work/design.md"

(cd "$CITY" && "$GC" migrate graph-journal init)
"$GC" start "$CITY"
```

Build the input with absolute paths, then use the public front door:

```bash
INPUT="$(jq -cn \
  --arg document_path "$CITY/work/design.md" \
  --arg repository_path "$REPOSITORY_SNAPSHOT" \
  --arg artifact_dir "$CITY/review-artifacts" \
  --arg objective "Make the gc reload design implementation-ready" \
  --arg lane_one_id "implementation-realism" \
  --arg lane_two_id "test-operability" \
  '{document_path:$document_path,repository_path:$repository_path,artifact_dir:$artifact_dir,objective:$objective,lane_one_id:$lane_one_id,lane_two_id:$lane_two_id}')"

cd "$CITY"
"$GC" run review-quorum.lumen --route synthesisAgent --input "$INPUT"
```

From another terminal, observe each inference session:

```bash
cd "$CITY"
"$GC" session list --state all
"$GC" session peek <session-id-or-name-from-the-list> --lines 80
```

The run passes only after `review-artifacts/verification.json` validates the two
independent reviews, the synthesis report, and a substantive document diff.
Stop the disposable City with `"$GC" stop "$CITY"` when finished.

To produce the continuous 15x recording, first commit the demo and build `gc`
from that clean commit. The recorder rejects stale or dirty binaries:

```bash
GOFLAGS=-buildvcs=false make build
cp -f bin/gc .bin/gc
GC_BIN="$PWD/.bin/gc" contrib/demo/record-lumen-review.sh
```

When Codex uses an HTTPS gateway instead of local login state, pass the gateway
origin explicitly; the recorder stages its command-backed token without putting
the credential in a tmux command line:

```bash
LUMEN_DEMO_CODEX_BASE_URL="$OPENAI_BASE_URL" \
  GC_BIN="$PWD/.bin/gc" \
  contrib/demo/record-lumen-review.sh
```

The same content and lifecycle assertions are available as an opt-in live gate:

```bash
GC_LUMEN_LIVE_REVIEW=1 PROFILE=claude/tmux-cli \
  go test -tags acceptance_c -count=1 -timeout 60m \
  ./test/acceptance/worker_inference -run '^TestLumenRealDesignReview$' -v
```
