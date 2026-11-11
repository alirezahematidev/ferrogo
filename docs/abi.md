# FerroGo ABI v1 and wire format

This document specifies how Go calls Rust functions compiled by FerroGo. It
applies to the WASM and native-library backends, and to native-process calls
through the worker protocol (§11). All of them use the same encoding. The Rust side is
[`internal/abi/ferrogo.rs`](../internal/abi/ferrogo.rs) and the Go codec is
[`internal/wire`](../internal/wire).

## 1. Calling conventions

| Convention | Declared with | Types | Backends |
| --- | --- | --- | --- |
| **envelope** (recommended) | `ferrogo::export! { fn … }` | all wire v1 types, `Result<T, E>` | WASM, native library |
| **raw C** | `#[no_mangle] pub extern "C" fn` | scalars only | WASM (`Call`, signature read from the module; or `CallRaw`, declaration checked) and native library (`CallRaw` with a declared `Signature`) |

Arbitrary Rust functions cannot be called. Rust's own ABI is unstable, and
generics, references, traits and closures have no meaning across a language
boundary.

**Native libraries.** A native symbol table carries no types, so
`Module.Call` never guesses a signature. `Module.CallRaw(ctx, name,
ferrogo.Signature{Params: …, Result: …}, args...)` calls a raw function
through a signature you declare. purego implements the C calling
convention, including floating-point registers.

* The declaration is trusted, so a wrong one is undefined behavior. Native
  libraries already require `Trusted`.
* Raw calls are unavailable on Windows, because syscall cannot pass floats,
  and on the native-process backend.

## 2. Rust side: `ferrogo::export!`

FerroGo appends `mod ferrogo;` to every library crate it builds, and adds
the helper as `src/ferrogo.rs`. Appending keeps your line numbers unchanged.
The name `ferrogo` is therefore reserved at the crate root. Libraries
require edition 2018 or later (default: 2021).

```rust
ferrogo::export! {
    /// Adds two numbers.
    fn add(a: i32, b: i32) -> i32 {
        a + b
    }

    fn greet(name: String) -> Result<String, String> {
        if name.is_empty() { return Err("empty name".into()); }
        Ok(format!("Hello, {name}!"))
    }
}
```

The macro accepts any number of functions:

* doc comments and attributes
* optional `pub`
* parameters written `name: Type`
* an optional return type

Generic functions and pattern parameters (`mut x`, `(a, b)`) are not
supported.

| Wire tag | Rust parameter / return | Go `Value` kind | Go constructor / accessor |
| --- | --- | --- | --- |
| `0x00` | `()` (return only) | `KindVoid` | `Void()` |
| `0x01` | `bool` | `KindBool` | `Bool` / `AsBool` |
| `0x02` | `i32` | `KindInt32` | `Int32` / `AsInt32` |
| `0x03` | `i64` | `KindInt64` | `Int64` / `AsInt64` |
| `0x04` | `u32` | `KindUint32` | `Uint32` / `AsUint32` |
| `0x05` | `u64` | `KindUint64` | `Uint64` / `AsUint64` |
| `0x06` | `f32` | `KindFloat32` | `Float32` / `AsFloat32` |
| `0x07` | `f64` | `KindFloat64` | `Float64` / `AsFloat64` |
| `0x08` | `String` (`&str` as a return value) | `KindString` | `String` / `AsString` |
| `0x09` | `Vec<u8>` | `KindBytes` | `Bytes` / `AsBytes` |
| `0x0A` | `ferrogo::Json` (UTF-8 JSON text) | `KindJSON` | `JSON(v)`, `RawJSON(b)` / `AsJSON`, `Decode(&dst)` |

Return types may be wrapped in `Result<T, E>` where `E: Display`. An `Err`
becomes a `*ferrogo.CallError` carrying `e.to_string()`. Structs cross the
boundary as JSON: Go marshals with `encoding/json`, and Rust parses
`ferrogo::Json.0` with any JSON parser, for example `serde_json` added with
`WithDependencies`.

