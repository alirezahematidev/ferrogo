//go:build !unix && !windows

package process

// processAlive cannot be determined portably here; report the process gone.
func processAlive(int) bool { return false }
