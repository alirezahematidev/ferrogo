package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alirezahematidev/ferrogo/internal/errdefs"
)

const helperEnv = "FERROGO_PROCESS_TEST_HELPER"

// TestMain lets the test binary act as a controllable child process.
func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		os.Exit(helper(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func helper(mode string, args []string) int {
	switch mode {
	case "echo":
		fmt.Fprint(os.Stdout, strings.Join(args, " "))
		fmt.Fprint(os.Stderr, "to-stderr")
		return 0
	case "env":
		for _, kv := range os.Environ() {
			fmt.Println(kv)
		}
		return 0
	case "cat":
		if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
			return 3
		}
		return 0
	case "exit":
		code, _ := strconv.Atoi(args[0])
		return code
	case "pwd":
		wd, _ := os.Getwd()
		fmt.Print(wd)
		return 0
	case "sleep":
		time.Sleep(time.Hour)
		return 0
	case "spam":
		chunk := bytes.Repeat([]byte("x"), 4096)
		for {
			if _, err := os.Stdout.Write(chunk); err != nil {
				return 4
			}
		}
	case "spawn-child":
		// Start a grandchild that sleeps, report its PID, then sleep too.
		exe, _ := os.Executable()
		child := exec.Command(exe)
		child.Env = []string{helperEnv + "=sleep"}
		if err := child.Start(); err != nil {
			return 5
		}
		fmt.Println(child.Process.Pid)
		time.Sleep(time.Hour)
		return 0
	case "alloc":
		// Touch 512 MiB so a memory limit must stop it.
		var keep [][]byte
		for range 512 {
			b := make([]byte, 1<<20)
			for i := range b {
				b[i] = 1
			}
			keep = append(keep, b)
		}
		fmt.Println(len(keep))
		return 0
	case "ignore-term":
		signal.Ignore(syscall.SIGTERM)
		fmt.Println("ready")
		time.Sleep(time.Hour)
		return 0
	}
	return 99
}

func helperSpec(t *testing.T, mode string, args ...string) Spec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Spec{Path: exe, Args: args, Env: []string{helperEnv + "=" + mode}}
}

func TestRunCapturesOutputAndExitCode(t *testing.T) {
	res, err := Run(context.Background(), helperSpec(t, "echo", "hello", "wörld"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(res.Stdout); got != "hello wörld" {
		t.Errorf("stdout = %q", got)
	}
	if got := string(res.Stderr); got != "to-stderr" {
		t.Errorf("stderr = %q", got)
	}
	if res.ExitCode != 0 || res.Signal != "" {
		t.Errorf("exit = %d signal = %q", res.ExitCode, res.Signal)
	}
}

func TestRunArgumentsAreNotShellInterpreted(t *testing.T) {
	arg := `$(touch /tmp/ferrogo-pwned); "quoted" 'single' | & ; *`
	res, err := Run(context.Background(), helperSpec(t, "echo", arg))
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Stdout) != arg {
		t.Errorf("argument was altered: %q", res.Stdout)
	}
}

func TestRunNonZeroExitIsNotAnError(t *testing.T) {
	res, err := Run(context.Background(), helperSpec(t, "exit", "42"))
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 42 {
		t.Errorf("exit code = %d, want 42", res.ExitCode)
	}
}

func TestRunEnvironmentIsExplicit(t *testing.T) {
	t.Setenv("FERROGO_SECRET_FROM_PARENT", "leak")
	spec := helperSpec(t, "env")
	spec.Env = append(spec.Env, "ALLOWED=yes")
	res, err := Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	out := string(res.Stdout)
	if strings.Contains(out, "FERROGO_SECRET_FROM_PARENT") {
		t.Error("parent environment leaked into child")
	}
	if !strings.Contains(out, "ALLOWED=yes") {
		t.Errorf("explicit env missing: %q", out)
	}
}

func TestRunStdinAndDir(t *testing.T) {
	spec := helperSpec(t, "cat")
	spec.Stdin = strings.NewReader("über input")
	res, err := Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Stdout) != "über input" {
		t.Errorf("stdout = %q", res.Stdout)
	}

	dir := t.TempDir()
	spec = helperSpec(t, "pwd")
	spec.Dir = dir
	res, err = Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := os.Stat(dir)
	got, statErr := os.Stat(string(res.Stdout))
	if statErr != nil || !os.SameFile(want, got) {
		t.Errorf("working dir = %q, want %q", res.Stdout, dir)
	}
}

func TestRunTeeStreams(t *testing.T) {
	var out, errOut bytes.Buffer
	spec := helperSpec(t, "echo", "live")
	spec.Stdout, spec.Stderr = &out, &errOut
	if _, err := Run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if out.String() != "live" || errOut.String() != "to-stderr" {
		t.Errorf("tee: stdout=%q stderr=%q", out.String(), errOut.String())
	}
}

func TestRunTimeout(t *testing.T) {
	spec := helperSpec(t, "sleep")
	spec.Timeout = 200 * time.Millisecond
	start := time.Now()
	res, err := Run(context.Background(), spec)
	if !errors.Is(err, errdefs.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if res == nil {
		t.Fatal("partial result missing")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timeout took %s", elapsed)
	}
}

func TestRunCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	_, err := Run(ctx, helperSpec(t, "sleep"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRunOutputLimit(t *testing.T) {
	spec := helperSpec(t, "spam")
	spec.MaxOutput = 64 << 10
	spec.Timeout = 30 * time.Second
	res, err := Run(context.Background(), spec)
	if !errors.Is(err, errdefs.ErrOutputLimit) {
		t.Fatalf("err = %v, want ErrOutputLimit", err)
	}
	if got := int64(len(res.Stdout) + len(res.Stderr)); got != spec.MaxOutput {
		t.Errorf("captured %d bytes, want exactly %d", got, spec.MaxOutput)
	}
}

func TestRunGraceThenKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM semantics are Unix-only")
	}
	spec := helperSpec(t, "ignore-term")
	spec.Timeout = 300 * time.Millisecond
	spec.GracePeriod = 200 * time.Millisecond
	start := time.Now()
	res, err := Run(context.Background(), spec)
	if !errors.Is(err, errdefs.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if res.Signal != syscall.SIGKILL.String() {
		t.Errorf("signal = %q, want %q (process ignored SIGTERM)", res.Signal, syscall.SIGKILL.String())
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("kill took %s", elapsed)
	}
}

func TestRunStartFailure(t *testing.T) {
	res, err := Run(context.Background(), Spec{Path: "/nonexistent/ferrogo-binary"})
	if err == nil || res != nil {
		t.Fatalf("expected start failure, got res=%v err=%v", res, err)
	}
}

func TestRunKillsWholeProcessTree(t *testing.T) {
	spec := helperSpec(t, "spawn-child")
	spec.Timeout = 1500 * time.Millisecond
	res, err := Run(context.Background(), spec)
	if !errors.Is(err, errdefs.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(res.Stdout)))
	if convErr != nil {
		t.Fatalf("grandchild PID not reported: %q", res.Stdout)
	}
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d survived termination", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRunMemoryLimit(t *testing.T) {
	spec := helperSpec(t, "alloc")
	spec.MemoryLimit = 64 << 20
	spec.Timeout = 30 * time.Second
	res, err := Run(context.Background(), spec)
	if !treeMemoryLimits {
		if err == nil {
			t.Fatal("memory limit accepted on a platform that cannot enforce it")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 {
		t.Errorf("allocation of 512 MiB succeeded under a 64 MiB limit: %q", res.Stdout)
	}
}
