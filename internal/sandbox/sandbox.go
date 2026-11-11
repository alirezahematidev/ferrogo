// Package sandbox runs processes under the operating system's sandbox:
// Seatbelt (sandbox-exec) on macOS and bubblewrap on Linux. It is used for
// native-process execution and, optionally, for the compiler itself.
//
// Every policy denies the network (unless allowed), denies writes outside
// the Writable paths, and hides file contents inside the user's home
// directory except the Readable paths. Paths outside the home directory
// stay readable (system libraries, toolchains in /usr, ...).
package sandbox

// Policy is a sandbox policy.
type Policy struct {
	AllowNetwork bool
	// Writable paths (directories or files) the process may write.
	Writable []string
	// Readable paths inside the home directory that stay readable.
	Readable []string
	// Dir is the working directory (bubblewrap needs it explicitly).
	Dir string
}
