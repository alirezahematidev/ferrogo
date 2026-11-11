//go:build !unix

package cache

// syncDir is a no-op where directories cannot be fsynced (Windows).
func syncDir(string) {}
