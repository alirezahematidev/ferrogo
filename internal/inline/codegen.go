package inline

import (
	"bytes"
	"fmt"
	"go/format"
	"go/token"
	"sort"
	"strings"
	"text/template"
	"unicode"
)

// Signature is a parsed Rust function signature, as produced by
// ferrogo_v1_sig_<name>: "fn add(a: i32, b: i32) -> i32".
type Signature struct {
	Name   string
	Params []Param
	Ret    string // "" for no return value
}

// Param is a Rust parameter.
type Param struct{ Name, Type string }

// ParseSignature parses a signature string from the ABI helper.
func ParseSignature(s string) (Signature, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(s), "fn ")
	open := strings.IndexByte(rest, '(')
	if !ok || open <= 0 {
		return Signature{}, fmt.Errorf("unrecognised signature %q", s)
	}
	sig := Signature{Name: strings.TrimSpace(rest[:open])}
	depth, close := 0, -1
	for i := open; i < len(rest) && close < 0; i++ {
		switch rest[i] {
		case '(', '<', '[':
			depth++
		case ')', '>', ']':
			depth--
			if depth == 0 {
				close = i
			}
		}
	}
	if close < 0 {
		return Signature{}, fmt.Errorf("unbalanced signature %q", s)
	}
	for _, p := range splitTopLevel(rest[open+1 : close]) {
		name, typ, ok := strings.Cut(p, ":")
		if !ok {
			return Signature{}, fmt.Errorf("parameter %q in %q has no type", p, s)
		}
		sig.Params = append(sig.Params, Param{Name: strings.TrimSpace(name), Type: normType(typ)})
	}
	if ret, ok := strings.CutPrefix(strings.TrimSpace(rest[close+1:]), "->"); ok {
		sig.Ret = normType(ret)
	}
	return sig, nil
}

func splitTopLevel(s string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(', '<', '[':
			depth++
		case ')', '>', ']':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	if strings.TrimSpace(s[start:]) != "" {
		out = append(out, s[start:])
	}
	return out
}

// normType removes whitespace so "Vec < u8 >" and "Vec<u8>" compare equal.
func normType(t string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, t)
}

// goType describes how a Rust type crosses into Go.
type goType struct {
	Go       string // Go type
	Ctor     string // ferrogo.Value constructor
	Accessor string // Value accessor (results)
	Zero     string // zero value
	JSON     bool   // ferrogo::Json: any in, json.RawMessage out
}

var rustTypes = map[string]goType{
	"bool":    {"bool", "ferrogo.Bool", "AsBool", "false", false},
	"i32":     {"int32", "ferrogo.Int32", "AsInt32", "0", false},
	"i64":     {"int64", "ferrogo.Int64", "AsInt64", "0", false},
	"u32":     {"uint32", "ferrogo.Uint32", "AsUint32", "0", false},
	"u64":     {"uint64", "ferrogo.Uint64", "AsUint64", "0", false},
	"f32":     {"float32", "ferrogo.Float32", "AsFloat32", "0", false},
	"f64":     {"float64", "ferrogo.Float64", "AsFloat64", "0", false},
	"String":  {"string", "ferrogo.String", "AsString", `""`, false},
	"Vec<u8>": {"[]byte", "ferrogo.Bytes", "AsBytes", "nil", false},
	"Json":    {"json.RawMessage", "ferrogo.JSON", "AsJSON", "nil", true},
}

func lookupType(t string, result bool) (goType, error) {
	t = strings.TrimPrefix(strings.TrimPrefix(t, "crate::"), "ferrogo::")
	if result && (t == "&str" || t == "&'staticstr") {
		t = "String"
	}
	if g, ok := rustTypes[t]; ok {
		return g, nil
	}
	return goType{}, fmt.Errorf("type %s cannot cross the Go/Rust boundary (see docs/abi.md §2)", t)
}

// Config controls code generation.
type Config struct {
	Package string
	// TypeName is the generated module type (default "Rust").
	TypeName string
	// FuncPrefix prefixes the package-level convenience functions.
	FuncPrefix string
	// Existing holds the package's own top-level identifiers.
	Existing map[string]bool
	// Header lines recorded at the top of the generated file.
	SourceHash, Toolchain, Sources string
	// Docs holds Rust doc comments by function name (see DocComments).
	Docs map[string]string
}

type genFunc struct {
	RustName, GoName, Doc string
	DocLines              []string
	Params                []genParam
	HasResult             bool
	Result                goType
	NeedsJSON             bool
}

type genParam struct {
	Name string
	goType
}

// Generate renders the Go wrapper file for the given signatures.
func Generate(cfg Config, sigs []Signature) ([]byte, error) {
	if cfg.TypeName == "" {
		cfg.TypeName = "Rust"
	}
	if !token.IsIdentifier(cfg.TypeName) || !token.IsExported(cfg.TypeName) {
		return nil, fmt.Errorf("type name %q must be an exported Go identifier", cfg.TypeName)
	}
	taken := map[string]string{} // generated package-level name -> what uses it
	claim := func(name, what string) error {
		if cfg.Existing[name] {
			return fmt.Errorf("%s would generate %s, which the package already declares (use -prefix or -type)", what, name)
		}
		if prev, ok := taken[name]; ok {
			return fmt.Errorf("%s and %s both generate %s", prev, what, name)
		}
		taken[name] = what
		return nil
	}
	for _, n := range []string{cfg.TypeName, "Load" + cfg.TypeName, "ferrogoWASM", "ferrogoDefault"} {
		if err := claim(n, "the module type"); err != nil {
			return nil, err
		}
	}

	sort.Slice(sigs, func(i, j int) bool { return sigs[i].Name < sigs[j].Name })
	var funcs []genFunc
	usesJSON := false
	for _, sig := range sigs {
		f := genFunc{RustName: sig.Name, GoName: exportedName(sig.Name), Doc: sig.String()}
		if d := cfg.Docs[sig.Name]; d != "" {
			f.DocLines = strings.Split(d, "\n")
		}
		if f.GoName == "Close" || f.GoName == "Module" {
			return nil, fmt.Errorf("function %s would generate the reserved method %s; rename the Rust function", sig.Name, f.GoName)
		}
		if err := claim(cfg.FuncPrefix+f.GoName, "Rust function "+sig.Name); err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, p := range sig.Params {
			g, err := lookupType(p.Type, false)
			if err != nil {
				return nil, fmt.Errorf("function %s, parameter %s: %w", sig.Name, p.Name, err)
			}
			name := paramName(p.Name)
			for seen[name] {
				name += "_"
			}
			seen[name] = true
			f.Params = append(f.Params, genParam{Name: name, goType: g})
			f.NeedsJSON = f.NeedsJSON || g.JSON
		}
		if ret := unwrapResult(sig.Ret); ret != "" && ret != "()" {
			g, err := lookupType(ret, true)
			if err != nil {
				return nil, fmt.Errorf("function %s, result: %w", sig.Name, err)
			}
			f.HasResult, f.Result = true, g
			usesJSON = usesJSON || g.JSON
		}
		funcs = append(funcs, f)
	}

	var buf bytes.Buffer
	err := fileTemplate.Execute(&buf, map[string]any{
		"Cfg": cfg, "Funcs": funcs, "UsesJSON": usesJSON, "WASMFile": WASMFile,
	})
	if err != nil {
		return nil, err
	}
	out, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("formatting generated code: %w\n%s", err, buf.Bytes())
	}
	return out, nil
}

