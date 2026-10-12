package tlgen

import (
	"fmt"
)

// genEncodeBody emits the field serialization of a definition, in schema
// order. Flag words are computed from field presence and written at their
// schema position, exactly like the reference implementation's generated
// Python (which computes the flags word at encode time).
func (g *codegen) genEncodeBody(d *Def) error {
	g.out.WriteString("\t// Flag words are derived from field presence.\n")
	for wi, w := range g.flagWords(d) {
		fmt.Fprintf(g.out, "\tvar f%d uint32\n", wi)
		for _, a := range d.Args {
			if a.FlagWord != w.Name {
				continue
			}
			fmt.Fprintf(g.out, "\t%s\n", g.flagBitExprReal(a, fmt.Sprintf("f%d", wi)))
		}
		fmt.Fprintf(g.out, "\tw.PutUint32(f%d)\n", wi)
	}
	for _, a := range nonFlagArgs(d) {
		if err := g.genEncodeField(d, a); err != nil {
			return err
		}
	}
	g.out.WriteString("\treturn w.Err()\n")
	return nil
}

// flagBitExprReal returns a statement that sets the field's bit in word when
// the field is present. Fields sharing a bit each contribute independently,
// so marker/value pairs behave like the reference asserts require.
func (g *codegen) flagBitExprReal(a *Arg, word string) string {
	set := fmt.Sprintf("%s |= 1 << %d", word, a.FlagBit)
	switch {
	case a.Type.Kind == KindFlagTrue:
		return fmt.Sprintf("if x.%s { %s }", a.GoName, set)
	case g.isPointerOptional(a) || g.isSliceOptional(a):
		return fmt.Sprintf("if x.%s != nil { %s }", a.GoName, set)
	default: // interface: nil = absent
		return fmt.Sprintf("if x.%s != nil { %s }", a.GoName, set)
	}
}

func (g *codegen) isPointerOptional(a *Arg) bool {
	if a.FlagWord == "" {
		return false
	}
	t := a.Type
	if t.Kind == KindVector {
		return false
	}
	if t.Kind == KindNamed {
		switch t.Qualified() {
		case "bytes":
			return false
		case "int", "long", "double", "float", "string", "Bool", "true", "int128", "int256":
			return true
		}
	}
	return false // interfaces
}

func (g *codegen) isSliceOptional(a *Arg) bool {
	return a.FlagWord != "" && a.Type.Kind == KindVector
}

