// Package vectors loads the Python-generated compatibility fixtures.
// The JSON files under tests/vectors are produced by
// tools/vectors/generate.py from the immutable SPlusthon reference tree and
// are versioned with the project.
package vectors

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Meta describes the provenance of a vector file.
type Meta struct {
	Generator        string  `json:"generator"`
	Seed             int64   `json:"seed"`
	FixedTime        float64 `json:"fixed_time"`
	Layer            int     `json:"layer"`
	ReferenceVersion string  `json:"reference_version"`
	Python           string  `json:"python"`
}

type doc struct {
	Meta  Meta            `json:"meta"`
	Cases json.RawMessage `json:"cases"`
}

// File is a parsed vector file; callers decode Cases into typed case lists.
// Raw keeps the entire document for files with non-standard layouts
// (constructors.json uses generated/core keys instead of cases).
type File struct {
	Name  string
	Meta  Meta
	Cases json.RawMessage
	Raw   []byte
}

// Load reads a vector file from the fixture directory, resolved relative to
// this source file so it works from any test package's working directory.
func Load(name string) (*File, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return nil, fmt.Errorf("vectors: cannot resolve fixture directory")
	}
	path := filepath.Join(filepath.Dir(thisFile), name)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("vectors: %w", err)
	}
	var d doc
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("vectors: %s: %w", name, err)
	}
	return &File{Name: name, Meta: d.Meta, Cases: d.Cases, Raw: b}, nil
}

// Hex decodes a hex field of a vector case.
func Hex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(fmt.Sprintf("vectors: bad hex: %v", err))
	}
	return b
}
