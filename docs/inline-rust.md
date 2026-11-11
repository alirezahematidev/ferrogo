# Inline Rust

Write Rust inside your Go package and call it like Go:

```go
package calc

//go:generate go run github.com/alirezahematidev/ferrogo/cmd/ferrogo generate

/*ferrogo
pub fn add(a: i32, b: i32) -> i32 { a + b }

pub fn parse_port(s: String) -> Result<u32, String> {
    s.trim().parse().map_err(|e| format!("{s:?}: {e}"))
}
*/
```

```sh
go generate ./...
```

```go
sum, err := calc.Add(ctx, 2, 40)           // 42
port, err := calc.ParsePort(ctx, "8080")   // 8080; Err(..) becomes *ferrogo.CallError
```

Runnable example: [examples/inline](../examples/inline/main.go).

## How it works

Go cannot compile Rust syntax, and FerroGo does not pretend otherwise.

1. **The Rust lives in a comment.** It goes in a `/*ferrogo … */` block
   comment, the same idea as cgo's preamble, so the file stays valid Go and
   `gopls`, `go vet` and `go build` keep working. Standalone
   `*.ferrogo.rs` files in the package directory are included too, and
   rust-analyzer can open those.
2. **`ferrogo generate` compiles it.** It runs before `go build`, like
   `stringer` or `protoc`:
   1. collects every block and file in the package, sorted by file and
      position;
   2. compiles them as one crate to `wasm32-wasip1`;
   3. reads each exported function's real signature from the compiled
      module;
   4. writes two files:
      * `ferrogo_generated.wasm`: the module, embedded with `go:embed`;
      * `ferrogo_generated.go`: typed wrappers, marked
        `// Code generated … DO NOT EDIT.`
3. **Commit both generated files.** Programs that use the package then need
   **no Rust toolchain**: the WASM is inside the Go binary, and FerroGo
   loads it with `LoadWASM`.

## What gets exported

Every top-level `pub fn` in a block is exported. FerroGo wraps it in
`ferrogo::export!` on the same lines, so error line numbers stay exact.
Everything else is ordinary private Rust:

* helpers without `pub`
* `pub(crate)` items
* structs and `impl` blocks
* `use` statements

You can also write `ferrogo::export! { … }` yourself.

Exported functions must use the boundary types in [abi.md](abi.md) §2.

| Rust | Go parameter | Go result |
| --- | --- | --- |
| `bool`, `i32`, `i64`, `u32`, `u64`, `f32`, `f64` | `bool`, `int32`, `int64`, `uint32`, `uint64`, `float32`, `float64` | same |
| `String` (`&str` as a result) | `string` | `string` |
| `Vec<u8>` | `[]byte` | `[]byte` |
| `ferrogo::Json` | `any` (marshalled with `encoding/json`) | `json.RawMessage` |
| `Result<T, E>` | n/a | `T`. `Err(e)` becomes `*ferrogo.CallError` |
| `()` | n/a | only `error` |

Unsupported types, such as `Option<T>` or references in parameters, are
rejected by `ferrogo generate` with the function and parameter name.

## Generated API

For a package with `pub fn add(a: i32, b: i32) -> i32`:

```go
type Rust struct{ /* … */ }
func LoadRust(ctx, rt *ferrogo.Runtime, opts ...ferrogo.CompileOption) (*Rust, error)
func (r *Rust) Add(ctx context.Context, a int32, b int32) (int32, error)
func (r *Rust) Module() *ferrogo.Module
func (r *Rust) Close() error

func Add(ctx context.Context, a int32, b int32) (int32, error) // uses a lazily created default instance
```

* **Package-level functions** use a default instance. It is created on
  first use with FerroGo's defaults: sandboxed, with a 30s call timeout and
  256 MiB of guest memory.
* **`LoadRust`** loads the code into your own `Runtime` instead, so you
  choose the limits and lifetime:
  `LoadRust(ctx, rt, ferrogo.WithTimeout(time.Second))`.
* **Names:** `snake_case` becomes `CamelCase`. If a generated name collides
  with a declaration in your package, generation fails; rename the
  function, or pass `-prefix Rust` (giving `RustAdd`) or `-type Name`.

## Diagnostics

Compiler errors point at your `.go` file in `file:line:col:` form, which
editors make clickable:

```text
calc.go:9:41: error[E0277]: cannot add `&str` to `i32`
	no implementation for `i32 + &str`
```

Panics inside called functions report the `.go` location too
(`*ferrogo.PanicError`).

## Reproducibility and CI

* The generated Go file is deterministic. Its header records the toolchain
  and a SHA-256 **source hash** of the extracted Rust.
* The WASM is byte-identical for the same rustc version: build paths are
  remapped. A different rustc may produce different, but equivalent, bytes.
* `ferrogo generate -check` verifies that the generated files match the
  current Rust sources. It needs no Rust toolchain, so it is cheap to run in
  CI:

```sh
go run github.com/alirezahematidev/ferrogo/cmd/ferrogo generate -check ./calc
```

## Limitations

* A block cannot contain `*/`, because it would end the Go comment. Use `//`
  comments in Rust, or move the code to a `*.ferrogo.rs` file.
* All blocks of a package form one crate. Items are shared between blocks.
* Generated code always targets WASM. A native library is
  platform-specific and could not be embedded portably.
* Cargo dependencies in inline code will follow Phase 6.
* Rust `///` doc comments are copied into the wrappers' GoDoc.
* To give inline Rust host functions (`ferrogo::import!`), load it with
  `LoadRust(ctx, rt, ferrogo.WithHostFunction(…))`. The package-level
  default instance registers none.
