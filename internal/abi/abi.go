// Package abi embeds the Rust side of the FerroGo ABI (ferrogo.rs) and
// injects it into library crates. See docs/abi.md.
package abi

import (
	_ "embed"
	"strings"
)

// Version is the ABI version implemented by the helper (ferrogo_abi_version).
const Version = 1

// ModuleName is the crate-root module name the helper is injected as. User
// code refers to it as ferrogo::export!.
const ModuleName = "ferrogo"

// FileName is the helper's file name inside the generated crate's src/.
const FileName = ModuleName + ".rs"

// Source is the Rust helper source.
//
//go:embed ferrogo.rs
var Source []byte

// WorkerSource is the native-process worker program (see worker.rs).
//
//go:embed worker.rs
var WorkerSource []byte

// Inject appends the helper's module declaration to a crate root. Appending
// (rather than prepending) keeps every user line number unchanged, so
// diagnostics need no re-mapping.
func Inject(root []byte) []byte {
	var b strings.Builder
	b.Grow(len(root) + 64)
	b.Write(root)
	if len(root) > 0 && root[len(root)-1] != '\n' {
		b.WriteByte('\n')
	}
	b.WriteString("\n// Injected by FerroGo: the ABI helper behind ferrogo::export!.\nmod " + ModuleName + ";\n")
	return []byte(b.String())
}
