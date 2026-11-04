// Command tlgen generates the Go TL protocol code from the canonical
// SPlusthon schema files (api.tl + mtproto.tl).
//
// Determinism: the same schema bytes always produce identical output; run
// with -verify to assert that the checked-in generated code is current.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/Skryldev/suromesh/internal/tlgen"
)

func main() {
	apiPath := flag.String("api", "tl/schema/api.tl", "path to api.tl")
	mtprotoPath := flag.String("mtproto", "tl/schema/mtproto.tl", "path to mtproto.tl")
	outDir := flag.String("out", "tl/gen", "output directory")
	pkg := flag.String("pkg", "gen", "generated package name")
	verify := flag.Bool("verify", false, "verify existing output instead of writing")
	flag.Parse()

	if err := run(*apiPath, *mtprotoPath, *outDir, *pkg, *verify); err != nil {
		fmt.Fprintln(os.Stderr, "tlgen:", err)
		os.Exit(1)
	}
}

func run(apiPath, mtprotoPath, outDir, pkg string, verify bool) error {
	apiSrc, err := os.ReadFile(apiPath)
	if err != nil {
		return fmt.Errorf("read api schema: %w", err)
	}
	mtSrc, err := os.ReadFile(mtprotoPath)
	if err != nil {
		return fmt.Errorf("read mtproto schema: %w", err)
	}

	api, err := tlgen.Parse(filepath.Base(apiPath), apiSrc)
	if err != nil {
		return err
	}
	mtproto, err := tlgen.Parse(filepath.Base(mtprotoPath), mtSrc)
	if err != nil {
		return err
	}

	schema, err := tlgen.Validate(api, mtproto)
	if err != nil {
		return fmt.Errorf("schema validation failed: %w", err)
	}

	apiSum := sha256.Sum256(apiSrc)
	mtSum := sha256.Sum256(mtSrc)
	opt := &tlgen.Options{
		Package:   pkg,
		SchemaSHA: [2]string{hex.EncodeToString(apiSum[:8]), hex.EncodeToString(mtSum[:8])},
		Layer:     schema.Layer,
	}
	files, err := tlgen.Generate(schema, opt)
	if err != nil {
		return err
	}

	names := make([]string, 0, len(files))
	total := 0
	for n := range files {
		names = append(names, n)
		total += len(files[n])
	}
	sort.Strings(names)

	if verify {
		for _, n := range names {
			want := files[n]
			have, err := os.ReadFile(filepath.Join(outDir, n))
			if err != nil {
				return fmt.Errorf("verify %s: %w", n, err)
			}
			if string(want) != string(have) {
				return fmt.Errorf("verify: %s is stale; rerun tlgen to regenerate", n)
			}
		}
		fmt.Printf("verified %d files (%d constructors, LAYER %d), output is current\n",
			len(files), len(schema.Types)+len(schema.Functions), schema.Layer)
		return nil
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(outDir, n), files[n], 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("generated %d files, %d bytes, %d constructors (%d types, %d functions), LAYER %d\n",
		len(files), total, len(schema.Types)+len(schema.Functions), len(schema.Types), len(schema.Functions), schema.Layer)

	if schema.Layer != 182 {
		return errors.New("schema layer is not 182: the canonical Soroush Plus schema must declare LAYER 182")
	}
	return nil
}