## 3. Symbols

For each exported function `<name>`:

| Symbol | C signature | Purpose |
| --- | --- | --- |
| `ferrogo_v1_call_<name>` | `uint32_t (const uint8_t *in, size_t len, fg_buf *out)` | the call |
| `ferrogo_v1_sig_<name>` | `uint32_t (fg_buf *out)` | Rust signature text, e.g. `fn add(a: i32, b: i32) -> i32` |

Once per module:

| Symbol | C signature | Purpose |
| --- | --- | --- |
| `ferrogo_abi_version` | `uint32_t (void)` | Returns `1`. Identifies a FerroGo ABI module. |
| `ferrogo_alloc` | `uint8_t *(size_t len)` | Allocates from the module's allocator. NULL on failure. |
| `ferrogo_free` | `void (uint8_t *ptr, size_t len)` | Frees a buffer from `ferrogo_alloc` or an out-slot. |
| `ferrogo_v1_last_panic` | `uint32_t (fg_buf *out)` | Returns the last panic on this thread (used after WASM traps). |

```c
typedef struct { uint8_t *ptr; size_t len; } fg_buf;   // #[repr(C)]
```

`fg_buf` is 8 bytes on `wasm32` and 16 bytes on 64-bit native targets.
Rust writes it with `write_unaligned`, so the host may place it anywhere.

Function names are discovered without executing code:

* WASM: from the module's export section.
* Native: from the symbol table, read with Go's `debug/elf` and
  `debug/macho`, and an export-directory reader for PE.

## 4. Status codes

| Code | Name | `*out` contains | Go result |
| --- | --- | --- | --- |
| 0 | OK | encoded result value (§5) | `Value` |
| 1 | ERROR | UTF-8 `Display` of the `Err` | `*CallError` |
| 2 | PANIC | `file:line:col` + `0x1F` + message | `*PanicError` (file re-mapped to your name) |
| 3 | ABI | UTF-8 description of the decode or type mismatch | wraps `ErrABIMismatch` |

Any other status is reported as `ErrABIMismatch`.

## 5. Wire format v1

Little-endian. Lengths are `u32`. Both directions are bounded by the
maximum payload (16 MiB).

```text
args   := version:u8 (=1) count:u32 value{count}
result := value
value  := tag:u8 payload
  0x00 void      (empty)
  0x01 bool      u8 (0 or 1)
  0x02 i32 / 0x04 u32 / 0x06 f32   4 bytes
  0x03 i64 / 0x05 u64 / 0x07 f64   8 bytes (floats as IEEE-754 bits)
  0x08 string / 0x09 bytes / 0x0A json   len:u32 bytes
```

Both decoders (Rust and Go) reject:

* unknown tags
* truncated input and trailing bytes
* lengths beyond the input or the payload limit
* argument-count mismatches
* invalid UTF-8 in `string` and `json`

Go also rejects JSON that is not syntactically valid. Kinds are never
coerced: an `Int64` passed to an `i32` parameter fails with
`ErrABIMismatch`. The Go decoder is fuzz-tested (`FuzzDecodeValue`).

## 6. Memory ownership

| Buffer | Allocated by | Freed by | Lifetime |
| --- | --- | --- | --- |
| Input (WASM) | host, via `ferrogo_alloc` | host, via `ferrogo_free` | the call |
| Input (native) | Go heap, pinned with `runtime.Pinner` | Go GC | the call. **Rust must not retain the pointer.** |
| `fg_buf` out-slot | host (guest memory on WASM, pinned Go memory on native) | host | the call |
| Result bytes | Rust (`Box<[u8]>`) | host calls `ferrogo_free(ptr, len)` after copying | until freed |

Each library has its own allocator, so buffers are never mixed between
modules. On the Go side, `unsafe` is confined to
`internal/backend/dylib/invoke.go`.

## 7. Panics

The helper installs a panic hook that records `file:line:col` and the
message in a thread-local, instead of printing to the host's stderr.

