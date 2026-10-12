// Package tlgen parses SPlusthon TL schemas (api.tl / mtproto.tl) into a
// neutral intermediate representation, validates it loudly, and emits
// deterministic Go source.
//
// Pipeline (deliberately decoupled):
//
//	schema text → Parser → IR → Validator → Codegen → Go files
package tlgen

import "fmt"

// Kind distinguishes type constructors from RPC functions.
type Kind int

const (
	KindType Kind = iota
	KindFunction
)

func (k Kind) String() string {
	if k == KindFunction {
		return "function"
	}
	return "type"
}

// Schema is the parsed, validated intermediate representation.
type Schema struct {
	Layer     int    // from the "// LAYER n" header; 0 when absent
	Types     []*Def // in schema order
	Functions []*Def // in schema order

	// Filled by Validate.
	abstractCtors map[string][]*Def
	manual        map[uint32]bool

	// Structural lists result types of id-less structural definitions
	// (e.g. TlsClientHello) that were skipped at parse time. References to
	// them from generated definitions are a validation error.
	Structural []string
}

// Def is one TL definition (constructor or function).
type Def struct {
	Kind      Kind
	Namespace string // "" when unqualified
	Name      string // TL name as written ("sendMessage", "Dialogs")
	ID        uint32
	Args      []*Arg
	Result    TypeRef // declared result type
	Line      int     // 1-based line in the source file, for diagnostics

	// Resolved naming (filled by Validate).
	GoName string

	// Manual marks definitions that are hand-written in package tl and must
	// be excluded from generation (their dispatch case delegates to tl).
	Manual bool
}

// Arg is one argument of a definition.
type Arg struct {
	Name     string
	GoName   string
	FlagWord string // name of the governing flag-word arg, "" if not optional
	FlagBit  int    // -1 when not optional
	Type     TypeRef
	Generic  bool // declared as {t:Type}; only wrappers carry these
}

// TypeKind classifies a type reference.
type TypeKind int

const (
	KindNamed        TypeKind = iota // boxed constructor type or abstract type
	KindVector                       // vector<elem> (bare) or Vector<elem> (boxed)
	KindFlagWord                     // the "#" type
	KindFlagTrue                     // the "true" type
	KindBoxedGeneric                 // "!X" — a boxed RPC request
	KindGenericRef                   // "X" — reference to a {X:Type} parameter
)

// TypeRef is a type reference in the schema.
type TypeRef struct {
	Kind TypeKind
	NS   string // namespace qualifier, "" when none
	Name string // "int", "Dialogs", "auth.SentCode" → NS="auth", Name="SentCode"
	Elem *TypeRef
	Bare bool // element or field referenced in bare (lowercase) form
}

// Primitive reports whether the reference is a wire primitive.
func (t TypeRef) Primitive() bool {
	if t.Kind != KindNamed {
		return false
	}
	switch t.NS + "." + t.Name {
	case ".int", ".long", ".int128", ".int256", ".double", ".float",
		".string", ".bytes", ".Bool", ".true", ".#":
		return true
	}
	return false
}

// Qualified returns the namespace-qualified TL name ("auth.SentCode").
func (t TypeRef) Qualified() string {
	if t.NS == "" {
		return t.Name
	}
	return t.NS + "." + t.Name
}

func (d *Def) String() string {
	return fmt.Sprintf("%s %s#%08x (line %d)", d.Kind, d.Qualified(), d.ID, d.Line)
}

// Qualified returns "ns.Name" or "Name".
func (d *Def) Qualified() string {
	if d.Namespace == "" {
		return d.Name
	}
	return d.Namespace + "." + d.Name
}
