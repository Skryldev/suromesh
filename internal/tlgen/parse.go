package tlgen

import (
	"bufio"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Parse parses one TL schema file into raw definitions. Validation happens
// separately in Validate so both files can be merged first.
func Parse(filename string, src []byte) (*Schema, error) {
	sc := &Schema{}
	section := "types" // mtproto.tl defines core types before any section marker
	s := bufio.NewScanner(strings.NewReader(string(src)))
	s.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	lineNo := 0
	for s.Scan() {
		lineNo++
		line := s.Text()
		if i := strings.Index(line, "//"); i >= 0 {
			// A LAYER header may appear inside the comment: "// LAYER 182".
			if h := strings.TrimSpace(line[i+2:]); strings.HasPrefix(h, "LAYER ") {
				if sc.Layer != 0 {
					return nil, fmt.Errorf("%s:%d: duplicate LAYER header", filename, lineNo)
				}
				n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(h, "LAYER ")))
				if err != nil {
					return nil, fmt.Errorf("%s:%d: malformed LAYER header %q", filename, lineNo, h)
				}
				sc.Layer = n
			}
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case line == "---types---":
			section = "types"
			continue
		case line == "---functions---":
			section = "functions"
			continue
		}
		def, structural, err := parseDef(section, line, lineNo)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", filename, lineNo, err)
		}
		if structural != "" {
			sc.Structural = append(sc.Structural, structural)
			continue
		}
		if def == nil {
			continue
		}
		if def.Kind == KindFunction {
			sc.Functions = append(sc.Functions, def)
		} else {
			sc.Types = append(sc.Types, def)
		}
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("%s: scanning: %w", filename, err)
	}
	return sc, nil
}

// defRe matches "name#id args... = Result;" — the args and result are then
// split further. Names may be namespace-qualified ("messages.sendMessage").
var defRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_.]*)#([0-9a-fA-F]+)\s*(.*)$`)

