package ferrogo_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/alirezahematidev/ferrogo"
)

// Run a Rust program in the default backend, the WebAssembly sandbox.
// Requires Rust with the wasm32-wasip1 target.
func Example() {
	rt, err := ferrogo.New()
	if err != nil {
		log.Fatal(err)
	}
	defer rt.Close()

	res, err := rt.Run(context.Background(), ferrogo.Code(`
		fn main() {
			println!("Hello from Rust!");
		}
	`))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(res.Stdout)
}

// Configure a runtime for trusted code on the native-process backend.
func ExampleNew() {
	rt, err := ferrogo.New(
		ferrogo.WithBackend(ferrogo.BackendNativeProcess),
		ferrogo.WithTrust(ferrogo.Trusted), // native code runs with your privileges
		ferrogo.WithProfile(ferrogo.ProfileRelease),
		ferrogo.WithTimeout(10*time.Second),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer rt.Close()
}

// Pass input to a program and handle the ways it can fail.
func ExampleRuntime_Run() {
	rt, err := ferrogo.New()
	if err != nil {
		log.Fatal(err)
	}
	defer rt.Close()

	res, err := rt.Run(context.Background(), ferrogo.File("wordcount.rs"),
		ferrogo.WithStdin(strings.NewReader("the quick brown fox")),
		ferrogo.WithArgs("--verbose"),
		ferrogo.WithEnv(map[string]string{"LANG": "en"}),
		ferrogo.WithTimeout(5*time.Second),
	)
	var (
		compileErr *ferrogo.CompileError
		panicErr   *ferrogo.PanicError
		exitErr    *ferrogo.ExitError
	)
	switch {
	case err == nil:
		fmt.Print(res.Stdout)
	case errors.As(err, &compileErr):
		for _, d := range compileErr.Errors() {
			fmt.Printf("%s: %s\n", d.Location(), d.Message)
		}
	case errors.As(err, &panicErr):
		fmt.Printf("panic at %s:%d: %s\n", panicErr.File, panicErr.Line, panicErr.Message)
	case errors.As(err, &exitErr):
		fmt.Printf("exit status %d; stderr: %s\n", exitErr.Code, res.Stderr)
	case errors.Is(err, ferrogo.ErrTimeout):
		fmt.Printf("timed out; partial output: %s\n", res.Stdout)
	default:
		log.Fatal(err)
	}
}

// Grant sandboxed code read-only access to one directory.
func ExampleWithMount() {
	rt, err := ferrogo.New()
	if err != nil {
		log.Fatal(err)
	}
	defer rt.Close()

	res, err := rt.Run(context.Background(),
		ferrogo.Code(`fn main() { print!("{}", std::fs::read_to_string("/data/input.txt").unwrap()); }`),
		ferrogo.WithMount(ferrogo.Mount{HostPath: "./testdata", GuestPath: "/data", ReadOnly: true}),
	)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(res.Stdout)
}

// Compile without running, for example to validate code or ship the artifact.
func ExampleRuntime_Build() {
	rt, err := ferrogo.New()
	if err != nil {
		log.Fatal(err)
	}
	defer rt.Close()

	art, err := rt.Build(context.Background(), ferrogo.Code(`fn main() {}`), ferrogo.ArtifactProgram)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(art.Target, art.Path, art.Cached)
}

// Discover what a backend can do instead of assuming.
func ExampleCapabilitiesOf() {
	for _, b := range ferrogo.Backends {
		c := ferrogo.CapabilitiesOf(b)
		fmt.Printf("%-15s sandboxed=%-5v interruptible=%-5v requires-trust=%v\n",
			b, c.Sandboxed, c.Interruptible, c.RequiresTrust)
	}
	// Output:
	// wasm            sandboxed=true  interruptible=true  requires-trust=false
	// wazy            sandboxed=true  interruptible=true  requires-trust=false
	// native-process  sandboxed=false interruptible=true  requires-trust=true
	// native-library  sandboxed=false interruptible=false requires-trust=true
}

// Parse backend names from flags or configuration.
func ExampleParseBackend() {
	for _, name := range []string{"auto", "native", "dylib"} {
		b, err := ferrogo.ParseBackend(name)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(name, "=>", b)
	}
	// Output:
	// auto => auto
	// native => native-process
	// dylib => native-library
}

// Compile a library once and call its exported functions repeatedly.
func ExampleRuntime_Compile() {
	rt, err := ferrogo.New()
	if err != nil {
		log.Fatal(err)
	}
	defer rt.Close()

	mod, err := rt.Compile(context.Background(), ferrogo.Code(`
		ferrogo::export! {
			fn add(a: i32, b: i32) -> i32 { a + b }
			fn greet(name: String) -> Result<String, String> {
				if name.is_empty() { return Err("empty name".into()); }
				Ok(format!("Hello, {name}!"))
			}
		}
	`))
	if err != nil {
		log.Fatal(err)
	}
	defer mod.Close()

	sum, err := mod.Call(context.Background(), "add", ferrogo.Int32(10), ferrogo.Int32(20))
	if err != nil {
		log.Fatal(err)
	}
	n, _ := sum.AsInt32()
	fmt.Println(n) // 30

	_, err = mod.Call(context.Background(), "greet", ferrogo.String(""))
	var callErr *ferrogo.CallError
	if errors.As(err, &callErr) {
		fmt.Println(callErr.Message) // empty name
	}
}
