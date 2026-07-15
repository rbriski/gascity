package nudgequeue

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestCommandIndexEntryIsAnExactKnownOrOpaqueTaggedUnion(t *testing.T) {
	entryType := reflect.TypeOf(CommandIndexEntry{})
	if got, want := entryType.NumField(), 2; got != want {
		t.Fatalf("CommandIndexEntry field count = %d, want %d", got, want)
	}
	if field, ok := entryType.FieldByName("Command"); !ok || field.Type != reflect.TypeOf((*Command)(nil)) {
		t.Fatalf("CommandIndexEntry.Command = %#v, want *Command", field)
	}
	if field, ok := entryType.FieldByName("Opaque"); !ok || field.Type != reflect.TypeOf((*OpaqueCommand)(nil)) {
		t.Fatalf("CommandIndexEntry.Opaque = %#v, want *OpaqueCommand", field)
	}

	opaqueType := reflect.TypeOf(OpaqueCommand{})
	if got, want := opaqueType.NumField(), 3; got != want {
		t.Fatalf("OpaqueCommand field count = %d, want %d", got, want)
	}
	for _, fieldName := range []string{"Version", "Routing", "Raw"} {
		if _, ok := opaqueType.FieldByName(fieldName); !ok {
			t.Fatalf("OpaqueCommand is missing %s", fieldName)
		}
	}

	known := indexTestCommand("command-1", "session-a", 1, 1, CommandStatePending)
	opaque := opaqueIndexTestCommand(t, 2, "command-1", "session-a", 1, 1, 1, "first")
	for name, entry := range map[string]CommandIndexEntry{
		"neither": {},
		"both":    {Command: &known, Opaque: &opaque},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := BuildCommandIndex(CommandIndexSnapshot{
				Store:             indexTestStoreBinding(),
				Entries:           []CommandIndexEntry{entry},
				Revision:          1,
				SequenceHighWater: 1,
			})
			if err == nil || !strings.Contains(err.Error(), "exactly one") {
				t.Fatalf("BuildCommandIndex error = %v, want exact tagged-union rejection", err)
			}
		})
	}
}

func TestCommandIndexMixedKnownAndOpaqueEntriesBlockOnlyTheirSession(t *testing.T) {
	knownBefore := knownIndexTestEntry(indexTestCommand("command-1", "session-a", 1, 1, CommandStatePending))
	opaque := opaqueIndexTestEntry(t, 2, "command-2", "session-a", 1, 2, 2, "opaque")
	knownAfter := knownIndexTestEntry(indexTestCommand("command-3", "session-a", 3, 3, CommandStatePending))
	unrelated := knownIndexTestEntry(indexTestCommand("command-4", "session-b", 4, 4, CommandStatePending))

	index, err := BuildCommandIndex(CommandIndexSnapshot{
		Store:             indexTestStoreBinding(),
		Entries:           []CommandIndexEntry{knownAfter, unrelated, opaque, knownBefore},
		Revision:          4,
		SequenceHighWater: 4,
	})
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}

	page, err := index.Page("session-a", 0, MaxCommandIndexPageSize)
	if err != nil {
		t.Fatalf("Page(session-a): %v", err)
	}
	if got, want := indexEntryIDs(page.Entries), []string{"command-1", "command-2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("session-a page IDs = %v, want %v", got, want)
	}
	if page.NextAfterSequence != 0 {
		t.Fatalf("opaque barrier continuation = %d, want zero", page.NextAfterSequence)
	}
	if page.Entries[0].Command == nil || page.Entries[0].Opaque != nil {
		t.Fatalf("first entry = %#v, want known command", page.Entries[0])
	}
	if page.Entries[1].Opaque == nil || page.Entries[1].Command != nil {
		t.Fatalf("second entry = %#v, want opaque command", page.Entries[1])
	}
	if _, err := index.Page("session-a", 2, MaxCommandIndexPageSize); !errors.Is(err, ErrCommandIndexOpaqueBarrier) {
		t.Fatalf("Page past opaque barrier error = %v, want ErrCommandIndexOpaqueBarrier", err)
	}

	unrelatedPage, err := index.Page("session-b", 0, MaxCommandIndexPageSize)
	if err != nil {
		t.Fatalf("Page(session-b): %v", err)
	}
	if got, want := indexEntryIDs(unrelatedPage.Entries), []string{"command-4"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("session-b page IDs = %v, want %v", got, want)
	}

	resolution, err := index.Resolve("command-2")
	if err != nil {
		t.Fatalf("Resolve(command-2): %v", err)
	}
	if !resolution.Found || resolution.Entry.Opaque == nil || !bytes.Equal(resolution.Entry.Opaque.Raw, opaque.Opaque.Raw) {
		t.Fatalf("opaque resolution = %#v, want exact opaque entry", resolution)
	}
}