// structuralRe matches id-less definitions such as the tlsClientHello /
// tlsBlock* family, which exist for MTProxy TLS transport framing and are
// never boxed TL constructors. They are parsed and skipped deterministically;
// Validate fails loudly if any generated definition references their result.
var structuralRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_.]*)\s+(.*)$`)

// parseDef parses a single non-empty, non-comment line. The third return
// value reports an id-less structural definition that was skipped.
func parseDef(section, line string, lineNo int) (def *Def, structural string, err error) {
	m := defRe.FindStringSubmatch(line)
	if m == nil {
		// Id-less structural definition (e.g. tlsClientHello): record its
		// result type so references to it can be rejected loudly.
		if sm := structuralRe.FindStringSubmatch(line); sm != nil {
			if eq := strings.LastIndex(sm[2], "="); eq >= 0 {
				res := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sm[2][eq+1:]), ";"))
				return nil, res, nil
			}
		}
		return nil, "", fmt.Errorf("malformed definition: %q", line)
	}
	fullName, idHex, rest := m[1], m[2], strings.TrimSpace(m[3])

	// The generic vector definition ("vector#1cb5c415 {t:Type} # [ t ] = Vector t;")
	// is a schema-level primitive handled by the runtime; skip it.
	if fullName == "vector" {
		return nil, "", nil
	}

	eq := strings.LastIndex(rest, "=")
	if eq < 0 {
		return nil, "", fmt.Errorf("missing '= Result' in %q", line)
	}
	resultPart := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest[eq+1:]), ";"))
	argsPart := strings.TrimSpace(rest[:eq])

	ns, name := splitQualified(fullName)
	id, err := strconv.ParseUint(idHex, 16, 32)
	if err != nil {
		return nil, "", fmt.Errorf("bad constructor id %q: %w", idHex, err)
	}

	kind := KindType
	if section == "functions" {
		kind = KindFunction
	}
	def = &Def{Kind: kind, Namespace: ns, Name: name, ID: uint32(id), Line: lineNo}

	result, err := parseTypeRef(resultPart, true)
	if err != nil {
		return nil, "", fmt.Errorf("bad result %q: %w", resultPart, err)
	}
	def.Result = *result

	if argsPart != "" {
		for _, tok := range strings.Fields(argsPart) {
			if strings.HasPrefix(tok, "{") {
				// Generic parameter declaration "{t:Type}".
				inner := strings.TrimSuffix(strings.TrimPrefix(tok, "{"), "}")
				if !strings.HasSuffix(inner, ":Type") {
					return nil, "", fmt.Errorf("unsupported generic parameter %q", tok)
				}
				continue // recorded implicitly; only vector/wrappers may use it
			}
			arg, err := parseArg(tok)
			if err != nil {
				return nil, "", fmt.Errorf("arg %q: %w", tok, err)
			}
			def.Args = append(def.Args, arg)
		}
	}
	return def, "", nil
}

// splitQualified splits "ns.name" into ("ns", "name"); a single token yields
// ("", name). Only one dot level is legal in these schemas.
func splitQualified(s string) (string, string) {
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[:i], s[i+1:]
	}
	return "", s
}

func parseArg(tok string) (*Arg, error) {
	i := strings.Index(tok, ":")
	if i <= 0 {
		return nil, fmt.Errorf("malformed (missing ':type')")
	}
	name, typePart := tok[:i], tok[i+1:]

	if strings.HasPrefix(typePart, "!") {
		inner := typePart[1:]
		if inner != "X" {
			return nil, fmt.Errorf("unsupported boxed generic %q", typePart)
		}
		return &Arg{Name: name, Type: TypeRef{Kind: KindBoxedGeneric, Name: inner}}, nil
	}

	// Optional: flags.N?type
	if j := strings.Index(typePart, "?"); j >= 0 {
		flagPart, inner := typePart[:j], typePart[j+1:]
		dot := strings.LastIndex(flagPart, ".")
		if dot < 0 || flagPart[:dot] == "" {
			return nil, fmt.Errorf("malformed optional flag %q", typePart)
		}
		bit, err := strconv.Atoi(flagPart[dot+1:])
		if err != nil || bit < 0 || bit > 31 {
			return nil, fmt.Errorf("bad flag bit in %q", typePart)
		}
		t, err := parseTypeRef(inner, false)
		if err != nil {
			return nil, err
		}
		return &Arg{Name: name, FlagWord: flagPart[:dot], FlagBit: bit, Type: *t}, nil
	}

	t, err := parseTypeRef(typePart, false)
	if err != nil {
		return nil, err
	}
	return &Arg{Name: name, FlagBit: -1, Type: *t}, nil
}

var primitives = map[string]bool{
	"int": true, "long": true, "int128": true, "int256": true,
	"double": true, "float": true, "string": true, "bytes": true,
	"#": true, "true": true, "Bool": true,
}

// parseTypeRef parses "ns.Name", "Vector<Elem>", "vector<elem>", "#", "true",
// "X", or a primitive name.
func parseTypeRef(tok string, isResult bool) (*TypeRef, error) {
	switch {
	case tok == "#":
		return &TypeRef{Kind: KindFlagWord, Name: "#"}, nil
	case tok == "true":
		return &TypeRef{Kind: KindFlagTrue, Name: "true"}, nil
	case tok == "X":
		return &TypeRef{Kind: KindGenericRef, Name: "X"}, nil
	case strings.HasPrefix(tok, "Vector<") && strings.HasSuffix(tok, ">"):
		elem, err := parseTypeRef(tok[len("Vector<"):len(tok)-1], false)
		if err != nil {
			return nil, err
		}
		if elem.Kind != KindNamed {
			return nil, fmt.Errorf("unsupported vector element kind in %q", tok)
		}
		return &TypeRef{Kind: KindVector, Elem: elem, Bare: false}, nil
	case strings.HasPrefix(tok, "vector<") && strings.HasSuffix(tok, ">"):
		elem, err := parseTypeRef(tok[len("vector<"):len(tok)-1], false)
		if err != nil {
			return nil, err
		}
		if elem.Kind != KindNamed {
			return nil, fmt.Errorf("unsupported vector element kind in %q", tok)
		}
		return &TypeRef{Kind: KindVector, Elem: elem, Bare: true}, nil
	}
	ns, name := splitQualified(tok)
	if primitives[name] && ns == "" {
		k := KindNamed
		switch name {
		case "#":
			k = KindFlagWord
		case "true":
			k = KindFlagTrue
		}
		return &TypeRef{Kind: k, Name: name}, nil
	}
	// Bare reference: lowercase first letter means the constructor ID is
	// omitted on the wire (only legal inside bare vectors).
	bare := !isResult && len(name) > 0 && name[0] >= 'a' && name[0] <= 'z' && !primitives[name]
	return &TypeRef{Kind: KindNamed, NS: ns, Name: name, Bare: bare}, nil
}
