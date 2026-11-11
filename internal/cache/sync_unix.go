//go:build unix

package cache

import "os"

// syncDir flushes a directory entry so a committed rename survives a crash.
// Failure only weakens durability (the entry may need rebuilding after a
// crash), never correctness, so it is not reported.
func syncDir(dir string) {
	f, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = f.Sync()
	_ = f.Close()
}