func TestCommandIndexKnownUpgradeRequiredAlsoBlocksOnlyItsSession(t *testing.T) {
	before := indexTestCommand("command-1", "session-a", 1, 1, CommandStatePending)
	barrier := indexTestCommand("command-2", "session-a", 2, 2, CommandStateUpgradeRequired)
	after := indexTestCommand("command-3", "session-a", 3, 3, CommandStatePending)
	unrelated := indexTestCommand("command-4", "session-b", 4, 4, CommandStatePending)
	index, err := BuildCommandIndex(indexTestSnapshot([]Command{after, unrelated, barrier, before}, 4))
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}

	page, err := index.Page("session-a", 0, MaxCommandIndexPageSize)
	if err != nil {
		t.Fatalf("Page(session-a): %v", err)
	}
	if got, want := indexEntryIDs(page.Entries), []string{"command-1", "command-2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("session-a page IDs = %v, want %v", got, want)
	}
	if page.NextAfterSequence != 0 {
		t.Fatalf("upgrade barrier continuation = %d, want zero", page.NextAfterSequence)
	}
	if _, err := index.Page("session-a", 2, MaxCommandIndexPageSize); !errors.Is(err, ErrCommandIndexUpgradeBarrier) {
		t.Fatalf("Page past known upgrade barrier error = %v, want ErrCommandIndexUpgradeBarrier", err)
	}
	unrelatedPage, err := index.Page("session-b", 0, MaxCommandIndexPageSize)
	if err != nil || !reflect.DeepEqual(indexEntryIDs(unrelatedPage.Entries), []string{"command-4"}) {
		t.Fatalf("unrelated page = %#v, err=%v", unrelatedPage, err)
	}
}

func TestCommandIndexOpaqueBarrierSurvivesMutationAndPagination(t *testing.T) {
	index, err := BuildCommandIndex(CommandIndexSnapshot{Store: indexTestStoreBinding()})
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}
	known1 := knownIndexTestEntry(indexTestCommand("command-1", "session-a", 1, 1, CommandStatePending))
	opaque2 := opaqueIndexTestEntry(t, 2, "command-2", "session-a", 1, 2, 2, "opaque")
	known3 := knownIndexTestEntry(indexTestCommand("command-3", "session-a", 3, 3, CommandStatePending))
	for revision, entry := range []CommandIndexEntry{known1, opaque2, known3} {
		entry := entry
		if err := index.Apply(CommandIndexMutation{
			Store:    indexTestStoreBinding(),
			Revision: uint64(revision + 1),
			Entry:    &entry,
		}); err != nil {
			t.Fatalf("Apply revision %d: %v", revision+1, err)
		}
	}

	first, err := index.Page("session-a", 0, 1)
	if err != nil {
		t.Fatalf("first Page: %v", err)
	}
	if got, want := indexEntryIDs(first.Entries), []string{"command-1"}; !reflect.DeepEqual(got, want) || first.NextAfterSequence != 1 {
		t.Fatalf("first page = IDs %v next %d, want [command-1] next 1", got, first.NextAfterSequence)
	}
	second, err := index.Page("session-a", first.NextAfterSequence, 1)
	if err != nil {
		t.Fatalf("second Page: %v", err)
	}
	if got, want := indexEntryIDs(second.Entries), []string{"command-2"}; !reflect.DeepEqual(got, want) || second.NextAfterSequence != 0 {
		t.Fatalf("second page = IDs %v next %d, want [command-2] next 0", got, second.NextAfterSequence)
	}
}

