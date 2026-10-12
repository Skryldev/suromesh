package tlgen

import "fmt"

// genDecodeBody emits the field deserialization of a definition, in schema
// order. Flag words are read into locals at their schema position and tested
// with bitmask checks; optional fields are decoded only when their bit is
// set. Unknown constructors inside object vectors leave the reader at the
// element start and produce a partial list, mirroring the reference loop.
func (g *codegen) genDecodeBody(d *Def) error {
	words := g.flagWords(d)
	referenced := func(w *Arg) bool {
		for _, a := range d.Args {
			if a.FlagWord == w.Name {
				return true
			}
		}
		return false
	}
	// One function-scoped err: used by word reads (always) or field reads.
	if len(words) > 0 || g.usesOuterErr(d) {
		g.out.WriteString("\tvar err error\n")
	}
	for wi, w := range words {
		if referenced(w) {
			fmt.Fprintf(g.out, "\tvar f%d uint32\n\tif f%d, err = r.ReadUint32(); err != nil {\n\t\treturn nil, err\n\t}\n", wi, wi)
		} else {
			// Unreferenced flag word: consume its 4 bytes without binding.
			fmt.Fprintf(g.out, "\tif _, err = r.ReadUint32(); err != nil {\n\t\treturn nil, err\n\t}\n")
		}
	}
	g.out.WriteString("\tx := &" + d.GoName + "{}\n")
	for _, a := range nonFlagArgs(d) {
		if err := g.genDecodeField(d, a, words); err != nil {
			return err
		}
	}
	g.out.WriteString("\treturn x, nil\n")
	return nil
}

// usesOuterErr reports whether any field decode assigns the outer err.
func (g *codegen) usesOuterErr(d *Def) bool {
	for _, a := range nonFlagArgs(d) {
		if a.FlagWord != "" {
			continue // optional decodes shadow err in their own scope
		}
		switch a.Type.Kind {
		case KindNamed:
			switch a.Type.Qualified() {
			// int128/int256 use a shadowing `b, err :=` block of their own.
			case "int", "long", "double", "float", "string", "bytes", "Bool":
				return true
			}
		case KindFlagTrue, KindBoxedGeneric, KindVector:
			// these shadow err in their own scope or need none
		}
	}
	return false
}

func (g *codegen) wordIndex(words []*Arg, name string) int {
	for i, w := range words {
		if w.Name == name {
			return i
		}
	}
	return -1
}

func (g *codegen) genDecodeField(d *Def, a *Arg, words []*Arg) error {
	acc := fmt.Sprintf("x.%s", a.GoName)
	bitTest := ""
	if a.FlagWord != "" {
		bitTest = fmt.Sprintf("if f%d&(1<<%d) != 0 {\n%s\n\t}", g.wordIndex(words, a.FlagWord), a.FlagBit, "%s")
	}
	emit := func(body string) {
		// Every field decodes inside its own block so that local names
		// (obj, v, n, err) never collide between fields.
		if bitTest == "" {
			fmt.Fprintf(g.out, "\t{\n%s\t}\n", body)
			return
		}
		fmt.Fprintf(g.out, "\t"+bitTest+"\n", body)
	}

	switch a.Type.Kind {
	case KindFlagTrue:
		if a.FlagWord == "" {
			return fmt.Errorf("%s: non-optional true field", d)
		}
		emit(fmt.Sprintf("\t\t%s = f%d&(1<<%d) != 0\n", acc, g.wordIndex(words, a.FlagWord), a.FlagBit))
		return nil
	case KindBoxedGeneric:
		emit(fmt.Sprintf("\t\tobj, err := readObject(r)\n\t\tif err != nil {\n\t\t\treturn nil, err\n\t\t}\n\t\tv, ok := obj.(tl.Request)\n\t\tif !ok {\n\t\t\treturn nil, fmt.Errorf(\"tl: %s: field %%s: expected tl.Request, got %%T\", %q, obj)\n\t\t}\n\t\t%s = v\n", d.Qualified(), a.GoName, acc))
		return nil
	case KindVector:
		elem := a.Type.Elem
		if a.Type.Bare {
			emit(fmt.Sprintf("\t\tn, err := r.ReadVectorBareHeader()\n\t\tif err != nil {\n\t\t\treturn nil, err\n\t\t}\n%s", g.decodeVectorLoop(d, a, elem, true)))
			return nil
		}
		emit(fmt.Sprintf("\t\tn, err := r.ReadVectorBoxedHeader()\n\t\tif err != nil {\n\t\t\treturn nil, err\n\t\t}\n%s", g.decodeVectorLoop(d, a, elem, false)))
		return nil
	case KindNamed:
		q := a.Type.Qualified()
		iface, _ := g.isIfaceType(a.Type)
		opt := a.FlagWord != ""
		switch {
		case q == "int128" || q == "int256":
			n := 16
			if q == "int256" {
				n = 32
			}
			if opt {
				emit(fmt.Sprintf("\t\tb, err := r.ReadRaw(%d)\n\t\tif err != nil {\n\t\t\treturn nil, err\n\t\t}\n\t\tv := [%d]byte{}\n\t\tcopy(v[:], b)\n\t\t%s = &v\n", n, n, acc))
			} else {
				emit(fmt.Sprintf("\t\tb, err := r.ReadRaw(%d)\n\t\tif err != nil {\n\t\t\treturn nil, err\n\t\t}\n\t\tcopy(%s[:], b)\n", n, acc))
			}
			return nil
		case q == "bytes":
			if opt {
				emit(fmt.Sprintf("\t\tif %s, err = r.ReadBytes(); err != nil {\n\t\t\treturn nil, err\n\t\t}\n", acc))
			} else {
				emit(fmt.Sprintf("\t\tif %s, err = r.ReadBytes(); err != nil {\n\t\t\treturn nil, err\n\t\t}\n", acc))
			}
			return nil
		case iface:
			ifaceName := g.goBaseTypeName(a.Type)
			body := fmt.Sprintf("\t\tobj, err := readObject(r)\n\t\tif err != nil {\n\t\t\treturn nil, err\n\t\t}\n\t\tv, ok := obj.(%s)\n\t\tif !ok {\n\t\t\treturn nil, fmt.Errorf(\"tl: %s: field %%s: expected %s, got %%T\", %q, obj)\n\t\t}\n\t\t%s = v\n",
				ifaceName, d.Qualified(), ifaceName, a.GoName, acc)
			emit(body)
			return nil
		default:
			if opt {
				emit(fmt.Sprintf("\t\tv, err := %s\n\t\tif err != nil {\n\t\t\treturn nil, err\n\t\t}\n\t\t%s = &v\n", readerExpr(a.Type), acc))
			} else {
				emit(fmt.Sprintf("\t\tif %s, err = %s; err != nil {\n\t\t\treturn nil, err\n\t\t}\n", acc, readerExpr(a.Type)))
			}
			return nil
		}
	}
	return fmt.Errorf("%s: unsupported decode arg kind for %q", d, a.GoName)
}

