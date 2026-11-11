// Command wasm shows the WebAssembly sandbox: untrusted code gets no
// filesystem, network, or host environment unless granted, and is stopped by
// memory, time, and output limits.
//
//	go run ./examples/wasm
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/alirezahematidev/ferrogo"
)

func main() {
	ctx := context.Background()
	// No WithTrust: the default policy only allows the sandboxed backend.
	rt, err := ferrogo.New(
		ferrogo.WithTimeout(5*time.Second),
		ferrogo.WithWASMMemoryLimit(64<<20),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer rt.Close()

	// 1. A read-only data directory is the only filesystem the guest sees.
	data, err := os.MkdirTemp("", "ferrogo-example-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(data)
	if err := os.WriteFile(filepath.Join(data, "numbers.txt"), []byte("4 8 15 16 23 42"), 0o600); err != nil {
		log.Fatal(err)
	}
	res, err := rt.Run(ctx, ferrogo.Code(`
fn main() {
    let text = std::fs::read_to_string("/data/numbers.txt").expect("read input");
    let sum: u32 = text.split_whitespace().map(|n| n.parse::<u32>().unwrap()).sum();
    println!("sum = {sum}");
    println!("can read /etc/passwd: {}", std::fs::read_to_string("/etc/passwd").is_ok());
    println!("can write /data: {}", std::fs::write("/data/x", "x").is_ok());
    println!("can open sockets: {}", std::net::TcpStream::connect("1.1.1.1:443").is_ok());
    println!("sees $HOME: {}", std::env::var("HOME").is_ok());
}
`), ferrogo.WithMount(ferrogo.Mount{HostPath: data, GuestPath: "/data", ReadOnly: true}))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(res.Stdout)

	// 2. Memory bombs hit the guest memory limit, not the host.
	_, err = rt.Run(ctx, ferrogo.Code(`fn main() { let mut v = Vec::new(); loop { v.push(vec![0u8; 1 << 20]); } }`))
	fmt.Println("memory bomb stopped:", errors.Is(err, ferrogo.ErrMemoryLimit))

	// 3. Infinite loops are interrupted at the deadline.
	start := time.Now()
	_, err = rt.Run(ctx, ferrogo.Code(`fn main() { loop { std::hint::black_box(()); } }`), ferrogo.WithTimeout(500*time.Millisecond))
	fmt.Printf("infinite loop stopped: %v (after %s)\n", errors.Is(err, ferrogo.ErrTimeout), time.Since(start).Round(100*time.Millisecond))

	// 4. Output floods are cut off.
	_, err = rt.Run(ctx, ferrogo.Code(`fn main() { loop { println!("spam"); } }`), ferrogo.WithMaxOutput(64<<10))
	fmt.Println("output flood stopped:", errors.Is(err, ferrogo.ErrOutputLimit))
}