func TestCommandIndexOpaqueRawIsDecodeValidatedAndDeepCopied(t *testing.T) {
	source := opaqueIndexTestEntry(t, 2, "command-1", "session-a", 1, 1, 1, "owned")
	wantRaw := append([]byte(nil), source.Opaque.Raw...)
	index, err := BuildCommandIndex(CommandIndexSnapshot{
		Store:             indexTestStoreBinding(),
		Entries:           []CommandIndexEntry{source},
		Revision:          1,
		SequenceHighWater: 1,
	})
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}
	source.Opaque.Raw[0] ^= 0xff

	resolution, err := index.Resolve("command-1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !bytes.Equal(resolution.Entry.Opaque.Raw, wantRaw) {
		t.Fatalf("resolved raw = %q, want %q", resolution.Entry.Opaque.Raw, wantRaw)
	}
	resolution.Entry.Opaque.Raw[0] ^= 0xff
	page, err := index.Page("session-a", 0, 1)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if !bytes.Equal(page.Entries[0].Opaque.Raw, wantRaw) {
		t.Fatal("Resolve result aliases indexed opaque raw")
	}
	page.Entries[0].Opaque.Raw[0] ^= 0xff
	resolution, err = index.Resolve("command-1")
	if err != nil || !bytes.Equal(resolution.Entry.Opaque.Raw, wantRaw) {
		t.Fatalf("Page result aliases indexed opaque raw: resolution=%#v err=%v", resolution, err)
	}

	updated := opaqueIndexTestEntry(t, 2, "command-1", "session-a", 1, 1, 2, "mutation-owned")
	wantUpdatedRaw := append([]byte(nil), updated.Opaque.Raw...)
	if err := index.Apply(CommandIndexMutation{Store: indexTestStoreBinding(), Revision: 2, Entry: &updated}); err != nil {
		t.Fatalf("Apply opaque update: %v", err)
	}
	updated.Opaque.Raw[0] ^= 0xff
	resolution, err = index.Resolve("command-1")
	if err != nil || !bytes.Equal(resolution.Entry.Opaque.Raw, wantUpdatedRaw) {
		t.Fatalf("mutation input aliases indexed opaque raw: resolution=%#v err=%v", resolution, err)
	}
}

func TestCommandIndexRejectsForgedOpaqueMetadata(t *testing.T) {
	base := opaqueIndexTestEntry(t, 2, "command-1", "session-a", 1, 1, 1, "base")
	tests := map[string]func(*OpaqueCommand){
		"version":        func(command *OpaqueCommand) { command.Version++ },
		"command id":     func(command *OpaqueCommand) { command.Routing.CommandID = "forged-command" },
		"store uuid":     func(command *OpaqueCommand) { command.Routing.Store.StoreUUID = "forged-store" },
		"restore epoch":  func(command *OpaqueCommand) { command.Routing.Store.RestoreEpoch++ },
		"target session": func(command *OpaqueCommand) { command.Routing.TargetSessionID = "forged-session" },
		"generation":     func(command *OpaqueCommand) { command.Routing.IntentGeneration++ },
		"sequence":       func(command *OpaqueCommand) { command.Routing.Sequence++ },
		"revision":       func(command *OpaqueCommand) { command.Routing.Revision++ },
		"raw routing": func(command *OpaqueCommand) {
			command.Raw = bytes.Replace(command.Raw, []byte(`"session_id":"session-a"`), []byte(`"session_id":"raw-forgery"`), 1)
		},
		"invalid known raw": func(command *OpaqueCommand) {
			known := indexTestCommand("command-1", "session-a", 1, 1, CommandStatePending)
			known.Message = ""
			command.Version = CommandVersion1
			command.Routing = commandRoutingHeader(known)
			command.Raw, _ = json.Marshal(known)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			entry := cloneIndexTestEntry(base)
			mutate(entry.Opaque)
			_, err := BuildCommandIndex(CommandIndexSnapshot{
				Store:             indexTestStoreBinding(),
				Entries:           []CommandIndexEntry{entry},
				Revision:          max(uint64(1), entry.Opaque.Routing.Revision),
				SequenceHighWater: max(uint64(1), entry.Opaque.Routing.Sequence),
			})
			if err == nil {
				t.Fatal("BuildCommandIndex accepted forged opaque metadata")
			}
		})
	}
}