// decodeVectorLoop emits the element loop body (header already read into n).
func (g *codegen) decodeVectorLoop(d *Def, a *Arg, elem *TypeRef, bare bool) string {
	acc := fmt.Sprintf("x.%s", a.GoName)
	assertErr := func(iface string) string {
		// Emitted code gets one %T verb; the outer Sprintf needs it escaped.
		return fmt.Sprintf("return nil, fmt.Errorf(\"tl: %s: field %s: expected %s, got %%T\", obj)",
			d.Qualified(), a.GoName, iface)
	}
	switch {
	case elem.Kind == KindNamed && !elem.Primitive() && elem.Bare:
		cn, err := g.goConcreteType(*elem)
		if err != nil {
			panic(err)
		}
		return fmt.Sprintf("\t\ts := make([]%s, 0, min(n, 1024))\n\t\tfor i := 0; i < n; i++ {\n\t\t\tv, err := decode%sBody(r)\n\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n\t\t\ts = append(s, *v)\n\t\t}\n\t\t%s = s\n", cn, cn, acc)
	case elem.Kind == KindNamed && !elem.Primitive():
		iface := g.goBaseTypeName(*elem)
		return fmt.Sprintf("\t\ts := make([]%s, 0, min(n, 1024))\n\t\tfor i := 0; i < n; i++ {\n\t\t\tobj, err := readObject(r)\n\t\t\tif err != nil {\n\t\t\t\tif tl.IsUnknownConstructor(err) {\n\t\t\t\t\tbreak // mirror reference: partial vector, position at element\n\t\t\t\t}\n\t\t\t\treturn nil, err\n\t\t\t}\n\t\t\tv, ok := obj.(%s)\n\t\t\tif !ok {\n\t\t\t\t%s\n\t\t\t}\n\t\t\ts = append(s, v)\n\t\t}\n\t\t%s = s\n", iface, iface, assertErr(iface), acc)
	case elem.Kind == KindNamed && elem.Primitive():
		return fmt.Sprintf("\t\ts := make([]%s, 0, min(n, 1024))\n\t\tfor i := 0; i < n; i++ {\n\t\t\tv, err := %s\n\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n\t\t\ts = append(s, v)\n\t\t}\n\t\t%s = s\n", g.goBaseTypeName(*elem), primitiveReaderExpr(*elem), acc)
	}
	panic(fmt.Sprintf("unsupported vector element %s in %s", elem.Qualified(), d.Qualified()))
}

// goBaseTypeName returns the Go type name of a scalar or interface reference.
func (g *codegen) goBaseTypeName(t TypeRef) string {
	s, err := g.goBaseType(t)
	if err != nil {
		panic(err)
	}
	return s
}

// readerExpr returns an r.Read* expression for scalar/boxed-bool reads.
func readerExpr(t TypeRef) string {
	switch t.Qualified() {
	case "int":
		return "r.ReadInt32()"
	case "long":
		return "r.ReadInt64()"
	case "double":
		return "r.ReadDouble()"
	case "float":
		return "r.ReadFloat()"
	case "string":
		return "r.ReadString()"
	case "Bool":
		return "r.ReadBoolBoxed()"
	}
	panic("readerExpr: unsupported " + t.Qualified())
}

func primitiveReaderExpr(t TypeRef) string {
	switch t.Qualified() {
	case "int":
		return "r.ReadInt32()"
	case "long":
		return "r.ReadInt64()"
	case "double":
		return "r.ReadDouble()"
	case "float":
		return "r.ReadFloat()"
	case "string":
		return "r.ReadString()"
	case "bytes":
		return "r.ReadBytes()"
	case "Bool":
		return "r.ReadBoolBoxed()"
	}
	panic("primitiveReaderExpr: unsupported " + t.Qualified())
}
