# Getting started

## 1. Install the toolchain

```sh
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh   # or see rustup.rs
rustup target add wasm32-wasip1
go install github.com/alirezahematidev/ferrogo/cmd/ferrogo@latest
ferrogo doctor
```

`ferrogo doctor` prints one line per check, with a `fix:` line for anything
missing.

## 2. Run Rust from Go

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/alirezahematidev/ferrogo"
)

func main() {
    rt, err := ferrogo.New() // default: WASM sandbox, untrusted code
    if err != nil {
        log.Fatal(err)
    }
    defer rt.Close()

    res, err := rt.Run(context.Background(), ferrogo.Code(`
        fn main() {
            let squares: Vec<u32> = (1..=5).map(|n| n * n).collect();
            println!("{squares:?}");
        }
    `))
    if err != nil {
        log.Fatal(err)
    }
    fmt.Print(res.Stdout) // [1, 4, 9, 16, 25]
}
```

The first run compiles, which takes a few hundred milliseconds. Later runs
of the same code are served from the cache: `res.Artifact.Cached` is `true`,
and a warm WASM run takes under 1 ms.

## 3. Feed it input

```go
res, err := rt.Run(ctx, ferrogo.File("wordcount.rs"),
    ferrogo.WithStdin(strings.NewReader(text)),
    ferrogo.WithArgs("--top", "10"),
    ferrogo.WithEnv(map[string]string{"LANG": "en"}),
    ferrogo.WithMount(ferrogo.Mount{HostPath: "./corpus", GuestPath: "/corpus", ReadOnly: true}),
)
```

WASM code sees only what you grant: stdin, arguments, the environment
variables you pass, and mounted directories.

## 4. Handle failures

```go
res, err := rt.Run(ctx, src)
var (
    ce *ferrogo.CompileError
    pe *ferrogo.PanicError
    ee *ferrogo.ExitError
)
switch {
case err == nil:
case errors.As(err, &ce):            // rustc diagnostics, mapped to your file names
    fmt.Print(ce.Rendered())
case errors.As(err, &pe):            // Rust panic, with file:line
    fmt.Println(pe.File, pe.Line, pe.Message)
case errors.As(err, &ee):            // non-zero exit; res.Stdout/res.Stderr are set
    fmt.Println("exit", ee.Code)
case errors.Is(err, ferrogo.ErrTimeout), errors.Is(err, ferrogo.ErrOutputLimit),
    errors.Is(err, ferrogo.ErrMemoryLimit):
    // limit hit; res holds partial output
case errors.Is(err, ferrogo.ErrTargetNotInstalled):
    var te *ferrogo.TargetError
    errors.As(err, &te)
    fmt.Println("run:", te.Fix)      // e.g. rustup target add wasm32-wasip1
}
```

## 5. Native execution (trusted code only)

```go
rt, err := ferrogo.New(
    ferrogo.WithBackend(ferrogo.BackendNativeProcess),
    ferrogo.WithTrust(ferrogo.Trusted), // the code runs with your privileges
)
```

Native processes get threads, sockets and the full standard library, at
roughly native speed. They are not sandboxed. Read
[security.md](security.md) before enabling them.

## 6. Configure with a file

```sh
ferrogo init   # writes ferrogo.toml
```

```go
rt, err := ferrogo.New(ferrogo.WithConfigFile("ferrogo.toml"), ferrogo.WithTimeout(time.Second))
```

Options after `WithConfigFile` override the file. See
[configuration.md](configuration.md).

## Next

* [examples/](../examples): `basic`, `wasm` (sandbox limits), `native`
* [backends.md](backends.md): choosing a backend
* [troubleshooting.md](troubleshooting.md)
