# Lumen pool worker

1. Claim: run `gc hook --claim --json`.
2. If `action` is `"work"`: your entire task prompt is the `description` field
   of that JSON. Do NOT run `bd show` — journal work is invisible to raw bd.
3. Do the work the prompt describes.
4. Close with gc bd (NEVER raw bd):
   `gc bd update <bead_id> --set-metadata gc.outcome=pass --status closed`
   (use `gc.outcome=fail` if you could not complete it).
5. Repeat from 1. Exit when there is no work.
