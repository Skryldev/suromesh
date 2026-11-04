package tlgen

import (
	"fmt"
	"sort"
)

// manualIDs are definitions hand-written in package tl; the generator drops
// them from emission and wires their dispatch cases to the tl package.
// gzip_packed, msg_container, rpc_result are commented out in the schema and
// never parse; rpc_error does parse and is dropped here.
var manualIDs = map[uint32]string{
	0x3072cfa1: "GzipPacked",
	0x73f1f8dc: "MessageContainer",
	0xf35c6d01: "RpcResult",
	0x2144ca19: "RpcError",
}

// Validate merges schemas, applies manual overrides, and enforces every
// structural rule loudly: duplicates, dangling references, bad flags,
// ambiguous names, and unsupported constructs. It returns the merged schema
// ready for codegen.
func Validate(schemas ...*Schema) (*Schema, error) {
	out := &Schema{Layer: 0}
	for _, s := range schemas {
		if s.Layer != 0 {
			if out.Layer != 0 && out.Layer != s.Layer {
				return nil, fmt.Errorf("conflicting LAYER headers: %d vs %d", out.Layer, s.Layer)
			}
			out.Layer = s.Layer
		}
		out.Types = append(out.Types, s.Types...)
		out.Functions = append(out.Functions, s.Functions...)
	}

	// Drop manual definitions, remembering their dispatch delegation.
	var keptTypes, keptFuncs []*Def
	manual := map[uint32]bool{}
	for _, d := range out.Types {
		if _, ok := manualIDs[d.ID]; ok {
			manual[d.ID] = true
			continue
		}
		keptTypes = append(keptTypes, d)
	}
	for _, d := range out.Functions {
		if _, ok := manualIDs[d.ID]; ok {
			manual[d.ID] = true
			continue
		}
		keptFuncs = append(keptFuncs, d)
	}
	out.Types, out.Functions = keptTypes, keptFuncs
	out.manual = manual

	structural := map[string]bool{}
	for _, s := range schemas {
		for _, key := range s.Structural {
			structural[key] = true
			out.Structural = append(out.Structural, key)
		}
	}

	byID := map[uint32]*Def{}
	byName := map[string]*Def{}
	for _, d := range append(append([]*Def{}, out.Types...), out.Functions...) {
		if prev, dup := byID[d.ID]; dup {
			return nil, fmt.Errorf("duplicate constructor id %#08x: %s and %s", d.ID, prev, d)
		}
		if prev, dup := byName[d.Qualified()]; dup {
			return nil, fmt.Errorf("duplicate definition name %s: %s and %s", d.Qualified(), prev, d)
		}
		byID[d.ID] = d
		byName[d.Qualified()] = d
	}

	// Abstract type table: qualified result name -> constructors.
	abstractCtors := map[string][]*Def{}
	for _, d := range out.Types {
		if d.Result.Kind != KindNamed || d.Result.Primitive() {
			continue
		}
		key := d.Result.Qualified()
		abstractCtors[key] = append(abstractCtors[key], d)
	}

	// Structural rules per definition.
	for _, d := range out.Types {
		if err := validateDef(d); err != nil {
			return nil, err
		}
		switch {
		case d.Result.Kind == KindNamed && (d.Result.Primitive() || d.Result.Name == "Bool"):
			// boolTrue/boolFalse (= Bool), true (= True): structs without an
			// abstract interface.
		case d.Result.Kind != KindNamed:
			return nil, fmt.Errorf("%s: type constructor must have a named result, got %q", d, d.Result.Qualified())
		}
	}
	for _, d := range out.Functions {
		if err := validateDef(d); err != nil {
			return nil, err
		}
	}

	// Resolve every reference.
	defNames := map[string]bool{}
	for _, d := range append(append([]*Def{}, out.Types...), out.Functions...) {
		defNames[d.Qualified()] = true
	}
	for _, d := range append(append([]*Def{}, out.Types...), out.Functions...) {
		for _, a := range d.Args {
			if err := resolveRef(d, a, &a.Type, abstractCtors, defNames, structural, false); err != nil {
				return nil, err
			}
		}
	}

	// Naming pass + collision detection.
	structNames := map[string]*Def{}
	for _, d := range out.Types {
		d.GoName = goTypeDefName(d)
	}
	for _, d := range out.Functions {
		d.GoName = goFuncName(d)
	}
	for _, d := range append(append([]*Def{}, out.Types...), out.Functions...) {
		if prev, dup := structNames[d.GoName]; dup {
			return nil, fmt.Errorf("ambiguous Go type name %q: %s and %s", d.GoName, prev, d)
		}
		structNames[d.GoName] = d
		if err := validateFieldNames(d); err != nil {
			return nil, err
		}
	}

	out.abstractCtors = abstractCtors
	return out, nil
}