func TestCommandIndexOpaqueRevisionConservesIdentityAndCannotBeNormalized(t *testing.T) {
	initial := opaqueIndexTestEntry(t, 2, "command-1", "session-a", 7, 1, 1, "initial")
	updated := opaqueIndexTestEntry(t, 2, "command-1", "session-a", 7, 1, 2, "updated")
	index := buildOpaqueIndexTest(t, initial)
	if err := index.Apply(CommandIndexMutation{Store: indexTestStoreBinding(), Revision: 2, Entry: &updated}); err != nil {
		t.Fatalf("Apply opaque revision: %v", err)
	}
	resolution, err := index.Resolve("command-1")
	if err != nil || !bytes.Equal(resolution.Entry.Opaque.Raw, updated.Opaque.Raw) {
		t.Fatalf("updated opaque resolution = %#v, err=%v", resolution, err)
	}

	for name, replacement := range map[string]CommandIndexEntry{
		"known v1":        knownIndexTestEntry(indexTestCommand("command-1", "session-a", 1, 2, CommandStatePending)),
		"terminalized v1": knownIndexTestEntry(indexTestCommand("command-1", "session-a", 1, 2, CommandStateDelivered)),
		"new id":          opaqueIndexTestEntry(t, 2, "command-2", "session-a", 7, 1, 2, "changed-id"),
		"session":         opaqueIndexTestEntry(t, 2, "command-1", "session-b", 7, 1, 2, "changed-session"),
		"generation":      opaqueIndexTestEntry(t, 2, "command-1", "session-a", 8, 1, 2, "changed-generation"),
		"sequence":        opaqueIndexTestEntry(t, 2, "command-1", "session-a", 7, 2, 2, "changed-sequence"),
		"version":         opaqueIndexTestEntry(t, 3, "command-1", "session-a", 7, 1, 2, "changed-version"),
	} {
		t.Run(name, func(t *testing.T) {
			index := buildOpaqueIndexTest(t, initial)
			replacement := replacement
			if err := index.Apply(CommandIndexMutation{Store: indexTestStoreBinding(), Revision: 2, Entry: &replacement}); !errors.Is(err, ErrCommandIndexUnsynced) {
				t.Fatalf("Apply identity rewrite error = %v, want ErrCommandIndexUnsynced", err)
			}
		})
	}

	foreign := opaqueIndexTestEntry(t, 2, "command-1", "session-a", 7, 1, 2, "changed-store")
	foreign.Opaque.Raw = bytes.Replace(foreign.Opaque.Raw, []byte(`"store_uuid":"store-1"`), []byte(`"store_uuid":"store-2"`), 1)
	decoded := DecodeCommand(foreign.Opaque.Raw)
	if decoded.Disposition != CommandDecodeUpgradeRequired {
		t.Fatalf("DecodeCommand foreign-store fixture = %#v", decoded)
	}
	foreign.Opaque.Version = decoded.Version
	foreign.Opaque.Routing = decoded.Routing
	foreign.Opaque.Raw = decoded.Raw
	index = buildOpaqueIndexTest(t, initial)
	if err := index.Apply(CommandIndexMutation{Store: decoded.Routing.Store, Revision: 2, Entry: &foreign}); !errors.Is(err, ErrCommandIndexUnsynced) {
		t.Fatalf("Apply opaque store rewrite error = %v, want ErrCommandIndexUnsynced", err)
	}
}

