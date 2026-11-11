package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/alirezahematidev/ferrogo"
	"github.com/alirezahematidev/ferrogo/internal/inline"
)

const generateUsage = `generate [flags] [dir]

Compiles the Rust code embedded in a Go package and generates typed Go
wrappers. Rust is embedded as /*ferrogo ... */ block comments in .go files or
as *.ferrogo.rs files; top-level ` + "`pub fn`" + ` items are exported to Go.

Writes ` + inline.GoFile + ` and ` + inline.WASMFile + ` (embedded with go:embed), so
programs using the package need no Rust toolchain. Typical use:

	//go:generate go run github.com/alirezahematidev/ferrogo/cmd/ferrogo generate`

func cmdGenerate(ctx context.Context, out *output, _ io.Reader, args []string) error {
	c := newCommon("generate", generateUsage, out.stderr)
	check := c.fs.Bool("check", false, "only verify that the generated files match the Rust sources (no toolchain needed)")
	prefix := c.fs.String("prefix", "", "prefix for the generated package-level functions")
	typeName := c.fs.String("type", "Rust", "name of the generated module type")
	positional, _, err := c.parse(args)
	if err != nil {
		return err
	}
	if len(positional) > 1 {
		c.fs.Usage()
		return errUsage
	}
	dir := "."
	if len(positional) == 1 {
		dir = positional[0]
	}
	if c.backend != "" && c.backend != "wasm" && c.backend != "auto" {
		return fmt.Errorf("generate embeds WebAssembly; --backend %s is not supported", c.backend)
	}

	pkg, err := inline.Extract(dir)
	if err != nil {
		return err
	}
	if len(pkg.Blocks) == 0 {
		return fmt.Errorf("%s: no /*ferrogo ... */ blocks or *%s files found", dir, inline.RustFileSuffix)
	}
	hash := pkg.SourceHash()
	if *check {
		_, wasmErr := os.Stat(filepath.Join(dir, inline.WASMFile))
		if inline.RecordedHash(dir) != hash || wasmErr != nil {
			out.errorf("%s is out of date with the Rust sources; run `ferrogo generate`", filepath.Join(dir, inline.GoFile))
			return exitError{code: 1}
		}
		if !c.json {
			out.status("Up to date", "%s", filepath.Join(dir, inline.GoFile))
		}
		return nil
	}

	fragments := make(ferrogo.Fragments, len(pkg.Blocks))
	docs := map[string]string{}
	files := map[string]bool{}
	var sources []string
	for i, b := range pkg.Blocks {
		fragments[i] = ferrogo.Fragment{
			File: filepath.ToSlash(filepath.Join(dir, b.File)), Line: b.Line, Column: b.Column,
			Text: inline.AutoExport(b.Text),
		}
		for name, d := range inline.DocComments(b.Text) {
			docs[name] = d
		}
		if !files[b.File] {
			files[b.File] = true
			sources = append(sources, b.File)
		}
	}

	c.backend = "wasm"
	rt, err := c.runtime(out)
	if err != nil {
		return err
	}
	defer rt.Close()
	if !c.json {
		out.status("Compiling", "Rust in %s (%d block(s))", dir, len(pkg.Blocks))
	}
	art, err := rt.Build(ctx, fragments, ferrogo.ArtifactLibrary)
	if err != nil {
		var ce *ferrogo.CompileError
		if errors.As(err, &ce) && len(ce.Errors()) > 0 {
			out.goStyleDiagnostics(ce.Diagnostics)
			return exitError{code: 1}
		}
		return err
	}
	if !c.json {
		out.goStyleDiagnostics(art.Warnings)
	}

	mod, err := rt.LoadModule(ctx, art.Path)
	if err != nil {
		return err
	}
	defer mod.Close()
	fns, err := mod.Functions(ctx)
	if err != nil {
		return err
	}
	if len(fns) == 0 {
		return errors.New("the Rust code exports no functions; mark functions `pub fn` at the top level of a ferrogo block")
	}
	sigs := make([]inline.Signature, len(fns))
	for i, f := range fns {
		if sigs[i], err = inline.ParseSignature(f.Signature); err != nil {
			return err
		}
	}
	tc, err := rt.Toolchain(ctx)
	if err != nil {
		return err
	}
	code, err := inline.Generate(inline.Config{
		Package: pkg.Name, TypeName: *typeName, FuncPrefix: *prefix, Existing: pkg.Decls, Docs: docs,
		SourceHash: hash, Sources: strings.Join(sources, ", "),
		Toolchain: fmt.Sprintf("rustc %s (%s), target %s", tc.Version, shortHash(tc.CommitHash), art.Target),
	}, sigs)
	if err != nil {
		return err
	}
	wasm, err := os.ReadFile(art.Path)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dir, inline.WASMFile), wasm); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dir, inline.GoFile), code); err != nil {
		return err
	}
	if c.json {
		return out.json(map[string]any{"go_file": filepath.Join(dir, inline.GoFile), "wasm_file": filepath.Join(dir, inline.WASMFile), "functions": fns})
	}
	out.status("Generated", "%s (%d function(s)), %s (%s)", filepath.Join(dir, inline.GoFile), len(fns),
		filepath.Join(dir, inline.WASMFile), humanBytes(int64(len(wasm))))
	return nil
}

func shortHash(h string) string {
	if len(h) > 9 {
		return h[:9]
	}
	return h
}

// writeFileAtomic replaces path so readers never see a partial file.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o644)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
