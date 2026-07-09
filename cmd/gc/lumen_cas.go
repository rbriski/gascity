package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// The Lumen run CAS dir holds the durable inputs a controller loop needs to drive
// a run across a restart: the journal pins ir_hash/input_hash but carries neither
// the IR nor the input. These are immutable, content-addressed provenance blobs —
// NOT status files. Run state lives only in the journal. BOTH blobs are keyed by the
// content hash run.started pins, so both are written BEFORE the run.started append:
// a crash between a blob write and the append leaves an orphan blob (harmless), never
// a discoverable run whose inputs cannot be loaded (a permanent wedge / phantom run).
//
//	<city>/.gc/graph/ir/<ir_hash>.json       — the compiled IR (shared across runs of one formula)
//	<city>/.gc/graph/input/<input_hash>.json — the run input (only when non-empty; shared across identical inputs)

// lumenIRBlobPath is the content-addressed path of a run's IR blob.
func lumenIRBlobPath(cityPath, irHash string) string {
	return filepath.Join(graphScopeRoot(cityPath), "ir", irHash+".json")
}

// lumenInputBlobPath is the content-addressed path of a run's input blob — keyed by
// the SAME input_hash run.started pins, so the loop can reload it by the manifest's
// InputHash across a controller restart (never by a streamID the journal alone can
// no longer resolve to a durable input).
func lumenInputBlobPath(cityPath, inputHash string) string {
	return filepath.Join(graphScopeRoot(cityPath), "input", inputHash+".json")
}

// writeLumenIRBlob writes doc's JSON, content-addressed by irHash, atomically
// (temp file + rename). An existing blob at the content-addressed path is left
// untouched (same hash ⇒ same content — content-addressed idempotence, so
// re-enqueuing a formula reuses the blob). It is written FIRST (before run.started
// is appended), so a crash never leaves a discoverable run whose formula cannot be
// loaded — the drive-critical blob is durable before the run exists.
func writeLumenIRBlob(cityPath, irHash string, doc *ir.IR) error {
	path := lumenIRBlobPath(cityPath, irHash)
	if _, err := os.Stat(path); err == nil {
		return nil // already present (content-addressed)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshaling IR blob: %w", err)
	}
	return writeLumenBlobAtomic(path, raw)
}

// writeLumenInputBlob writes the run input content-addressed by inputHash,
// atomically, only when the input is non-empty. An existing blob at the
// content-addressed path is left untouched (same hash ⇒ same content). An empty
// (unpinned) input imposes no resume constraint, so no blob is written (inputHash is
// "") and the loader returns nil for it. It is written BEFORE run.started is
// appended, so a crash never leaves a discoverable run whose pinned input cannot be
// loaded — the drive-critical blob is durable before the run exists.
func writeLumenInputBlob(cityPath, inputHash string, input map[string]any) error {
	if len(input) == 0 || inputHash == "" {
		return nil
	}
	path := lumenInputBlobPath(cityPath, inputHash)
	if _, err := os.Stat(path); err == nil {
		return nil // already present (content-addressed)
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("marshaling input blob: %w", err)
	}
	return writeLumenBlobAtomic(path, raw)
}

// writeLumenBlobAtomic writes raw to path via a temp file + os.Rename in the same
// directory (the repo's atomic-write convention), creating parent dirs.
func writeLumenBlobAtomic(path string, raw []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating blob dir %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-blob-*")
	if err != nil {
		return fmt.Errorf("creating temp blob: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // best-effort if a step below fails
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing temp blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp blob: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming blob into place: %w", err)
	}
	return nil
}

// loadLumenRunInputs loads a run's IR (by m.IRHash) and input (by m.InputHash) from
// the content-addressed CAS dir for the controller loop — both keyed by the hashes
// run.started pinned, so a restart reloads exactly the blobs the run was seeded with.
// A missing or malformed IR blob is a LOUD error naming the path — a run cannot be
// driven without its formula, and the caller must refuse rather than drive. A missing
// input blob yields nil (an unpinned-input run imposes no constraint, and a
// pinned-input run whose blob is gone is caught loudly by Advance's rebuild
// input_hash guard: inputHash(nil)=="" ≠ the pinned non-empty hash ⇒
// ErrInputHashMismatch). Blob equality is NOT re-verified here — the authoritative
// guard is Advance's rebuild (ErrIRHashMismatch / ErrInputHashMismatch), so a
// corrupted or swapped blob is a loud typed refusal, never a divergent drive.
func loadLumenRunInputs(cityPath string, m engine.RunManifest) (*ir.IR, map[string]any, error) {
	doc, err := loadLumenIR(cityPath, m.IRHash)
	if err != nil {
		return nil, nil, err
	}
	input, err := loadLumenInput(cityPath, m.InputHash)
	if err != nil {
		return nil, nil, err
	}
	return doc, input, nil
}

// loadLumenIR reads and decodes a run's IR blob. A missing file or a malformed
// contract is a loud error naming the expected path.
func loadLumenIR(cityPath, irHash string) (*ir.IR, error) {
	path := lumenIRBlobPath(cityPath, irHash)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading IR blob %q: %w", path, err)
	}
	doc, err := ir.Decode(raw)
	if err != nil {
		return nil, fmt.Errorf("decoding IR blob %q: %w", path, err)
	}
	return doc, nil
}

// loadLumenInput reads a run's input blob by its content hash. An empty inputHash is
// an unpinned run — (nil, nil), no blob expected. A non-empty inputHash whose blob is
// ABSENT also returns (nil, nil): the loader does not re-derive the pin, it defers to
// Advance's rebuild, which sees inputHash(nil)=="" against the journal's pinned
// non-empty hash and refuses loudly with ErrInputHashMismatch (the guard stays — a
// pinned-input run is never silently driven with the wrong scope). A
// present-but-malformed blob is a loud error.
func loadLumenInput(cityPath, inputHash string) (map[string]any, error) {
	if inputHash == "" {
		return nil, nil
	}
	path := lumenInputBlobPath(cityPath, inputHash)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading input blob %q: %w", path, err)
	}
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, fmt.Errorf("decoding input blob %q: %w", path, err)
	}
	return input, nil
}
