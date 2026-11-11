package ferrogo

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/alirezahematidev/ferrogo/internal/compiler"
	"github.com/alirezahematidev/ferrogo/internal/errdefs"
)

// Sentinel errors. Match them with errors.Is.
var (
	// ErrToolchainNotFound: no usable rustc/cargo was found, or rustc is too old.
	ErrToolchainNotFound = errdefs.ErrToolchainNotFound
	// ErrTargetNotInstalled: the target required by a backend is not installed.
	// errors.As with *TargetError gives the remediation command.
	ErrTargetNotInstalled = errdefs.ErrTargetNotInstalled
	// ErrCompilation: Rust compilation failed. errors.As with *CompileError
	// gives the diagnostics.
	ErrCompilation = errdefs.ErrCompilation
	// ErrTimeout: compilation or execution exceeded its time limit.
	ErrTimeout = errdefs.ErrTimeout
	// ErrOutputLimit: the program wrote more output than allowed.
	ErrOutputLimit = errdefs.ErrOutputLimit
	// ErrMemoryLimit: guest code exceeded its memory limit.
	ErrMemoryLimit = errdefs.ErrMemoryLimit
	// ErrUnsupported: the backend cannot perform the operation or enforce a
	// requested limit. errors.As with *UnsupportedError gives details.
	ErrUnsupported = errdefs.ErrUnsupported
	// ErrUntrusted: the trust policy forbids the backend. See WithTrust.
	ErrUntrusted = errdefs.ErrUntrusted
	// ErrABIMismatch: argument or result types do not match the Rust function.
	ErrABIMismatch = errdefs.ErrABIMismatch
	// ErrFunctionNotFound: the module does not export the function.
	ErrFunctionNotFound = errdefs.ErrFunctionNotFound
	// ErrClosed: the Runtime or Module has been closed.
	ErrClosed = errdefs.ErrClosed
	// ErrDependencyPolicy: the dependency policy forbids a dependency.
	ErrDependencyPolicy = errdefs.ErrDependencyPolicy
)

// Diagnostic is a Rust compiler message.
type Diagnostic struct {
	// Severity is "error", "warning", "note", "help", or
	// "error: internal compiler error".
	Severity string `json:"severity"`
	// Code is the rustc error code such as "E0308", if any.
	Code     string       `json:"code,omitempty"`
	Message  string       `json:"message"`
	Spans    []Span       `json:"spans,omitempty"`
	Children []Diagnostic `json:"children,omitempty"`
	// Rendered is rustc's human-readable rendering (no ANSI colours), with
	// file names re-mapped to the names you supplied.
	Rendered string `json:"rendered,omitempty"`
}

// Span is a source range in a diagnostic. Lines and columns are 1-based.
type Span struct {
	File      string `json:"file"`
	Line      int    `json:"line"`
	Column    int    `json:"column"`
	EndLine   int    `json:"end_line"`
	EndColumn int    `json:"end_column"`
	Label     string `json:"label,omitempty"`
	Primary   bool   `json:"primary"`
}

// Location returns the primary span as "file:line:column", or "".
func (d Diagnostic) Location() string {
	for _, s := range d.Spans {
		if s.Primary {
			return fmt.Sprintf("%s:%d:%d", s.File, s.Line, s.Column)
		}
	}
	return ""
}

func convertDiagnostics(in []compiler.Diagnostic) []Diagnostic {
	if len(in) == 0 {
		return nil
	}
	out := make([]Diagnostic, len(in))
	for i, d := range in {
		out[i] = Diagnostic{
			Severity: d.Level, Code: d.Code, Message: d.Message,
			Rendered: d.Rendered, Children: convertDiagnostics(d.Children),
		}
		for _, s := range d.Spans {
			out[i].Spans = append(out[i].Spans, Span(s))
		}
	}
	return out
}

// CompileError reports a failed build.
type CompileError struct {
	// Reason classifies the failure: "compile", "link", "target",
	// "dependency", "manifest", "timeout" or "internal".
	Reason string
	Target string
	// Diagnostics holds every compiler message, errors and warnings.
	Diagnostics []Diagnostic
	// Output is cargo's textual output, useful when Reason is not "compile".
	Output string

	err *compiler.Error
}

func (e *CompileError) Error() string { return e.err.Error() }

// Unwrap matches ErrCompilation, plus ErrTimeout or ErrTargetNotInstalled
// when those caused the failure.
func (e *CompileError) Unwrap() []error { return e.err.Unwrap() }

// Errors returns only the error-level diagnostics.
func (e *CompileError) Errors() []Diagnostic {
	var out []Diagnostic
	for _, d := range e.Diagnostics {
		if strings.HasPrefix(d.Severity, "error") {
			out = append(out, d)
		}
	}
	return out
}

// Rendered returns all diagnostics as rustc would print them, or the raw
// cargo output when there are none.
func (e *CompileError) Rendered() string {
	var b strings.Builder
	for _, d := range e.Diagnostics {
		b.WriteString(d.Rendered)
	}
	if b.Len() == 0 {
		return e.Output
	}
	return b.String()
}

func newCompileError(err *compiler.Error) *CompileError {
	return &CompileError{
		Reason: string(err.Reason), Target: err.Target,
		Diagnostics: convertDiagnostics(err.Diagnostics), Output: err.Output, err: err,
	}
}

