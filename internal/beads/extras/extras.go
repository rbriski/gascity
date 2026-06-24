// Package extras is the shared, pure persistence primitive every per-class
// domain row codec reuses to translate a bead's metadata map to and from its
// durable storage form without losing any key.
//
// A relocated infrastructure entity is stored as a set of promoted, typed
// columns plus a single versioned "extras" blob. The blob holds two things:
// the enumerated-but-not-promoted fields the owning adapter models as a typed
// struct (Known), and every metadata key this binary did not enumerate at all
// (Unknown), preserved verbatim. Keeping the Unknown passthrough is what lets
// the bd-delegating phase round-trip losslessly: a key a future binary adds is
// carried through untouched rather than dropped.
//
// This package is a Layer-0 substrate helper: it imports only the standard
// library and must never depend on internal/api, internal/session, or any other
// domain (Layer-1) package. It performs no I/O and reads no clock — every
// function here is pure.
package extras

import (
	"encoding/json"
	"fmt"
)

// Version is the current extras-envelope schema version. It is stamped into
// every blob (see Encode) so a future schema change can migrate older rows.
const Version = 1

// Envelope is the versioned extras blob persisted alongside an entity's
// promoted columns. K is the owning adapter's typed struct of enumerated,
// non-promoted fields; the adapter owns the K<->map conversion, so this package
// stays generic and never reflects over domain types.
type Envelope[K any] struct {
	V       int               `json:"v"`
	Known   K                 `json:"known"`
	Unknown map[string]string `json:"unknown,omitempty"`
}

// Encode serializes an envelope to its durable JSON form. A zero V is defaulted
// to the current Version so callers need not set it explicitly. Encode does not
// mutate its argument.
func Encode[K any](e Envelope[K]) ([]byte, error) {
	if e.V == 0 {
		e.V = Version
	}
	data, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("extras: encode envelope: %w", err)
	}
	return data, nil
}

// Decode parses a durable extras blob back into a typed envelope.
func Decode[K any](data []byte) (Envelope[K], error) {
	var e Envelope[K]
	if err := json.Unmarshal(data, &e); err != nil {
		return Envelope[K]{}, fmt.Errorf("extras: decode envelope: %w", err)
	}
	return e, nil
}

// Leftover returns a new map containing every entry of meta whose key is not in
// claimed. It never mutates meta and always returns a non-nil map. Adapters use
// it to compute the Unknown passthrough: claimed is the set of metadata keys
// already captured by the entity's promoted columns and its typed Known struct,
// so the result is exactly the keys this binary did not model.
func Leftover(meta map[string]string, claimed ...string) map[string]string {
	skip := make(map[string]struct{}, len(claimed))
	for _, k := range claimed {
		skip[k] = struct{}{}
	}
	out := make(map[string]string, len(meta))
	for k, v := range meta {
		if _, ok := skip[k]; ok {
			continue
		}
		out[k] = v
	}
	return out
}

// Union merges parts into a single new map and is the reconstruct-union
// primitive: an adapter rebuilds an entity's full metadata map by unioning its
// promoted columns, its Known fields, and its Unknown passthrough. A key that
// appears in more than one part is reported as an error regardless of value —
// the double-write/drift guard — so a field that has drifted into both Known and
// Unknown can never silently double-write or diverge. Nil and empty parts are
// ignored. parts are not mutated.
func Union(parts ...map[string]string) (map[string]string, error) {
	out := make(map[string]string)
	for _, p := range parts {
		for k, v := range p {
			if _, dup := out[k]; dup {
				return nil, fmt.Errorf("extras: duplicate key %q across parts (double-write/drift)", k)
			}
			out[k] = v
		}
	}
	return out, nil
}
