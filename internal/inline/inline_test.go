package inline

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAutoExport(t *testing.T) {
	src := `#![allow(dead_code)]
use std::collections::HashMap;

/// Adds numbers.
#[inline]
pub fn add(a: i32, b: i32) -> i32 { a + b }

fn helper(x: [u8; 4]) -> usize { x.len() }
pub(crate) fn internal() {}
pub async fn later() {}
pub fn bytes(x: Vec<u8>) -> usize {
    let s = "pub fn fake() { } } {";
    let r = r#"pub fn raw() "#;
    let c = '}';
    let q = b'{';
    // pub fn commented() {}
    /* pub fn block() { /* nested */ } */
    fn inner() {}
    x.len() + s.len() + r.len() + (c as usize) + (q as usize)
}
pub struct S<'a> { s: &'a str }
impl S<'_> { pub fn method(&self) {} }
pub fn arr(x: [u8; 4]) -> u32 { x.len() as u32 }
ferrogo::export! { pub fn already(a: i32) -> i32 { a } }
`
	out := AutoExport(src)
	if got, want := strings.Count(out, "ferrogo::export!"), 4; got != want {
		t.Fatalf("got %d exports, want %d (add, bytes, arr + the existing one):\n%s", got, want, out)
	}
	if strings.Count(out, "\n") != strings.Count(src, "\n") {
		t.Error("AutoExport changed the number of lines")
	}
	for _, want := range []string{
		"ferrogo::export! { /// Adds numbers.\n#[inline]\npub fn add(a: i32, b: i32) -> i32 { a + b } }",
		"ferrogo::export! { pub fn arr(",
		"ferrogo::export! { pub fn bytes(",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, notWant := range []string{"export! { fn helper", "export! { pub(crate)", "export! { pub async", "export! { pub fn method", "export! { ferrogo::export!"} {
		if strings.Contains(out, notWant) {
			t.Errorf("output wrongly contains %q", notWant)
		}
	}
	if AutoExport("fn main() {}") != "fn main() {}" {
		t.Error("source without pub fn was modified")
	}
}

func TestExtract(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("b.go", "package calc\n\n/*ferrogo\npub fn add(a: i32, b: i32) -> i32 { a + b }\n*/\n\nfunc Existing() {}\n\nvar x, Y int\n")
	write("a.go", "package calc\n\n/* not ferrogo */\n/*ferrogoX\nignored\n*/\ntype T struct{}\n")
	write("a_test.go", "package calc\n/*ferrogo\npub fn from_test() {}\n*/\n")
	write("math.ferrogo.rs", "pub fn sub(a: i32, b: i32) -> i32 { a - b }\n")
	write(GoFile, "package calc\n/*ferrogo\npub fn from_generated() {}\n*/\nfunc Add() {}\n")

	pkg, err := Extract(dir)
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Name != "calc" || len(pkg.Blocks) != 2 {
		t.Fatalf("package %q with %d blocks: %+v", pkg.Name, len(pkg.Blocks), pkg.Blocks)
	}
	b := pkg.Blocks[0]
	if b.File != "b.go" || b.Line != 3 || b.Column != 10 || !strings.HasPrefix(b.Text, "\npub fn add") {
		t.Errorf("block = %+v", b)
	}
	if pkg.Blocks[1].File != "math.ferrogo.rs" || pkg.Blocks[1].Line != 1 {
		t.Errorf("rust file block = %+v", pkg.Blocks[1])
	}
	for _, d := range []string{"Existing", "x", "Y", "T"} {
		if !pkg.Decls[d] {
			t.Errorf("declaration %s not recorded", d)
		}
	}
	if pkg.Decls["Add"] {
		t.Error("the generated file's declarations must be ignored")
	}
	h1 := pkg.SourceHash()
	write("math.ferrogo.rs", "pub fn sub(a: i32, b: i32) -> i32 { b - a }\n")
	pkg2, _ := Extract(dir)
	if pkg2.SourceHash() == h1 {
		t.Error("source hash ignores content changes")
	}

	write("c.go", "package other\n")
	if _, err := Extract(dir); err == nil {
		t.Error("mixed package names accepted")
	}
}

func TestParseSignature(t *testing.T) {
	cases := map[string]Signature{
		"fn add(a: i32, b: i32) -> i32":              {Name: "add", Params: []Param{{"a", "i32"}, {"b", "i32"}}, Ret: "i32"},
		"fn nothing()":                               {Name: "nothing"},
		"fn parse(s: String) -> Result<u32, String>": {Name: "parse", Params: []Param{{"s", "String"}}, Ret: "Result<u32,String>"},
		"fn f(m: Vec < u8 >, j: ferrogo::Json)":      {Name: "f", Params: []Param{{"m", "Vec<u8>"}, {"j", "ferrogo::Json"}}},
	}
	for in, want := range cases {
		got, err := ParseSignature(in)
		if err != nil || got.String() != want.String() {
			t.Errorf("ParseSignature(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "add(a: i32)", "fn f(a i32)", "fn f(a: i32"} {
		if _, err := ParseSignature(bad); err == nil {
			t.Errorf("ParseSignature(%q) accepted", bad)
		}
	}
}

func TestGenerate(t *testing.T) {
	sigs := []Signature{}
	for _, s := range []string{
		"fn add(a: i32, b: i32) -> i32",
		"fn parse_port(s: String) -> Result<u32, String>",
		"fn stats(type: ferrogo::Json, ctx: Vec<u8>) -> ferrogo::Json",
		"fn reset()",
		"fn log(doc: ferrogo::Json)",
		"fn name() -> &str",
	} {
		sig, err := ParseSignature(s)
		if err != nil {
			t.Fatal(err)
		}
		sigs = append(sigs, sig)
	}
	src, err := Generate(Config{Package: "calc", SourceHash: "abc", Toolchain: "rustc 1.94.1", Sources: "calc.go"}, sigs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "gen.go", src, 0); err != nil {
		t.Fatalf("generated code does not parse: %v\n%s", err, src)
	}
	for _, want := range []string{
		"// Code generated by ferrogo generate; DO NOT EDIT.",
		"// Source hash: abc",
		"func (r *Rust) Add(ctx context.Context, a int32, b int32) (int32, error)",
		"func ParsePort(ctx context.Context, s string) (uint32, error)",
		"func (r *Rust) Stats(ctx context.Context, typeArg any, ctxArg []byte) (json.RawMessage, error)",
		"func Reset(ctx context.Context) error",
		"func Name(ctx context.Context) (string, error)",
		"//go:embed " + WASMFile,
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("generated code lacks %q", want)
		}
	}

	if _, err := Generate(Config{Package: "calc", Existing: map[string]bool{"Add": true}}, sigs); err == nil {
		t.Error("collision with an existing declaration not detected")
	}
	if _, err := Generate(Config{Package: "calc", Existing: map[string]bool{"Add": true}, FuncPrefix: "Rust"}, sigs); err != nil {
		t.Errorf("-prefix should resolve the collision: %v", err)
	}
	bad, _ := ParseSignature("fn f(x: Option<i32>)")
	if _, err := Generate(Config{Package: "calc"}, []Signature{bad}); err == nil || !strings.Contains(err.Error(), "Option<i32>") {
		t.Errorf("unsupported type: err = %v", err)
	}
	reserved, _ := ParseSignature("fn close()")
	if _, err := Generate(Config{Package: "calc"}, []Signature{reserved}); err == nil {
		t.Error("reserved method name accepted")
	}
}

func TestDocComments(t *testing.T) {
	src := `/// Adds two numbers.
///
/// Wraps on overflow.
#[inline]
pub fn add(a: i32, b: i32) -> i32 { a.wrapping_add(b) }

// plain comment, not docs
pub fn undocumented() {}

ferrogo::export! {
    /// Inside the macro.
    fn inner(x: u32) -> u32 { x }
}

/// Belongs to the struct.
pub struct S;
pub fn after_struct() {}
`
	docs := DocComments(src)
	if docs["add"] != "Adds two numbers.\n\nWraps on overflow." || docs["inner"] != "Inside the macro." {
		t.Errorf("docs = %q", docs)
	}
	for _, name := range []string{"undocumented", "after_struct"} {
		if _, ok := docs[name]; ok {
			t.Errorf("%s wrongly documented: %q", name, docs[name])
		}
	}
	sig, _ := ParseSignature("fn add(a: i32, b: i32) -> i32")
	src2, err := Generate(Config{Package: "p", Docs: docs}, []Signature{sig})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src2), "// Add calls the Rust function `fn add(a: i32, b: i32) -> i32`.\n//\n// Adds two numbers.\n//\n// Wraps on overflow.\nfunc (r *Rust) Add(") {
		t.Errorf("generated docs:\n%s", src2)
	}
}
