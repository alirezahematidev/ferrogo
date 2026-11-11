package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/alirezahematidev/ferrogo"
	"github.com/alirezahematidev/ferrogo/internal/inline"
)

// loadForCall compiles a .rs file or project as a library, or loads a
// prebuilt artifact (.wasm, .so, .dylib, .dll).
func loadForCall(ctx context.Context, rt *ferrogo.Runtime, src string) (*ferrogo.Module, error) {
	switch strings.ToLower(filepath.Ext(src)) {
	case ".wasm", ".so", ".dylib", ".dll":
		return rt.LoadModule(ctx, src)
	}
	if fi, err := os.Stat(src); err == nil && fi.IsDir() {
		return rt.Compile(ctx, ferrogo.Project(src))
	}
	return rt.Compile(ctx, ferrogo.File(src))
}

func cmdCall(ctx context.Context, out *output, _ io.Reader, args []string) error {
	c := newCommon("call", "call [flags] <file.rs | dir | artifact> <function> [args...]\n\n"+
		"Arguments are converted using the Rust signature: integers and floats as decimal,\n"+
		"bool as true/false, String as-is, Vec<u8> as hex, ferrogo::Json as JSON text.", out.stderr)
	positional, rest, err := c.parseN(args, 2) // flags must precede <function>
	if err != nil {
		return err
	}
	positional = append(positional, rest...)
	if len(positional) < 2 {
		c.fs.Usage()
		return errUsage
	}
	src, fn, rawArgs := positional[0], positional[1], positional[2:]
	if fi, err := os.Stat(src); err == nil && fi.IsDir() {
		c.trust = true // like `cargo run`: the user points at their own project
	}
	rt, err := c.runtime(out)
	if err != nil {
		return err
	}
	defer rt.Close()
	mod, err := loadForCall(ctx, rt, src)
	if err != nil {
		out.failure(err)
		return exitError{code: 1}
	}
	defer mod.Close()

	sig, err := mod.Functions(ctx)
	if err != nil {
		return err
	}
	var signature string
	for _, f := range sig {
		if f.Name == fn {
			signature = f.Signature
		}
	}
	if signature == "" {
		names := make([]string, len(sig))
		for i, f := range sig {
			names[i] = f.Name
		}
		return fmt.Errorf("%s exports no function %q (exports: %s)", src, fn, strings.Join(names, ", "))
	}
	parsed, err := inline.ParseSignature(signature)
	if err != nil {
		return err
	}
	if len(rawArgs) != len(parsed.Params) {
		return fmt.Errorf("%s takes %d argument(s), got %d", signature, len(parsed.Params), len(rawArgs))
	}
	values := make([]ferrogo.Value, len(rawArgs))
	for i, p := range parsed.Params {
		if values[i], err = parseArg(p.Type, rawArgs[i]); err != nil {
			return fmt.Errorf("argument %s: %w", p.Name, err)
		}
	}
	v, err := mod.Call(ctx, fn, values...)
	if err != nil {
		out.failure(err)
		return exitError{code: 1}
	}
	if c.json {
		return out.json(map[string]any{"function": signature, "kind": v.Kind().String(), "result": resultJSON(v)})
	}
	fmt.Fprintln(out.stdout, formatResult(v))
	return nil
}

// parseArg converts a command-line argument to the Rust parameter type.
func parseArg(rustType, s string) (ferrogo.Value, error) {
	t := strings.TrimPrefix(strings.TrimPrefix(rustType, "crate::"), "ferrogo::")
	bits := map[string]int{"i32": 32, "i64": 64, "u32": 32, "u64": 64}
	switch t {
	case "bool":
		b, err := strconv.ParseBool(s)
		return ferrogo.Bool(b), err
	case "i32", "i64":
		n, err := strconv.ParseInt(s, 10, bits[t])
		if t == "i32" {
			return ferrogo.Int32(int32(n)), err
		}
		return ferrogo.Int64(n), err
	case "u32", "u64":
		n, err := strconv.ParseUint(s, 10, bits[t])
		if t == "u32" {
			return ferrogo.Uint32(uint32(n)), err
		}
		return ferrogo.Uint64(n), err
	case "f32":
		f, err := strconv.ParseFloat(s, 32)
		return ferrogo.Float32(float32(f)), err
	case "f64":
		f, err := strconv.ParseFloat(s, 64)
		return ferrogo.Float64(f), err
	case "String":
		return ferrogo.String(s), nil
	case "Vec<u8>":
		b, err := hex.DecodeString(s)
		return ferrogo.Bytes(b), err
	case "Json":
		return ferrogo.RawJSON([]byte(s))
	}
	return ferrogo.Value{}, fmt.Errorf("type %s cannot be passed from the command line", rustType)
}

func formatResult(v ferrogo.Value) string {
	switch v.Kind() {
	case ferrogo.KindVoid:
		return "()"
	case ferrogo.KindString:
		s, _ := v.AsString()
		return s
	case ferrogo.KindBytes:
		b, _ := v.AsBytes()
		return hex.EncodeToString(b)
	case ferrogo.KindJSON:
		b, _ := v.AsJSON()
		return string(b)
	}
	s := v.String() // e.g. i32(30) -> 30
	if i := strings.IndexByte(s, '('); i >= 0 && strings.HasSuffix(s, ")") {
		return s[i+1 : len(s)-1]
	}
	return s
}

func resultJSON(v ferrogo.Value) any {
	if v.Kind() == ferrogo.KindJSON {
		b, _ := v.AsJSON()
		return json.RawMessage(b)
	}
	if v.Kind() == ferrogo.KindString {
		s, _ := v.AsString()
		return s
	}
	return formatResult(v)
}

// inspectArtifact describes a prebuilt module.
func inspectArtifact(ctx context.Context, c *common, out *output, path string) error {
	rt, err := c.runtime(out)
	if err != nil {
		return err
	}
	defer rt.Close()
	mod, err := rt.LoadModule(ctx, path)
	if err != nil {
		if errors.Is(err, ferrogo.ErrUntrusted) {
			return fmt.Errorf("%w (loading a native library runs its initialisation code; pass --trust)", err)
		}
		return err
	}
	defer mod.Close()
	fns, err := mod.Functions(ctx)
	if err != nil {
		return err
	}
	info := mod.Info()
	if c.json {
		return out.json(map[string]any{"path": info.Path, "backend": info.Backend, "functions": fns})
	}
	fmt.Fprintf(out.stdout, "%s (%s backend)\n", info.Path, info.Backend)
	if len(fns) == 0 {
		fmt.Fprintln(out.stdout, "  no ferrogo::export! functions")
	}
	for _, f := range fns {
		fmt.Fprintf(out.stdout, "  %s\n", f.Signature)
	}
	return nil
}