* **Native:** entry points wrap the call in `catch_unwind` and return
  status 2. Panics never unwind across the FFI boundary. FerroGo builds
  native libraries with `panic = "unwind"`.
* **WASM:** Rust's WASM targets abort on panic, so the instance traps. The
  host reads `ferrogo_v1_last_panic` from the trapped instance, reports
  `*PanicError`, and discards the instance. The next call uses a fresh
  instance whose statics are reset.

Both backends report the same `*PanicError`. This is verified by
`TestModulePanicsAreContained`.

## 8. Raw C convention on WASM

Arguments are matched against the export's core types:

| Core type | Accepted `Value` kinds |
| --- | --- |
| `i32` | `Int32`, `Uint32`, `Bool` |
| `i64` | `Int64`, `Uint64` |
| `f32` | `Float32` |
| `f64` | `Float64` |

Results come back as `Int32`, `Int64`, `Float32` or `Float64`, or as
`Void` when there is no result.

## 9. Versioning

* `ferrogo_abi_version` is incremented only for incompatible changes.
  Loading a module with a different version fails with `ErrABIMismatch`.
* New wire tags may be added. Older modules reject unknown tags with
  status 3, which is a clean error rather than corruption.
* The helper source is part of every library's cache key, so upgrading
  FerroGo never reuses an artifact built with an older helper.

## 10. Host functions (Rust → Go, WASM only)

Rust declares Go functions with `ferrogo::import!`. Each declaration
becomes a stub returning `Result<T, String>`:

```rust
ferrogo::import! {
    fn lookup_user(id: u64) -> String;   // -> Result<String, String>
    fn log(msg: String);                  // -> Result<(), String>
}
```

Go registers them with `WithHostFunction(name, func(ctx, args []Value) (Value, error))`.
The context passed to the host function is the context of the
`Module.Call` in progress.

All stubs go through one import, `ferrogo.ferrogo_host_call`:

```c
uint32_t ferrogo_host_call(const uint8_t *name, size_t name_len,
                           const uint8_t *in, size_t in_len, fg_buf *out);
```

| Aspect | Rule |
| --- | --- |
| Arguments | Encoded with the args format (§5). |
| Result | One encoded value with status 0, or a UTF-8 error message with status 1. |
| Result memory | The **host allocates it with the guest's `ferrogo_alloc`**. The guest takes ownership (`Box::from_raw`), so every buffer is freed by the allocator that created it. |
| Errors | Go errors, panics (recovered), and unknown names reach Rust as `Err(message)`. The guest instance stays healthy. |
| Native libraries | Host functions are not supported: the native-library backend rejects `WithHostFunction`, and the stubs return `Err` on native targets. |
| Capabilities | WASM code can call only the functions you register. |

Host functions also work in WASM **programs** (`Run`): FerroGo includes the
helper in every WASM program built with edition 2018 or later.

## 11. Native-process worker protocol

`Compile` on the native-process backend builds the library as a `cdylib`.
It also builds a small generic worker program once and caches it
([`internal/abi/worker.rs`](../internal/abi/worker.rs)). A pool of workers
serves calls over stdin and stdout:

```text
request:  op:u8 (1 = call, 2 = signature) name_len:u32 name in_len:u32 input
response: status:u32 out_len:u32 output        (status 255 = no such function)
```

`input`, `output` and the statuses are exactly those of §3–§5. The worker
calls `ferrogo_v1_call_<name>` and frees results with `ferrogo_free`. Its
protocol stdout is private: the process's standard output is redirected to
stderr, so `println!` in library code cannot corrupt the stream.

| Event | What happens |
| --- | --- |
| Timeout or cancellation | The worker's process tree is killed and the worker replaced. |
| Hard crash (abort, segfault) | The call fails with an error that includes the worker's stderr; the next call starts a fresh worker. |
| Concurrent calls | Each call gets its own worker. Rust statics are per worker, as with WASM instances. |
