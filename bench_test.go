package ferrogo_test

import (
	"context"
	"os/exec"
	"testing"

	"github.com/alirezahematidev/ferrogo"
)

// Benchmarks compare backends on identical workloads. They need a Rust
// toolchain with the wasm32-wasip1 target:
//
//	go test -run '^$' -bench . -benchtime 50x

const benchProgram = `fn main() {
    let n: u64 = std::env::args().nth(1).map(|a| a.parse().unwrap()).unwrap_or(30);
    fn fib(n: u64) -> u64 { if n < 2 { n } else { fib(n - 1) + fib(n - 2) } }
    println!("{}", fib(n));
}`

func benchRuntime(b *testing.B, backend ferrogo.Backend) *ferrogo.Runtime {
	b.Helper()
	if _, err := exec.LookPath("rustc"); err != nil {
		b.Skip("rustc not installed")
	}
	rt, err := ferrogo.New(ferrogo.WithCacheDir(sharedCache), ferrogo.WithBackend(backend), ferrogo.WithTrust(ferrogo.Trusted))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { rt.Close() })
	// Warm the artifact cache and, for native, the OS's first-launch checks.
	if _, err := rt.Run(context.Background(), ferrogo.Code(benchProgram), ferrogo.WithArgs("1")); err != nil {
		b.Fatal(err)
	}
	return rt
}

// BenchmarkRunWarm measures a cached run of a trivial workload: the fixed
// per-run overhead of each backend (cache lookup, instantiation/spawn).
func BenchmarkRunWarm(b *testing.B) {
	for _, backend := range []ferrogo.Backend{ferrogo.BackendWASM, ferrogo.BackendNativeProcess} {
		b.Run(string(backend), func(b *testing.B) {
			rt := benchRuntime(b, backend)
			b.ResetTimer()
			for range b.N {
				if _, err := rt.Run(context.Background(), ferrogo.Code(benchProgram), ferrogo.WithArgs("1")); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRunCompute measures a CPU-bound workload (recursive fib(30)).
func BenchmarkRunCompute(b *testing.B) {
	for _, backend := range []ferrogo.Backend{ferrogo.BackendWASM, ferrogo.BackendNativeProcess} {
		b.Run(string(backend), func(b *testing.B) {
			rt := benchRuntime(b, backend)
			b.ResetTimer()
			for range b.N {
				if _, err := rt.Run(context.Background(), ferrogo.Code(benchProgram), ferrogo.WithArgs("30")); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkBuildCold measures a full compilation (cache disabled).
func BenchmarkBuildCold(b *testing.B) {
	if _, err := exec.LookPath("rustc"); err != nil {
		b.Skip("rustc not installed")
	}
	for _, backend := range []ferrogo.Backend{ferrogo.BackendWASM, ferrogo.BackendNativeProcess} {
		b.Run(string(backend), func(b *testing.B) {
			rt, err := ferrogo.New(ferrogo.WithCache(false), ferrogo.WithBackend(backend))
			if err != nil {
				b.Fatal(err)
			}
			defer rt.Close()
			for range b.N {
				if _, err := rt.Build(context.Background(), ferrogo.Code(benchProgram), ferrogo.ArtifactProgram); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkModuleCall measures per-call overhead of Module.Call: envelope
// encoding, the boundary crossing, and result decoding.
func BenchmarkModuleCall(b *testing.B) {
	if _, err := exec.LookPath("rustc"); err != nil {
		b.Skip("rustc not installed")
	}
	payload := ferrogo.Bytes(make([]byte, 64<<10))
	for _, backend := range []ferrogo.Backend{ferrogo.BackendWASM, ferrogo.BackendNativeLibrary, ferrogo.BackendNativeProcess} {
		if !ferrogo.CapabilitiesOf(backend).Call {
			continue
		}
		rt, err := ferrogo.New(ferrogo.WithCacheDir(sharedCache), ferrogo.WithTrust(ferrogo.Trusted))
		if err != nil {
			b.Fatal(err)
		}
		m, err := rt.Compile(context.Background(), ferrogo.Code(libSource), ferrogo.WithBackend(backend))
		if err != nil {
			b.Fatal(err)
		}
		cases := []struct {
			name string
			fn   string
			args []ferrogo.Value
		}{
			{"add-i32", "add", []ferrogo.Value{ferrogo.Int32(1), ferrogo.Int32(2)}},
			{"greet-string", "greet", []ferrogo.Value{ferrogo.String("benchmark")}},
			{"reverse-64KiB", "reverse", []ferrogo.Value{payload}},
		}
		if backend == ferrogo.BackendWASM {
			cases = append(cases, struct {
				name string
				fn   string
				args []ferrogo.Value
			}{"raw-c-add", "calculate", []ferrogo.Value{ferrogo.Int32(1), ferrogo.Int32(2)}})
		}
		for _, c := range cases {
			b.Run(string(backend)+"/"+c.name, func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					if _, err := m.Call(context.Background(), c.fn, c.args...); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
		m.Close()
		rt.Close()
	}
}

// BenchmarkJSONValue measures the Go-side cost of building and decoding JSON
// values (encoding/json), excluding the boundary crossing.
func BenchmarkJSONValue(b *testing.B) {
	type point struct {
		X, Y float64
		Tags []string
	}
	in := point{X: 1.5, Y: -2, Tags: []string{"a", "b", "c"}}
	b.ReportAllocs()
	for range b.N {
		v, err := ferrogo.JSON(in)
		if err != nil {
			b.Fatal(err)
		}
		var out point
		if err := v.Decode(&out); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkModuleCallParallel measures call throughput under concurrency
// (WASM: instance pool; native library: shared image).
func BenchmarkModuleCallParallel(b *testing.B) {
	if _, err := exec.LookPath("rustc"); err != nil {
		b.Skip("rustc not installed")
	}
	for _, backend := range []ferrogo.Backend{ferrogo.BackendWASM, ferrogo.BackendNativeLibrary, ferrogo.BackendNativeProcess} {
		if !ferrogo.CapabilitiesOf(backend).Call {
			continue
		}
		rt, err := ferrogo.New(ferrogo.WithCacheDir(sharedCache), ferrogo.WithTrust(ferrogo.Trusted))
		if err != nil {
			b.Fatal(err)
		}
		m, err := rt.Compile(context.Background(), ferrogo.Code(libSource), ferrogo.WithBackend(backend))
		if err != nil {
			b.Fatal(err)
		}
		b.Run(string(backend), func(b *testing.B) {
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if _, err := m.Call(context.Background(), "add", ferrogo.Int32(1), ferrogo.Int32(2)); err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
		m.Close()
		rt.Close()
	}
}
