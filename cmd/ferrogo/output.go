package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/alirezahematidev/ferrogo"
)

// output renders human-readable (optionally coloured) and JSON output.
type output struct {
	stdout, stderr io.Writer
	color          bool
}

func newOutput(stdout, stderr io.Writer) *output {
	return &output{stdout: stdout, stderr: stderr, color: isTerminal(stderr)}
}

// isTerminal reports whether w is a character device and colours are wanted.
func isTerminal(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

const (
	red    = "\x1b[1;31m"
	yellow = "\x1b[1;33m"
	green  = "\x1b[1;32m"
	bold   = "\x1b[1m"
	dim    = "\x1b[2m"
	reset  = "\x1b[0m"
)

func (o *output) paint(style, s string) string {
	if !o.color {
		return s
	}
	return style + s + reset
}

func (o *output) errorf(format string, args ...any) {
	fmt.Fprintf(o.stderr, "%s %s\n", o.paint(red, "error:"), fmt.Sprintf(format, args...))
}

func (o *output) status(label, format string, args ...any) {
	fmt.Fprintf(o.stderr, "%s %s\n", o.paint(green, fmt.Sprintf("%12s", label)), fmt.Sprintf(format, args...))
}

// diagnostics prints rustc-rendered diagnostics, colouring severities.
func (o *output) diagnostics(diags []ferrogo.Diagnostic) {
	for _, d := range diags {
		text := d.Rendered
		if text == "" {
			text = d.Severity + ": " + d.Message + "\n"
		}
		if o.color {
			for _, sev := range []struct{ prefix, style string }{{"error", red}, {"warning", yellow}} {
				if strings.HasPrefix(text, sev.prefix) {
					text = o.paint(sev.style, sev.prefix) + text[len(sev.prefix):]
				}
			}
		}
		fmt.Fprint(o.stderr, text)
	}
}

// goStyleDiagnostics prints diagnostics as "file:line:col: severity: message"
// lines, which editors and `go generate` output make clickable.
func (o *output) goStyleDiagnostics(diags []ferrogo.Diagnostic) {
	for _, d := range diags {
		sev := d.Severity
		if d.Code != "" {
			sev += "[" + d.Code + "]"
		}
		loc := d.Location()
		if loc == "" {
			loc = "ferrogo"
		}
		style := yellow
		if strings.HasPrefix(d.Severity, "error") {
			style = red
		}
		fmt.Fprintf(o.stderr, "%s: %s %s\n", loc, o.paint(style, sev+":"), d.Message)
		for _, s := range d.Spans {
			if s.Label != "" && !s.Primary {
				fmt.Fprintf(o.stderr, "\t%s:%d:%d: %s\n", s.File, s.Line, s.Column, s.Label)
			} else if s.Label != "" {
				fmt.Fprintf(o.stderr, "\t%s\n", s.Label)
			}
		}
		for _, ch := range d.Children {
			if ch.Message != "" {
				fmt.Fprintf(o.stderr, "\t%s: %s\n", ch.Severity, ch.Message)
			}
		}
	}
}

// failure prints an error in the most helpful form for its type.
func (o *output) failure(err error) {
	var ce *ferrogo.CompileError
	if errors.As(err, &ce) {
		if len(ce.Diagnostics) > 0 {
			o.diagnostics(ce.Diagnostics)
		} else if ce.Output != "" {
			fmt.Fprint(o.stderr, ce.Output)
		}
	}
	o.errorf("%v", err)
}

func (o *output) json(v any) error {
	enc := json.NewEncoder(o.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// jsonError is the JSON shape of an error.
type jsonError struct {
	Message     string               `json:"message"`
	Kind        string               `json:"kind"`
	Diagnostics []ferrogo.Diagnostic `json:"diagnostics,omitempty"`
	Panic       *ferrogo.PanicError  `json:"panic,omitempty"`
}

func toJSONError(err error) *jsonError {
	if err == nil {
		return nil
	}
	je := &jsonError{Message: err.Error(), Kind: "error"}
	var (
		ce *ferrogo.CompileError
		ee *ferrogo.ExitError
		te *ferrogo.TargetError
	)
	switch {
	case errors.As(err, &ce):
		je.Kind, je.Diagnostics = "compile", ce.Diagnostics
	case errors.As(err, &ee):
		je.Kind, je.Panic = "exit", ee.Panic
		if ee.Panic != nil {
			je.Kind = "panic"
		}
	case errors.As(err, &te):
		je.Kind = "target"
	case errors.Is(err, ferrogo.ErrTimeout):
		je.Kind = "timeout"
	case errors.Is(err, ferrogo.ErrOutputLimit):
		je.Kind = "output_limit"
	case errors.Is(err, ferrogo.ErrUntrusted):
		je.Kind = "untrusted"
	case errors.Is(err, ferrogo.ErrUnsupported):
		je.Kind = "unsupported"
	case errors.Is(err, ferrogo.ErrToolchainNotFound):
		je.Kind = "toolchain"
	}
	return je
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
