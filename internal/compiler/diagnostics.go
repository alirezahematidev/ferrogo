package compiler

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/alirezahematidev/ferrogo/internal/errdefs"
)

// Diagnostic is a compiler message with source locations re-mapped to the
// names the user knows (e.g. "main.rs" or the path passed to File).
type Diagnostic struct {
	// Level is rustc's level: "error", "warning", "note", "help", or
	// "error: internal compiler error".
	Level    string       `json:"level"`
	Code     string       `json:"code,omitempty"`
	Message  string       `json:"message"`
	Spans    []Span       `json:"spans,omitempty"`
	Children []Diagnostic `json:"children,omitempty"`
	// Rendered is rustc's human-readable rendering, without ANSI colours.
	Rendered string `json:"rendered,omitempty"`
}

// Span is a source range. Lines and columns are 1-based.
type Span struct {
	File      string `json:"file"`
	Line      int    `json:"line"`
	Column    int    `json:"column"`
	EndLine   int    `json:"end_line"`
	EndColumn int    `json:"end_column"`
	Label     string `json:"label,omitempty"`
	Primary   bool   `json:"primary"`
}

// Location formats the primary span as file:line:column, or "" if none.
func (d Diagnostic) Location() string {
	for _, s := range d.Spans {
		if s.Primary {
			return fmt.Sprintf("%s:%d:%d", s.File, s.Line, s.Column)
		}
	}
	return ""
}

// Reason classifies a build failure.
type Reason string

// Build failure reasons.
const (
	ReasonCompile    Reason = "compile"    // rustc reported errors in the source
	ReasonLink       Reason = "link"       // the linker failed
	ReasonTarget     Reason = "target"     // the target's standard library is missing
	ReasonDependency Reason = "dependency" // dependency resolution or download failed
	ReasonManifest   Reason = "manifest"   // Cargo.toml was rejected
	ReasonTimeout    Reason = "timeout"    // the compile timeout expired
	ReasonInternal   Reason = "internal"   // cargo or rustc failed unexpectedly
)

// Error is a failed build.
type Error struct {
	Reason      Reason
	Target      string
	Diagnostics []Diagnostic
	// Output is cargo's textual stderr, for failures that produce no diagnostics.
	Output string
}

// Errors returns the error-level diagnostics.
func (e *Error) Errors() []Diagnostic {
	var out []Diagnostic
	for _, d := range e.Diagnostics {
		if strings.HasPrefix(d.Level, "error") {
			out = append(out, d)
		}
	}
	return out
}

func (e *Error) Error() string {
	errs := e.Errors()
	var msg string
	switch {
	case len(errs) > 0:
		msg = errs[0].Message
		if loc := errs[0].Location(); loc != "" {
			msg = loc + ": " + msg
		}
		if len(errs) > 1 {
			msg += fmt.Sprintf(" (and %d more error(s))", len(errs)-1)
		}
	case e.Output != "":
		msg = firstCargoError(e.Output)
	default:
		msg = string(e.Reason)
	}
	return fmt.Sprintf("%s (%s): %s", errdefs.ErrCompilation, e.Reason, msg)
}

// Unwrap lets errors.Is match ErrCompilation and, for timeouts and missing
// targets, the more specific sentinel too.
func (e *Error) Unwrap() []error {
	errs := []error{errdefs.ErrCompilation}
	switch e.Reason {
	case ReasonTimeout:
		errs = append(errs, errdefs.ErrTimeout)
	case ReasonTarget:
		errs = append(errs, errdefs.ErrTargetNotInstalled)
	}
	return errs
}

func firstCargoError(out string) string {
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, "error") {
			return strings.TrimSpace(line)
		}
	}
	return strings.TrimSpace(out)
}

// cargoMessage is one line of `cargo build --message-format=json`.
type cargoMessage struct {
	Reason     string           `json:"reason"`
	PackageID  string           `json:"package_id"`
	Message    *rustcDiagnostic `json:"message"`
	Target     *cargoTarget     `json:"target"`
	Filenames  []string         `json:"filenames"`
	Executable *string          `json:"executable"`
	Success    *bool            `json:"success"`
	// ManifestPath and Fresh are set on compiler-artifact messages.
	ManifestPath string `json:"manifest_path"`
	Fresh        bool   `json:"fresh"`
}

type cargoTarget struct {
	Name       string   `json:"name"`
	Kind       []string `json:"kind"`
	CrateTypes []string `json:"crate_types"`
}

type rustcDiagnostic struct {
	Message  string                 `json:"message"`
	Code     *struct{ Code string } `json:"code"`
	Level    string                 `json:"level"`
	Spans    []rustcSpan            `json:"spans"`
	Children []rustcDiagnostic      `json:"children"`
	Rendered *string                `json:"rendered"`
}

type rustcSpan struct {
	FileName    string  `json:"file_name"`
	LineStart   int     `json:"line_start"`
	LineEnd     int     `json:"line_end"`
	ColumnStart int     `json:"column_start"`
	ColumnEnd   int     `json:"column_end"`
	IsPrimary   bool    `json:"is_primary"`
	Label       *string `json:"label"`
}

