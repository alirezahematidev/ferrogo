// Command functions compiles a Rust library once and calls its exported
// functions repeatedly: on the WASM sandbox, and (as trusted code) as a
// native shared library loaded into this process.
//
//	go run ./examples/functions
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/alirezahematidev/ferrogo"
)

const library = `
ferrogo::export! {
    /// Adds two numbers.
    fn add(a: i32, b: i32) -> i32 { a + b }

    /// Returns Err for bad input: surfaces in Go as *ferrogo.CallError.
    fn parse_port(s: String) -> Result<u32, String> {
        let port: u32 = s.trim().parse().map_err(|e| format!("{s:?}: {e}"))?;
        if port == 0 || port > 65535 { return Err(format!("{port} is out of range")); }
        Ok(port)
    }

    /// Structs cross the boundary as JSON.
    fn stats(numbers: ferrogo::Json) -> ferrogo::Json {
        let nums: Vec<f64> = numbers.0.trim_matches(|c| c == '[' || c == ']')
            .split(',').filter(|s| !s.trim().is_empty())
            .map(|s| s.trim().parse().unwrap()).collect();
        let sum: f64 = nums.iter().sum();
        ferrogo::Json(format!("{{\"count\":{},\"sum\":{},\"mean\":{}}}", nums.len(), sum, sum / nums.len() as f64))
    }
}

/// A plain C-ABI export: callable on the WASM backend without the macro.
#[no_mangle]
pub extern "C" fn calculate(a: i32, b: i32) -> i32 { a * b }
`

func main() {
	ctx := context.Background()
	rt, err := ferrogo.New(ferrogo.WithTrust(ferrogo.Trusted))
	if err != nil {
		log.Fatal(err)
	}
	defer rt.Close()

	for _, backend := range []ferrogo.Backend{ferrogo.BackendWASM, ferrogo.BackendNativeLibrary} {
		if !ferrogo.CapabilitiesOf(backend).Call {
			fmt.Printf("== %s: not supported on this platform\n", backend)
			continue
		}
		fmt.Printf("== %s\n", backend)
		start := time.Now()
		mod, err := rt.Compile(ctx, ferrogo.Code(library), ferrogo.WithBackend(backend))
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("compiled in %s\n", time.Since(start).Round(time.Millisecond))

		fns, err := mod.Functions(ctx)
		if err != nil {
			log.Fatal(err)
		}
		for _, f := range fns {
			fmt.Println("  export:", f.Signature)
		}

		sum, err := mod.Call(ctx, "add", ferrogo.Int32(10), ferrogo.Int32(20))
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("add(10, 20) =", sum)

		const calls = 10000
		start = time.Now()
		for i := range calls {
			if _, err := mod.Call(ctx, "add", ferrogo.Int32(int32(i)), ferrogo.Int32(1)); err != nil {
				log.Fatal(err)
			}
		}
		fmt.Printf("%d calls: %s per call\n", calls, (time.Since(start) / calls).Round(10*time.Nanosecond))

		_, err = mod.Call(ctx, "parse_port", ferrogo.String("99999"))
		var callErr *ferrogo.CallError
		if errors.As(err, &callErr) {
			fmt.Println("parse_port error:", callErr.Message)
		}

		in, err := ferrogo.JSON([]float64{2, 4, 9})
		if err != nil {
			log.Fatal(err)
		}
		out, err := mod.Call(ctx, "stats", in)
		if err != nil {
			log.Fatal(err)
		}
		var st struct{ Count, Sum, Mean float64 }
		if err := out.Decode(&st); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("stats: %+v\n", st)

		if backend == ferrogo.BackendWASM {
			product, err := mod.Call(ctx, "calculate", ferrogo.Int32(6), ferrogo.Int32(7))
			if err != nil {
				log.Fatal(err)
			}
			fmt.Println("raw calculate(6, 7) =", product)
		}
		if err := mod.Close(); err != nil {
			log.Fatal(err)
		}
	}
}