func (g *codegen) genEncodeField(d *Def, a *Arg) error {
	acc := fmt.Sprintf("x.%s", a.GoName)
	opt := a.FlagWord != ""
	requiredErr := fmt.Sprintf("return fmt.Errorf(\"tl: %s: field %s is required\")", d.Qualified(), a.GoName)

	switch a.Type.Kind {
	case KindFlagTrue:
		if opt {
			// Marker bool: the bit IS the value; nothing serialized in body.
			return nil
		}
		return fmt.Errorf("%s: non-optional true field", d)
	case KindBoxedGeneric:
		fmt.Fprintf(g.out, "\tif %s == nil {\n\t\t%s\n\t}\n", acc, requiredErr)
		fmt.Fprintf(g.out, "\tif err := %s.Encode(w); err != nil {\n\t\treturn err\n\t}\n", acc)
		return nil
	case KindVector:
		elem := a.Type.Elem
		var loop string
		switch {
		case elem.Kind == KindNamed && !elem.Primitive() && elem.Bare:
			loop = fmt.Sprintf("\tfor i := range %s {\n\t\tif err := (&(%s[i])).encodeBody(w); err != nil {\n\t\t\treturn err\n\t\t}\n\t}\n", acc, acc)
		case elem.Kind == KindNamed && !elem.Primitive():
			loop = fmt.Sprintf("\tfor i := range %s {\n\t\tif %s[i] == nil {\n\t\t\t%s\n\t\t}\n\t\tif err := %s[i].Encode(w); err != nil {\n\t\t\treturn err\n\t\t}\n\t}\n",
				acc, acc, nilElemErr(d, a), acc)
		case elem.Kind == KindNamed && elem.Primitive():
			put, err := g.primitivePut(*elem)
			if err != nil {
				return err
			}
			loop = fmt.Sprintf("\tfor i := range %s {\n\t\t%s\n\t}\n", acc, fmt.Sprintf(put, acc+"[i]"))
		default:
			return fmt.Errorf("%s: unsupported vector element in %q", d, a.GoName)
		}
		// Optional vectors serialize nothing when absent (nil slice);
		// an empty-but-present vector writes the header with count 0.
		body := fmt.Sprintf("\tw.PutVector%sHeader(len(%s))\n%s", map[bool]string{true: "Bare", false: "Boxed"}[a.Type.Bare], acc, loop)
		if a.FlagWord != "" {
			fmt.Fprintf(g.out, "\tif %s != nil {\n%s\t}\n", acc, body)
		} else {
			g.out.WriteString(body)
		}
		return nil
	case KindNamed:
		q := a.Type.Qualified()
		iface, _ := g.isIfaceType(a.Type)
		if iface {
			if opt {
				// Optional object: absent (nil) serializes nothing.
				fmt.Fprintf(g.out, "\tif %s != nil {\n\t\tif err := %s.Encode(w); err != nil {\n\t\t\treturn err\n\t\t}\n\t}\n", acc, acc)
				return nil
			}
			fmt.Fprintf(g.out, "\tif %s == nil {\n\t\t%s\n\t}\n", acc, requiredErr)
			fmt.Fprintf(g.out, "\tif err := %s.Encode(w); err != nil {\n\t\treturn err\n\t}\n", acc)
			return nil
		}
		if opt {
			// Optional scalars: presence is the flag bit; the body write is
			// skipped entirely when absent.
			var inner string
			switch q {
			case "bytes":
				inner = fmt.Sprintf("\tw.PutBytes(%s)\n", acc)
			case "int128", "int256":
				inner = fmt.Sprintf("\tw.PutRaw(%s[:])\n", acc)
			default:
				inner = fmt.Sprintf("\tw.%s(*%s)\n", putterName(a.Type), acc)
			}
			fmt.Fprintf(g.out, "\tif %s != nil {\n%s\t}\n", acc, inner)
			return nil
		}
		switch q {
		case "bytes":
			fmt.Fprintf(g.out, "\tw.PutBytes(%s)\n", acc)
		case "int128", "int256":
			fmt.Fprintf(g.out, "\tw.PutRaw(%s[:])\n", acc)
		default:
			fmt.Fprintf(g.out, "\tw.%s(%s)\n", putterName(a.Type), acc)
		}
		return nil
	}
	return fmt.Errorf("%s: unsupported arg kind for %q", d, a.GoName)
}

func nilElemErr(d *Def, a *Arg) string {
	return fmt.Sprintf("return fmt.Errorf(\"tl: %s: nil element in vector field %s\")", d.Qualified(), a.GoName)
}

func (g *codegen) primitivePut(t TypeRef) (string, error) {
	switch t.Qualified() {
	case "int":
		return "w.PutInt32(%s)", nil
	case "long":
		return "w.PutInt64(%s)", nil
	case "double":
		return "w.PutDouble(%s)", nil
	case "float":
		return "w.PutFloat(%s)", nil
	case "string":
		return "w.PutString(%s)", nil
	case "bytes":
		return "w.PutBytes(%s)", nil
	case "Bool":
		return "w.PutBoolBoxed(%s)", nil
	case "int128", "int256":
		return "w.PutRaw(%s[:])", nil
	}
	return "", fmt.Errorf("unsupported primitive %q", t.Qualified())
}

func putterName(t TypeRef) string {
	switch t.Qualified() {
	case "int":
		return "PutInt32"
	case "long":
		return "PutInt64"
	case "double":
		return "PutDouble"
	case "float":
		return "PutFloat"
	case "string":
		return "PutString"
	case "Bool", "true":
		return "PutBoolBoxed"
	}
	return "PutRaw"
}

func (g *codegen) isIfaceType(t TypeRef) (bool, error) {
	return t.Kind == KindNamed && !t.Primitive() && t.Qualified() != "Bool" && t.Qualified() != "true", nil
}
