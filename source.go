package ferrogo

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/alirezahematidev/ferrogo/internal/compiler"
)

// maxSourceSize bounds a single source file.
const maxSourceSize = 16 << 20

// Source is Rust code to compile: Code, File, Fragments or Project.
type Source interface {
	sources(kind ArtifactKind) ([]compiler.SourceFile, error)
}

// Code is Rust source text. For programs it must define fn main; for
// libraries it is the crate root. Diagnostics refer to it as main.rs or lib.rs.
//
//	rt.Run(ctx, ferrogo.Code(`fn main() { println!("hi"); }`))
type Code string

func (c Code) sources(kind ArtifactKind) ([]compiler.SourceFile, error) {
	if len(c) > maxSourceSize {
		return nil, fmt.Errorf("ferrogo: source is %d bytes; the limit is %d", len(c), maxSourceSize)
	}
	root := rootFile(kind)
	return []compiler.SourceFile{{Path: root, Content: []byte(c), DisplayName: root}}, nil
}

// File is the path of a single-file Rust program or library. Diagnostics
// refer to it by this path. Sibling modules (`mod foo;`) are not included;
// use a Cargo project for multi-file crates.
type File string

func (f File) sources(kind ArtifactKind) ([]compiler.SourceFile, error) {
	fh, err := os.Open(string(f))
	if err != nil {
		return nil, fmt.Errorf("ferrogo: %w", err)
	}
	defer fh.Close()
	data, err := io.ReadAll(io.LimitReader(fh, maxSourceSize+1))
	if err != nil {
		return nil, fmt.Errorf("ferrogo: reading %s: %w", f, err)
	}
	if len(data) > maxSourceSize {
		return nil, fmt.Errorf("ferrogo: %s exceeds the %d byte source limit", f, maxSourceSize)
	}
	return []compiler.SourceFile{{Path: rootFile(kind), Content: data, DisplayName: filepath.ToSlash(string(f))}}, nil
}

func rootFile(kind ArtifactKind) string {
	if kind == ArtifactLibrary {
		return "lib.rs"
	}
	return "main.rs"
}

// Fragment is a piece of Rust source taken from a larger file, such as a
// /*ferrogo ... */ block in a Go file.
type Fragment struct {
	// File is the name used in diagnostics and panic locations.
	File string
	// Line and Column locate the first character of Text in File (1-based).
	Line, Column int
	// Text is the Rust source.
	Text string
}

// Fragments is Rust source assembled from fragments of other files. The
// fragments are concatenated in order into one crate root, and compiler
// diagnostics and panics refer to the original files, lines and columns.
type Fragments []Fragment

func (fs Fragments) sources(kind ArtifactKind) ([]compiler.SourceFile, error) {
	var text strings.Builder
	var lines []compiler.Origin
	for i, f := range fs {
		if f.File == "" || f.Line < 1 || f.Column < 1 {
			return nil, fmt.Errorf("ferrogo: fragment %d: File, Line and Column are required", i)
		}
		for j, line := range strings.Split(f.Text, "\n") {
			o := compiler.Origin{File: f.File, Line: f.Line + j}
			if j == 0 {
				o.ColumnOffset = f.Column - 1
			}
			lines = append(lines, o)
			text.WriteString(line)
			text.WriteByte('\n')
		}
		if text.Len() > maxSourceSize {
			return nil, fmt.Errorf("ferrogo: fragments exceed the %d byte source limit", maxSourceSize)
		}
	}
	if len(fs) == 0 {
		return nil, errors.New("ferrogo: no fragments")
	}
	root := rootFile(kind)
	return []compiler.SourceFile{{Path: root, Content: []byte(text.String()), DisplayName: root, LineMap: lines}}, nil
}

// Project is the directory of an existing Cargo project (containing
// Cargo.toml). Cargo builds it in place, with its target directory in the
// FerroGo cache, so incremental compilation and the project's own
// Cargo.lock, profiles and dependencies apply. Programs need exactly one
// binary target (or default-run); libraries need crate-type = ["cdylib"].
//
// Building a project runs its build script, so Project sources require
// WithTrust(Trusted); its dependencies must be allowed by the
// DependencyPolicy.
type Project string

func (p Project) sources(ArtifactKind) ([]compiler.SourceFile, error) {
	return nil, errors.New("ferrogo: internal error: projects are built in place")
}