// String renders the signature the way Rust wrote it.
func (s Signature) String() string {
	parts := make([]string, len(s.Params))
	for i, p := range s.Params {
		parts[i] = p.Name + ": " + p.Type
	}
	out := "fn " + s.Name + "(" + strings.Join(parts, ", ") + ")"
	if s.Ret != "" {
		out += " -> " + s.Ret
	}
	return out
}

// unwrapResult turns Result<T,E> into T; errors surface as *ferrogo.CallError.
func unwrapResult(t string) string {
	inner, ok := strings.CutPrefix(t, "Result<")
	if !ok || !strings.HasSuffix(inner, ">") {
		return t
	}
	parts := splitTopLevel(strings.TrimSuffix(inner, ">"))
	if len(parts) == 0 {
		return t
	}
	return strings.TrimSpace(parts[0])
}

// exportedName converts snake_case to an exported CamelCase Go name.
func exportedName(rust string) string {
	var b strings.Builder
	upper := true
	for _, r := range strings.TrimPrefix(rust, "r#") {
		if r == '_' {
			upper = true
			continue
		}
		if upper {
			b.WriteRune(unicode.ToUpper(r))
			upper = false
		} else {
			b.WriteRune(r)
		}
	}
	name := b.String()
	if name == "" || !token.IsIdentifier(name) {
		name = "Fn" + name
	}
	return name
}

// paramName converts snake_case to lowerCamel and avoids keywords and the
// identifiers used inside generated function bodies.
func paramName(rust string) string {
	n := exportedName(rust)
	r := []rune(n)
	r[0] = unicode.ToLower(r[0])
	name := string(r)
	switch {
	case token.IsKeyword(name), name == "ctx", name == "r", name == "v", name == "err", name == "ferrogo", name == "json":
		return name + "Arg"
	}
	return name
}