func TestCommandIndexOldOwnerCannotTombstoneOpaqueEntry(t *testing.T) {
	initial := opaqueIndexTestEntry(t, 2, "command-1", "session-a", 7, 1, 1, "initial")

	for name, priorVersion := range map[string]uint32{
		"truthful opaque version": 2,
		"forged known version":    CommandVersion1,
	} {
		t.Run(name, func(t *testing.T) {
			index := buildOpaqueIndexTest(t, initial)
			tombstone := CommandIndexTombstone{
				CommandID:        "command-1",
				Store:            indexTestStoreBinding(),
				Revision:         2,
				PriorVersion:     priorVersion,
				PriorRevision:    1,
				PriorState:       CommandStateDelivered,
				TargetSessionID:  "session-a",
				IntentGeneration: 7,
				Sequence:         1,
			}
			if err := index.Apply(CommandIndexMutation{Store: indexTestStoreBinding(), Revision: 2, Tombstone: &tombstone}); err == nil {
				t.Fatal("Apply accepted opaque tombstone")
			}
		})
	}
}

func TestCommandIndexAuditPreservesOpaqueIdentityAndRaw(t *testing.T) {
	initial := opaqueIndexTestEntry(t, 2, "command-1", "session-a", 7, 1, 1, "initial")
	updated := opaqueIndexTestEntry(t, 2, "command-1", "session-a", 7, 1, 2, "updated")
	index := buildOpaqueIndexTest(t, initial)

	installed, err := index.CompleteAudit(1, CommandIndexSnapshot{
		Store:             indexTestStoreBinding(),
		Entries:           []CommandIndexEntry{updated},
		Revision:          2,
		SequenceHighWater: 1,
	})
	if err != nil || !installed {
		t.Fatalf("CompleteAudit updated opaque: installed=%t err=%v", installed, err)
	}
	resolution, err := index.Resolve("command-1")
	if err != nil || !bytes.Equal(resolution.Entry.Opaque.Raw, updated.Opaque.Raw) {
		t.Fatalf("audited opaque resolution = %#v, err=%v", resolution, err)
	}
	wantAuditedRaw := append([]byte(nil), updated.Opaque.Raw...)
	updated.Opaque.Raw[0] ^= 0xff
	resolution, err = index.Resolve("command-1")
	if err != nil || !bytes.Equal(resolution.Entry.Opaque.Raw, wantAuditedRaw) {
		t.Fatalf("audit input aliases indexed opaque raw: resolution=%#v err=%v", resolution, err)
	}

	forgedTombstone := CommandIndexTombstone{
		CommandID:        "command-1",
		Store:            indexTestStoreBinding(),
		Revision:         3,
		PriorVersion:     CommandVersion1,
		PriorRevision:    2,
		PriorState:       CommandStateDelivered,
		TargetSessionID:  "session-a",
		IntentGeneration: 7,
		Sequence:         1,
	}
	if installed, err := index.CompleteAudit(2, CommandIndexSnapshot{
		Store:             indexTestStoreBinding(),
		Tombstones:        []CommandIndexTombstone{forgedTombstone},
		Revision:          3,
		SequenceHighWater: 1,
	}); err == nil || installed {
		t.Fatalf("CompleteAudit accepted opaque tombstone: installed=%t err=%v", installed, err)
	}

	for name, replacement := range map[string]CommandIndexEntry{
		"session rewrite": opaqueIndexTestEntry(t, 2, "command-1", "session-b", 7, 1, 3, "forged"),
		"normalization":   knownIndexTestEntry(indexTestCommand("command-1", "session-a", 1, 3, CommandStatePending)),
	} {
		t.Run(name, func(t *testing.T) {
			replacement := replacement
			installed, err := index.CompleteAudit(2, CommandIndexSnapshot{
				Store:             indexTestStoreBinding(),
				Entries:           []CommandIndexEntry{replacement},
				Revision:          3,
				SequenceHighWater: 1,
			})
			if err == nil || installed {
				t.Fatalf("CompleteAudit accepted opaque rewrite: installed=%t err=%v", installed, err)
			}
		})
	}
}

