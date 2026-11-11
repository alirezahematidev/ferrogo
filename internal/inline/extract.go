// Package inline implements `ferrogo generate`: it extracts Rust code
// embedded in a Go package (/*ferrogo ... */ comment blocks and *.ferrogo.rs
// files), and generates typed Go wrappers for the compiled WebAssembly.
package inline

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Marker opens an inline Rust block in a Go file: /*ferrogo ... */.
const Marker = "/*ferrogo"

// Output file names written next to the package's Go files.
const (
	GoFile   = "ferrogo_generated.go"
	WASMFile = "ferrogo_generated.wasm"
)

// generatorVersion changes whenever the generated output format changes.
const generatorVersion = "ferrogo-generate-2"

// RustFileSuffix marks standalone Rust files included in the package.
const RustFileSuffix = ".ferrogo.rs"

// Block is one piece of Rust source with its location.
type Block struct {
	File         string // file name relative to the package directory
	Line, Column int    // 1-based position of the first character of Text
	Text         string
}

// Package is the Rust code found in a Go package directory.
type Package struct {
	Dir  string
	Name string // Go package name
	// Blocks are ordered by file name, then position.
	Blocks []Block
	// Decls are the package's top-level Go identifiers, excluding the
	// generated file, used to detect name collisions.
	Decls map[string]bool
}

// Extract scans dir for inline Rust. Every non-test .go file is scanned,
// regardless of build constraints: the WebAssembly artifact is platform
// independent.
func Extract(dir string) (*Package, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	pkg := &Package{Dir: dir, Decls: map[string]bool{}}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		path := filepath.Join(dir, name)
		switch {
		case strings.HasSuffix(name, RustFileSuffix):
			src, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			pkg.Blocks = append(pkg.Blocks, Block{File: name, Line: 1, Column: 1, Text: string(src)})
		case strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") && name != GoFile:
			if err := pkg.scanGoFile(fset, path, name); err != nil {
				return nil, err
			}
		}
	}
	sort.SliceStable(pkg.Blocks, func(i, j int) bool {
		a, b := pkg.Blocks[i], pkg.Blocks[j]
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
	return pkg, nil
}

func (pkg *Package) scanGoFile(fset *token.FileSet, path, name string) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return err
	}
	if pkg.Name == "" {
		pkg.Name = f.Name.Name
	} else if pkg.Name != f.Name.Name {
		return fmt.Errorf("%s: package %s, but other files are in package %s", name, f.Name.Name, pkg.Name)
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil {
				pkg.Decls[d.Name.Name] = true
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					pkg.Decls[s.Name.Name] = true
				case *ast.ValueSpec:
					for _, n := range s.Names {
						pkg.Decls[n.Name] = true
					}
				}
			}
		}
	}
	for _, group := range f.Comments {
		for _, c := range group.List {
			text, ok := blockText(c.Text)
			if !ok {
				continue
			}
			pos := fset.Position(c.Pos())
			pkg.Blocks = append(pkg.Blocks, Block{
				File: name, Line: pos.Line, Column: pos.Column + len(Marker), Text: text,
			})
		}
	}
	return nil
}

// blockText returns the Rust text of a /*ferrogo ... */ comment.
func blockText(comment string) (string, bool) {
	rest, ok := strings.CutPrefix(comment, Marker)
	if !ok || !strings.HasSuffix(rest, "*/") {
		return "", false
	}
	rest = strings.TrimSuffix(rest, "*/")
	// Require whitespace after the marker so /*ferrogoX is not a block.
	if rest != "" && !strings.ContainsRune(" \t\r\n", rune(rest[0])) {
		return "", false
	}
	return rest, true
}

// SourceHash fingerprints the extracted Rust code (names, positions and
// text). It is recorded in the generated file so that `ferrogo generate
// -check` can detect stale output without a Rust toolchain.
func (pkg *Package) SourceHash() string {
	h := sha256.New()
	// The generator version makes -check report files written by an older
	// generator (different output format) as stale.
	fmt.Fprintf(h, "%s\x00", generatorVersion)
	for _, b := range pkg.Blocks {
		fmt.Fprintf(h, "%s\x00%d\x00%d\x00%d\x00", b.File, b.Line, b.Column, len(b.Text))
		h.Write([]byte(b.Text))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// RecordedHash returns the source hash recorded in an existing generated
// file, or "" if there is none.
func RecordedHash(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, GoFile))
	if err != nil {
		return ""
	}
	const prefix = "// Source hash: "
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		if s, ok := bytes.CutPrefix(line, []byte(prefix)); ok {
			return strings.TrimSpace(string(s))
		}
	}
	return ""
}