var fileTemplate = template.Must(template.New("file").Parse(`// Code generated by ferrogo generate; DO NOT EDIT.
// Sources: {{.Cfg.Sources}}
// Toolchain: {{.Cfg.Toolchain}}
// Source hash: {{.Cfg.SourceHash}}

package {{.Cfg.Package}}

import (
	"context"
	_ "embed"
	"errors"
{{- if .UsesJSON}}
	"encoding/json"
{{- end}}
	"sync"

	"github.com/alirezahematidev/ferrogo"
)

// ferrogoWASM is the Rust code of this package, compiled to WebAssembly.
//
//go:embed {{.WASMFile}}
var ferrogoWASM []byte

// {{.Cfg.TypeName}} is a loaded instance of this package's Rust code. It is
// safe for concurrent use; see ferrogo.Module for the execution model.
type {{.Cfg.TypeName}} struct{ mod *ferrogo.Module }

// Load{{.Cfg.TypeName}} loads this package's Rust code into rt. It needs no
// Rust toolchain: the compiled WebAssembly is embedded in the binary.
func Load{{.Cfg.TypeName}}(ctx context.Context, rt *ferrogo.Runtime, opts ...ferrogo.CompileOption) (*{{.Cfg.TypeName}}, error) {
	m, err := rt.LoadWASM(ctx, ferrogoWASM, opts...)
	if err != nil {
		return nil, err
	}
	return &{{.Cfg.TypeName}}{mod: m}, nil
}

// Module returns the underlying module.
func (r *{{.Cfg.TypeName}}) Module() *ferrogo.Module { return r.mod }

// Close unloads the Rust code.
func (r *{{.Cfg.TypeName}}) Close() error { return r.mod.Close() }

// ferrogoDefault is the instance used by the package-level functions. It is
// created on first use with ferrogo's default (sandboxed) settings and lives
// for the rest of the process.
var ferrogoDefault = sync.OnceValues(func() (*{{.Cfg.TypeName}}, error) {
	rt, err := ferrogo.New()
	if err != nil {
		// No usable cache directory (e.g. a read-only file system).
		if rt, err = ferrogo.New(ferrogo.WithCache(false)); err != nil {
			return nil, err
		}
	}
	r, err := Load{{.Cfg.TypeName}}(context.Background(), rt)
	if err != nil {
		return nil, errors.Join(err, rt.Close())
	}
	return r, nil
})
{{range .Funcs}}{{$f := .}}
// {{.GoName}} calls the Rust function ` + "`{{.Doc}}`" + `.
{{- if .DocLines}}
//{{range .DocLines}}
//{{if .}} {{.}}{{end}}{{end}}
{{- end}}
func (r *{{$.Cfg.TypeName}}) {{.GoName}}(ctx context.Context{{range .Params}}, {{.Name}} {{if .JSON}}any{{else}}{{.Go}}{{end}}{{end}}) ({{if .HasResult}}{{.Result.Go}}, {{end}}error) {
{{- range .Params}}{{if .JSON}}
	{{.Name}}Value, err := ferrogo.JSON({{.Name}})
	if err != nil {
		return {{if $f.HasResult}}{{$f.Result.Zero}}, {{end}}err
	}
{{- end}}{{end}}
	{{if .HasResult}}v, err :={{else if .NeedsJSON}}_, err ={{else}}_, err :={{end}} r.mod.Call(ctx, "{{.RustName}}"{{range .Params}}, {{if .JSON}}{{.Name}}Value{{else}}{{.Ctor}}({{.Name}}){{end}}{{end}})
{{- if .HasResult}}
	if err != nil {
		return {{.Result.Zero}}, err
	}
{{- if .Result.JSON}}
	b, err := v.AsJSON()
	return json.RawMessage(b), err
{{- else}}
	return v.{{.Result.Accessor}}()
{{- end}}
{{- else}}
	return err
{{- end}}
}

// {{$.Cfg.FuncPrefix}}{{.GoName}} calls the Rust function ` + "`{{.Doc}}`" + ` on the package's
// default instance.
{{- if .DocLines}}
//{{range .DocLines}}
//{{if .}} {{.}}{{end}}{{end}}
{{- end}}
func {{$.Cfg.FuncPrefix}}{{.GoName}}(ctx context.Context{{range .Params}}, {{.Name}} {{if .JSON}}any{{else}}{{.Go}}{{end}}{{end}}) ({{if .HasResult}}{{.Result.Go}}, {{end}}error) {
	r, err := ferrogoDefault()
	if err != nil {
		return {{if .HasResult}}{{.Result.Zero}}, {{end}}err
	}
	return r.{{.GoName}}(ctx{{range .Params}}, {{.Name}}{{end}})
}
{{end}}`))
