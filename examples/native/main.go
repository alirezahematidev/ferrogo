// Command native runs Rust programs as native child processes.
//
// The native-process backend is not a sandbox: programs run with your
// privileges, so it must be enabled explicitly with WithTrust(Trusted).
//
//	go run ./examples/native
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/alirezahematidev/ferrogo"
)

func main() {
	ctx := context.Background()
	rt, err := ferrogo.New(
		ferrogo.WithBackend(ferrogo.BackendNativeProcess),
		ferrogo.WithTrust(ferrogo.Trusted), // we wrote this code ourselves
		ferrogo.WithTimeout(10*time.Second),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer rt.Close()

	// 1. stdin, arguments and environment variables.
	res, err := rt.Run(ctx, ferrogo.Code(`
use std::io::Read;

fn main() {
    let mut text = String::new();
    std::io::stdin().read_to_string(&mut text).unwrap();
    let words = text.split_whitespace().count();
    let unit = std::env::var("UNIT").unwrap_or_else(|_| "words".into());
    let label = std::env::args().nth(1).unwrap_or_default();
    println!("{label}: {words} {unit}");
}
`),
		ferrogo.WithStdin(strings.NewReader("the quick brown fox jumps over the lazy dog")),
		ferrogo.WithArgs("pangram"),
		ferrogo.WithEnv(map[string]string{"UNIT": "words"}),
	)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(res.Stdout)
	fmt.Printf("  (compiled: %v, cached: %v, ran in %s)\n",
		res.Artifact.BuildDuration.Round(time.Millisecond), res.Artifact.Cached, res.Duration.Round(time.Microsecond))

	// 2. Compiler diagnostics are structured.
	_, err = rt.Run(ctx, ferrogo.Code("fn main() {\n    let n: u8 = 300;\n}\n"))
	var ce *ferrogo.CompileError
	if errors.As(err, &ce) {
		for _, d := range ce.Errors() {
			fmt.Printf("compile error at %s: %s\n", d.Location(), d.Message)
		}
	}

	// 3. Panics are reported with their location.
	_, err = rt.Run(ctx, ferrogo.Code("fn main() {\n    let v = vec![1, 2, 3];\n    println!(\"{}\", v[10]);\n}\n"))
	var p *ferrogo.PanicError
	if errors.As(err, &p) {
		fmt.Printf("panic at %s:%d: %s\n", p.File, p.Line, p.Message)
	}

	// 4. Timeouts stop runaway programs.
	_, err = rt.Run(ctx, ferrogo.Code(`fn main() { loop { std::hint::black_box(0); } }`),
		ferrogo.WithTimeout(2*time.Second))
	fmt.Println("infinite loop stopped:", errors.Is(err, ferrogo.ErrTimeout))
}