// buildOutput is the parsed cargo JSON stream.
type buildOutput struct {
	diagnostics []Diagnostic
	artifacts   []cargoMessage
	finished    bool
	success     bool
}

// sourceMapper re-maps generated file names (src/main.rs) to user-visible
// names and applies line maps for sources extracted from larger files. For
// Cargo projects it prefixes project-relative paths with the project
// directory as the user wrote it.
type sourceMapper struct {
	files      map[string]SourceFile // key: "src/<path>"
	projectDir string
}

func newSourceMapper(sources []SourceFile) sourceMapper {
	m := sourceMapper{files: map[string]SourceFile{}}
	for _, s := range sources {
		m.files["src/"+s.Path] = s
	}
	return m
}

func (m sourceMapper) locate(file string, line, col int) (string, int, int, bool) {
	// rustc reports src\main.rs on Windows.
	file = slashPath(file)
	if src, ok := m.files[file]; ok {
		f, l, c := src.Locate(line, col)
		return f, l, c, true
	}
	if m.projectDir != "" && !filepath.IsAbs(file) && !strings.HasPrefix(file, "/rustc/") {
		return filepath.ToSlash(filepath.Join(m.projectDir, file)), line, col, true
	}
	return file, line, col, false
}

func (m sourceMapper) span(s rustcSpan) Span {
	out := Span{
		File: s.FileName, Line: s.LineStart, Column: s.ColumnStart,
		EndLine: s.LineEnd, EndColumn: s.ColumnEnd, Primary: s.IsPrimary,
	}
	if s.Label != nil {
		out.Label = *s.Label
	}
	if f, l, c, ok := m.locate(s.FileName, s.LineStart, s.ColumnStart); ok {
		out.File, out.Line, out.Column = f, l, c
		_, out.EndLine, out.EndColumn, _ = m.locate(s.FileName, s.LineEnd, s.ColumnEnd)
	}
	return out
}

// renderedLocation matches the location headers in rustc's rendering.
var renderedLocation = regexp.MustCompile(`(-->|:::) ([^\s:]+):(\d+):(\d+)`)

func (m sourceMapper) rendered(text string) string {
	return renderedLocation.ReplaceAllStringFunc(text, func(match string) string {
		g := renderedLocation.FindStringSubmatch(match)
		line, _ := strconv.Atoi(g[3]) // digits guaranteed by the regexp
		col, _ := strconv.Atoi(g[4])
		file, line, col, ok := m.locate(g[2], line, col)
		if !ok {
			return match
		}
		return fmt.Sprintf("%s %s:%d:%d", g[1], file, line, col)
	})
}

func (m sourceMapper) diagnostic(d *rustcDiagnostic) Diagnostic {
	out := Diagnostic{Level: d.Level, Message: d.Message}
	if d.Code != nil {
		out.Code = d.Code.Code
	}
	if d.Rendered != nil {
		out.Rendered = m.rendered(*d.Rendered)
	}
	for _, s := range d.Spans {
		out.Spans = append(out.Spans, m.span(s))
	}
	for i := range d.Children {
		out.Children = append(out.Children, m.diagnostic(&d.Children[i]))
	}
	return out
}

// isNoise reports summary messages that carry no information for the user.
func isNoise(d *rustcDiagnostic) bool {
	if d.Level == "failure-note" {
		return true
	}
	if len(d.Spans) == 0 {
		if strings.HasPrefix(d.Message, "aborting due to") {
			return true
		}
		if d.Level == "warning" && strings.Contains(d.Message, "generated ") && strings.Contains(d.Message, " warning") {
			return true
		}
	}
	return false
}

// parseCargoJSON parses cargo's JSON message stream. Lines that are not JSON
// (which cargo should never print on stdout) are ignored.
func parseCargoJSON(stdout []byte, mapper sourceMapper) buildOutput {
	var out buildOutput
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var msg cargoMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		switch msg.Reason {
		case "compiler-message":
			if msg.Message != nil && !isNoise(msg.Message) {
				out.diagnostics = append(out.diagnostics, mapper.diagnostic(msg.Message))
			}
		case "compiler-artifact":
			out.artifacts = append(out.artifacts, msg)
		case "build-finished":
			out.finished = true
			out.success = msg.Success != nil && *msg.Success
		}
	}
	return out
}

// classify determines why a build failed.
func classify(diags []Diagnostic, stderr string) Reason {
	for _, d := range diags {
		if !strings.HasPrefix(d.Level, "error") {
			continue
		}
		if d.Code == "E0463" && (strings.Contains(d.Message, "`std`") || strings.Contains(d.Message, "`core`")) {
			return ReasonTarget
		}
		if strings.HasPrefix(d.Message, "linking with") || strings.Contains(d.Message, "linker `") {
			return ReasonLink
		}
	}
	for _, d := range diags {
		if strings.HasPrefix(d.Level, "error") {
			return ReasonCompile
		}
	}
	switch {
	case containsAny(stderr, "failed to select a version", "no matching package named",
		"failed to load source for dependency", "failed to download", "failed to get `",
		"--offline was specified", "failed to fetch"):
		return ReasonDependency
	case containsAny(stderr, "failed to parse manifest"):
		return ReasonManifest
	}
	return ReasonInternal
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
