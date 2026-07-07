// Package ir models the emitted Lumen IR contract (lumen.ir) that gascity
// consumes and executes natively. It is the load-time decode surface for
// `.lumen.json` documents produced upstream by donbox/formula-language's
// compileLumenFormulaLanguage. The parser/compiler stay upstream; gc consumes
// the emitted IR.
//
// SPIKE S0.1: this package currently proves the IR is tractable to model in Go
// and pinnable to a contract version. It types the envelope + node taxonomy and
// preserves per-kind payloads verbatim (typed per-kind payload structs are
// Phase 1 work, derived from docs/spec/ir.lumen).
package ir

// ContractName is the fixed contract identifier every lumen.ir document carries.
const ContractName = "lumen.ir"

// SupportedVersions is the set of lumen.ir contract versions this build decodes.
// A set (not a single const) so a migration window can hold two. Decode fails
// on any version outside this set — at load time, never at run time.
var SupportedVersions = map[string]bool{
	"0.2.5": true,
}

// Origin is the source position every declaration-layer shape and node carries.
type Origin struct {
	URI  string `json:"uri"`
	Line int    `json:"line"`
	Col  int    `json:"col"`
}

// Contract is the envelope identity: {name, version, producer}.
type Contract struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Producer string `json:"producer"`
}

// InputDecl is the formula's typed input schema (the `accepts` block).
type InputDecl struct {
	Name   string  `json:"name"`
	Fields []Field `json:"fields"`
	Origin Origin  `json:"origin"`
}

// Field is one typed input field.
type Field struct {
	Name     string `json:"name"`
	Type     Type   `json:"type"`
	Required bool   `json:"required"`
	Default  any    `json:"default,omitempty"`
	Body     bool   `json:"body"`
	Origin   Origin `json:"origin"`
}

// Type is the Lumen type meta-model node. Only Kind is required; the remaining
// fields are populated per kind. Modeled loosely for the spike.
type Type struct {
	Kind    TypeKind `json:"kind"`
	Name    string   `json:"name,omitempty"`
	Element *Type    `json:"element,omitempty"`
	Of      []Type   `json:"of,omitempty"`
	Fields  []Field  `json:"fields,omitempty"`
	Origin  *Origin  `json:"origin,omitempty"`
}

// NodeKind is one of the closed set of emitted IR node kinds.
type NodeKind string

// The 26 emitted node kinds (schema definitions/node/kind enum, lumen.ir 0.2.5).
// EMITTED-ONLY (not public step types): do (lowering of prompt), settle, for-each.
const (
	NodeAsync       NodeKind = "async"
	NodeAwait       NodeKind = "await"
	NodeBlock       NodeKind = "block"
	NodeCancel      NodeKind = "cancel"
	NodeChannel     NodeKind = "channel"
	NodeCleanup     NodeKind = "cleanup"
	NodeClose       NodeKind = "close"
	NodeDispatch    NodeKind = "dispatch"
	NodeDo          NodeKind = "do"
	NodeExec        NodeKind = "exec"
	NodeFailChannel NodeKind = "fail-channel"
	NodeForEach     NodeKind = "for-each"
	NodeGather      NodeKind = "gather"
	NodeGuard       NodeKind = "guard"
	NodeInterp      NodeKind = "interp"
	NodeLit         NodeKind = "lit"
	NodeMap         NodeKind = "map"
	NodeQuote       NodeKind = "quote"
	NodeRaise       NodeKind = "raise"
	NodeRecover     NodeKind = "recover"
	NodeRepeat      NodeKind = "repeat"
	NodeRetry       NodeKind = "retry"
	NodeRun         NodeKind = "run"
	NodeScatter     NodeKind = "scatter"
	NodeSettle      NodeKind = "settle"
	NodeTimeout     NodeKind = "timeout"
)

// KnownNodeKinds is the closed emitted node-kind set. A kind outside this set is
// a decode error — the "unknown kind is a load error, not a runtime surprise"
// property. This slice is the intended lockstep anchor against the schema enum.
var KnownNodeKinds = map[NodeKind]bool{
	NodeAsync: true, NodeAwait: true, NodeBlock: true, NodeCancel: true,
	NodeChannel: true, NodeCleanup: true, NodeClose: true, NodeDispatch: true,
	NodeDo: true, NodeExec: true, NodeFailChannel: true, NodeForEach: true,
	NodeGather: true, NodeGuard: true, NodeInterp: true, NodeLit: true,
	NodeMap: true, NodeQuote: true, NodeRaise: true, NodeRecover: true,
	NodeRepeat: true, NodeRetry: true, NodeRun: true, NodeScatter: true,
	NodeSettle: true, NodeTimeout: true,
}

// TypeKind is one of the closed set of type meta-model kinds.
type TypeKind string

// The closed set of type meta-model kinds (schema type/kind enum, lumen.ir 0.2.5).
const (
	TypeAtomic  TypeKind = "atomic"
	TypeAlias   TypeKind = "alias"
	TypeLiteral TypeKind = "literal"
	TypeUnion   TypeKind = "union"
	TypeArray   TypeKind = "array"
	TypeRecord  TypeKind = "record"
	TypeChannel TypeKind = "channel"
	TypeHandle  TypeKind = "handle"
)

// KnownTypeKinds is the closed type-kind set.
var KnownTypeKinds = map[TypeKind]bool{
	TypeAtomic: true, TypeAlias: true, TypeLiteral: true, TypeUnion: true,
	TypeArray: true, TypeRecord: true, TypeChannel: true, TypeHandle: true,
}

// ChannelCapability is a channel handle's access facet.
type ChannelCapability string

// The channel handle access facets.
const (
	CapSource ChannelCapability = "source"
	CapSink   ChannelCapability = "sink"
	CapAll    ChannelCapability = "all"
)