// TargetError reports a missing compilation target.
type TargetError struct {
	Target  string
	Backend Backend
	// Fix is a command or instruction that installs the target.
	Fix string
}

func (e *TargetError) Error() string {
	return fmt.Sprintf("ferrogo: target %s (needed by the %s backend) is not installed; fix: %s", e.Target, e.Backend, e.Fix)
}

// Unwrap matches ErrTargetNotInstalled.
func (e *TargetError) Unwrap() error { return ErrTargetNotInstalled }

// UnsupportedError reports an operation or limit a backend cannot provide.
type UnsupportedError struct {
	Backend Backend
	Feature string
	// Hint suggests an alternative, if any.
	Hint string
}

func (e *UnsupportedError) Error() string {
	msg := fmt.Sprintf("ferrogo: the %s backend does not support %s", e.Backend, e.Feature)
	if e.Hint != "" {
		msg += "; " + e.Hint
	}
	return msg
}

// Unwrap matches ErrUnsupported.
func (e *UnsupportedError) Unwrap() error { return ErrUnsupported }

// ExitError reports a program that exited unsuccessfully. Run returns it
// together with the Result, which holds the program's output.
type ExitError struct {
	// Code is the exit status, or -1 if the program was killed by a signal.
	Code int
	// Signal names the terminating signal (Unix), or is empty.
	Signal string
	// Trap describes the WebAssembly trap that ended the program (WASM
	// backend), e.g. "unreachable" after a Rust panic.
	Trap string
	// Panic is set when the program exited because of a Rust panic.
	Panic *PanicError
}

func (e *ExitError) Error() string {
	switch {
	case e.Panic != nil:
		return e.Panic.Error()
	case e.Trap != "":
		return "ferrogo: program trapped: " + e.Trap
	case e.Signal != "":
		return "ferrogo: program terminated by signal: " + e.Signal
	default:
		return "ferrogo: program exited with status " + strconv.Itoa(e.Code)
	}
}

// Unwrap returns the panic, if any, so errors.As(err, **PanicError) works.
func (e *ExitError) Unwrap() error {
	if e.Panic == nil {
		return nil
	}
	return e.Panic
}

// PanicError describes a Rust panic.
type PanicError struct {
	Message string `json:"message"`
	// Thread is the panicking thread's name ("main" for the main thread).
	Thread string `json:"thread"`
	// File, Line and Column locate the panic, re-mapped to your file names.
	File   string `json:"file,omitempty"`
	Line   int    `json:"line,omitempty"`
	Column int    `json:"column,omitempty"`
}

func (e *PanicError) Error() string {
	if e.File == "" {
		return "ferrogo: Rust panic: " + e.Message
	}
	return fmt.Sprintf("ferrogo: Rust panic at %s:%d:%d: %s", e.File, e.Line, e.Column, e.Message)
}

// panicHeader matches Rust's panic message header: Rust 1.73+ format, with
// the thread ID that newer releases print after the name.
var panicHeader = regexp.MustCompile(`(?m)^thread '([^']*)'(?: \(\d+\))? panicked at (.+):(\d+):(\d+):$`)

// locator maps a location reported by rustc or a panic (relative to the
// generated crate or project) to the location the user knows.
type locator func(file string, line, col int) (string, int, int)

// snippetLocator maps src/<path> locations through generated sources.
func snippetLocator(sources []compiler.SourceFile) locator {
	return func(file string, line, col int) (string, int, int) {
		f, l, c, _ := compiler.LocateGenerated(sources, file, line, col)
		return f, l, c
	}
}

// projectLocator prefixes project-relative paths with the project directory.
func projectLocator(dir string) locator {
	return func(file string, line, col int) (string, int, int) {
		if !filepath.IsAbs(file) && !strings.HasPrefix(file, "/rustc/") {
			// rustc reports src\main.rs on Windows.
			file = filepath.ToSlash(filepath.Join(dir, strings.ReplaceAll(file, "\\", "/")))
		}
		return file, line, col
	}
}

func (l locator) locate(file string, line, col int) (string, int, int) {
	if l == nil {
		return file, line, col
	}
	return l(file, line, col)
}

// parsePanic extracts the first panic from a program's stderr.
func parsePanic(stderr string, loc locator) *PanicError {
	m := panicHeader.FindStringSubmatchIndex(stderr)
	if m == nil {
		return nil
	}
	p := &PanicError{Thread: stderr[m[2]:m[3]], File: stderr[m[4]:m[5]]}
	p.Line, _ = strconv.Atoi(stderr[m[6]:m[7]])   // digits guaranteed by the regexp
	p.Column, _ = strconv.Atoi(stderr[m[8]:m[9]]) // digits guaranteed by the regexp
	p.File, p.Line, p.Column = loc.locate(p.File, p.Line, p.Column)
	rest := strings.TrimPrefix(stderr[m[1]:], "\n")
	var msg []string
	for line := range strings.SplitSeq(rest, "\n") {
		if strings.HasPrefix(line, "note: run with `RUST_BACKTRACE") || line == "stack backtrace:" ||
			panicHeader.MatchString(line) {
			break
		}
		msg = append(msg, line)
	}
	p.Message = strings.TrimRight(strings.Join(msg, "\n"), "\n")
	return p
}
