// Command basic is the FerroGo quick start: compile a Rust snippet and run it
// in the default backend (the WebAssembly sandbox).
//
//	go run ./examples/basic
//
// Requires Rust with the wasm32-wasip1 target:
//
//	rustup target add wasm32-wasip1
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/alirezahematidev/ferrogo"
)

func main() {
	ctx := context.Background()

	runtime, err := ferrogo.New(
		ferrogo.WithBackend(ferrogo.BackendWASM),
		ferrogo.WithCache(true),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer runtime.Close()

	result, err := runtime.Run(ctx, ferrogo.Code(`
        fn main() {
            println!("Hello from Rust!");
        }
    `))
	if err != nil {
		log.Fatal(err)
	}

	fmt.Print(result.Stdout)
	fmt.Printf("backend=%s target=%s cached=%v\n", result.Backend, result.Artifact.Target, result.Artifact.Cached)
}
