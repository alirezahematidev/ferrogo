// Command inline shows Rust written directly inside a Go file.
//
// The Rust below lives in a /*ferrogo ... */ comment, so this file is valid
// Go and gopls, go vet and go build work as usual. `ferrogo generate`
// compiles it to WebAssembly and writes typed wrappers (ferrogo_generated.go)
// plus the embedded module (ferrogo_generated.wasm). Running the program
// needs no Rust toolchain.
//
// To regenerate after editing the Rust code:
//
//	go generate ./examples/inline
//
//go:generate go run github.com/alirezahematidev/ferrogo/cmd/ferrogo generate
package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/alirezahematidev/ferrogo"
)

/*ferrogo
/// Returns the n-th Fibonacci number.
pub fn fib(n: u32) -> u64 {
    let (mut a, mut b) = (0u64, 1u64);
    for _ in 0..n {
        (a, b) = (b, a.wrapping_add(b));
    }
    a
}

/// Counts whitespace-separated words.
pub fn word_count(text: String) -> u32 {
    text.split_whitespace().count() as u32
}

/// Fails on empty input: the error surfaces in Go as *ferrogo.CallError.
pub fn shout(text: String) -> Result<String, String> {
    if text.trim().is_empty() {
        return Err("nothing to shout".to_string());
    }
    Ok(format!("{}!", text.to_uppercase()))
}

// Not exported to Go: no `pub`.
fn checksum(data: &[u8]) -> u32 {
    data.iter().fold(0u32, |acc, b| acc.rotate_left(5) ^ u32::from(*b))
}

pub fn digest(data: Vec<u8>) -> u32 {
    checksum(&data)
}
*/

func main() {
	ctx := context.Background()

	n, err := Fib(ctx, 90)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("fib(90) =", n)

	words, err := WordCount(ctx, "the quick brown fox jumps over the lazy dog")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("word_count =", words)

	loud, err := Shout(ctx, "hello from rust")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("shout =", loud)

	_, err = Shout(ctx, "   ")
	var callErr *ferrogo.CallError
	if errors.As(err, &callErr) {
		fmt.Println("shout error =", callErr.Message)
	}

	sum, err := Digest(ctx, []byte("ferrogo"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("digest = %#x\n", sum)
}