func TestCommandIndexInvalidKnownRecordCannotHideAsOpaqueOrDisappear(t *testing.T) {
	known := indexTestCommand("command-1", "session-a", 1, 1, CommandStatePending)
	known.Message = ""
	wire, err := json.Marshal(known)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	decoded := DecodeCommand(wire)
	if decoded.Disposition != CommandDecodeDeadLetter || decoded.DeadLetterReason != CommandDeadLetterInvalidKnownVersion {
		t.Fatalf("DecodeCommand = %#v, want invalid-known dead letter", decoded)
	}
	opaque := OpaqueCommand{Version: CommandVersion1, Routing: commandRoutingHeader(known), Raw: wire}
	if _, err := BuildCommandIndex(CommandIndexSnapshot{
		Store:             indexTestStoreBinding(),
		Entries:           []CommandIndexEntry{{Opaque: &opaque}},
		Revision:          1,
		SequenceHighWater: 1,
	}); err == nil {
		t.Fatal("BuildCommandIndex parked invalid known command as opaque")
	}
	if _, err := BuildCommandIndex(CommandIndexSnapshot{
		Store:             indexTestStoreBinding(),
		Revision:          1,
		SequenceHighWater: 1,
	}); err == nil {
		t.Fatal("BuildCommandIndex accepted missing invalid-known sequence")
	}
}

func knownIndexTestEntry(command Command) CommandIndexEntry {
	command = cloneIndexTestCommand(command)
	return CommandIndexEntry{Command: &command}
}

func opaqueIndexTestEntry(t *testing.T, version uint32, id, sessionID string, generation, sequence, revision uint64, marker string) CommandIndexEntry {
	t.Helper()
	opaque := opaqueIndexTestCommand(t, version, id, sessionID, generation, sequence, revision, marker)
	return CommandIndexEntry{Opaque: &opaque}
}

func opaqueIndexTestCommand(t *testing.T, version uint32, id, sessionID string, generation, sequence, revision uint64, marker string) OpaqueCommand {
	t.Helper()
	raw := []byte(fmt.Sprintf(
		"  \n{\"version\":%d,\"id\":%q,\"store\":{\"store_uuid\":\"store-1\",\"restore_epoch\":1},\"target\":{\"session_id\":%q,\"intent_generation\":%d},\"order\":{\"sequence\":%d,\"revision\":%d},\"future\":{\"marker\":%q}}\t",
		version,
		id,
		sessionID,
		generation,
		sequence,
		revision,
		marker,
	))
	decoded := DecodeCommand(raw)
	if decoded.Disposition != CommandDecodeUpgradeRequired {
		t.Fatalf("DecodeCommand test fixture = %#v, want upgrade-required", decoded)
	}
	return OpaqueCommand{Version: decoded.Version, Routing: decoded.Routing, Raw: decoded.Raw}
}

func cloneIndexTestEntry(entry CommandIndexEntry) CommandIndexEntry {
	if entry.Command != nil {
		command := cloneIndexTestCommand(*entry.Command)
		entry.Command = &command
	}
	if entry.Opaque != nil {
		opaque := *entry.Opaque
		opaque.Raw = append([]byte(nil), opaque.Raw...)
		entry.Opaque = &opaque
	}
	return entry
}

func buildOpaqueIndexTest(t *testing.T, entry CommandIndexEntry) *CommandIndex {
	t.Helper()
	index, err := BuildCommandIndex(CommandIndexSnapshot{
		Store:             indexTestStoreBinding(),
		Entries:           []CommandIndexEntry{entry},
		Revision:          entry.Opaque.Routing.Revision,
		SequenceHighWater: entry.Opaque.Routing.Sequence,
	})
	if err != nil {
		t.Fatalf("BuildCommandIndex: %v", err)
	}
	return index
}

func indexEntryIDs(entries []CommandIndexEntry) []string {
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		switch {
		case entry.Command != nil:
			ids = append(ids, entry.Command.ID)
		case entry.Opaque != nil:
			ids = append(ids, entry.Opaque.Routing.CommandID)
		default:
			ids = append(ids, "<invalid>")
		}
	}
	return ids
}