func validateDef(d *Def) error {
	for _, a := range d.Args {
		// "true" is only legal as an optional flag marker.
		if a.Type.Kind == KindFlagTrue && a.FlagWord == "" {
			return fmt.Errorf("%s: arg %q: the true type requires a flags.N? prefix", d, a.Name)
		}
		// "#" is only legal as a flag word.
		if a.Type.Kind == KindFlagWord && a.FlagWord != "" {
			return fmt.Errorf("%s: arg %q: a flag word cannot be optional", d, a.Name)
		}
		// Generic references are only legal on request wrappers.
		if (a.Type.Kind == KindGenericRef || a.Type.Kind == KindBoxedGeneric) && d.Result.Kind != KindGenericRef {
			return fmt.Errorf("%s: arg %q: generic %q outside a !X wrapper", d, a.Name, a.Type.Name)
		}
		if a.Type.Kind == KindBoxedGeneric && d.Kind != KindFunction {
			return nil_or_errorf("%s: arg %q: boxed generic on a type constructor", d, a.Name)
		}
		// Optional args must reference a declared flag word that precedes them.
		if a.FlagWord != "" {
			found := false
			for _, w := range d.Args {
				if w.Name == a.FlagWord {
					if w.Type.Kind != KindFlagWord {
						return nil_or_errorf("%s: arg %q: flag word %q does not have the # type", d, a.Name, a.FlagWord)
					}
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("%s: arg %q: references missing flag word %q", d, a.Name, a.FlagWord)
			}
		}
	}
	// Flag words must be declared before the fields that use them.
	for i, a := range d.Args {
		if a.FlagWord == "" {
			continue
		}
		for j, w := range d.Args {
			if w.Name == a.FlagWord && j > i {
				return fmt.Errorf("%s: arg %q uses flag word %q declared later", d, a.Name, a.FlagWord)
			}
		}
	}
	// Bare constructor references are only legal as elements of bare vectors.
	var walk func(t *TypeRef, inBareVector bool) error
	walk = func(t *TypeRef, inBareVector bool) error {
		if t == nil {
			return nil
		}
		if t.Kind == KindNamed && t.Bare && !t.Primitive() && !inBareVector {
			return fmt.Errorf("%s: bare constructor reference %q outside a bare vector", d, t.Qualified())
		}
		if t.Kind == KindVector {
			return walk(t.Elem, t.Bare)
		}
		return walk(t.Elem, false)
	}
	for _, a := range d.Args {
		if err := walk(&a.Type, false); err != nil {
			return err
		}
	}
	return nil
}

func resolveRef(d *Def, a *Arg, t *TypeRef, abstract map[string][]*Def, defNames, structural map[string]bool, inVector bool) error {
	switch t.Kind {
	case KindNamed:
		if t.Primitive() || t.Name == "Bool" {
			return nil
		}
		if structural[t.Qualified()] {
			return fmt.Errorf("%s: arg %q: references structural-only type %q (no constructor id)", d, a.Name, t.Qualified())
		}
		if t.Bare {
			// A bare reference names a concrete definition by its bare
			// (lowercase) TL name, not an abstract result type.
			if !defNames[t.Qualified()] {
				return fmt.Errorf("%s: arg %q: unknown bare type %q", d, a.Name, t.Qualified())
			}
			return nil
		}
		if len(abstract[t.Qualified()]) == 0 {
			return fmt.Errorf("%s: arg %q: unknown type %q", d, a.Name, t.Qualified())
		}
		return nil
	case KindVector:
		return resolveRef(d, a, t.Elem, abstract, defNames, structural, true)
	case KindFlagWord, KindFlagTrue, KindGenericRef, KindBoxedGeneric:
		return nil
	}
	return fmt.Errorf("%s: arg %q: unsupported type kind %d", d, a.Name, t.Kind)
}

func validateFieldNames(d *Def) error {
	seen := map[string]string{}
	for _, a := range d.Args {
		if a.Type.Kind == KindFlagWord {
			continue // flag words are not fields
		}
		g := goFieldName(a.Name)
		a.GoName = g
		if prev, dup := seen[g]; dup {
			return fmt.Errorf("%s: ambiguous Go field name %q from args %q and %q", d, g, prev, a.Name)
		}
		seen[g] = a.Name
	}
	return nil
}

func nil_or_errorf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}

// AbstractTypes returns the abstract result types with their constructor
// lists, sorted by name for deterministic iteration.
func (s *Schema) AbstractTypes() []AbstractType {
	keys := make([]string, 0, len(s.abstractCtors))
	for k := range s.abstractCtors {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]AbstractType, 0, len(keys))
	for _, k := range keys {
		out = append(out, AbstractType{Key: k, Ctors: s.abstractCtors[k]})
	}
	return out
}

// AbstractType is one abstract result type and its constructors.
type AbstractType struct {
	Key   string // "auth.SentCode"
	Ctors []*Def
}
