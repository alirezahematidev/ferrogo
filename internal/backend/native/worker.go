package native

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/alirezahematidev/ferrogo/internal/backend"
	"github.com/alirezahematidev/ferrogo/internal/backend/dylib"
	"github.com/alirezahematidev/ferrogo/internal/errdefs"
	"github.com/alirezahematidev/ferrogo/internal/process"
	"github.com/alirezahematidev/ferrogo/internal/wire"
)

// Worker protocol operations and statuses (see internal/abi/worker.rs).
const (
	opCall          = 1
	opSignature     = 2
	statusNotFound  = 255
	maxWorkerStderr = 64 << 10
)

// WorkerSpec configures a worker-backed module.
type WorkerSpec struct {
	// Worker is the compiled worker program; Library the cdylib it serves.
	Worker, Library string
	Env             []string
	// CallTimeout bounds each call; an expired call kills its worker.
	CallTimeout time.Duration
	MemoryLimit int64
	Sandbox     *backend.Sandbox
	MaxPayload  int
}

// WorkerModule serves calls from a pool of worker processes. Each worker has
// its own address space (like a WASM instance): Rust statics are per worker,
// and a worker that crashes, panics fatally or times out is replaced.
type WorkerModule struct {
	spec      WorkerSpec
	functions []string

	mu     sync.RWMutex // read: calls in flight; write: Close
	closed bool
	idle   chan *worker
}

var _ backend.Module = (*WorkerModule)(nil)

type worker struct {
	proc *process.Proc
	r    *bufio.Reader
	w    *bufio.Writer
	dir  string
}

// OpenWorker starts the first worker and checks that it serves the library.
func OpenWorker(ctx context.Context, spec WorkerSpec) (*WorkerModule, error) {
	if spec.MaxPayload <= 0 {
		spec.MaxPayload = wire.DefaultMaxPayload
	}
	functions, err := dylib.ExportedFunctions(spec.Library)
	if err != nil {
		return nil, fmt.Errorf("native: reading exports of %s: %w", spec.Library, err)
	}
	m := &WorkerModule{spec: spec, functions: functions, idle: make(chan *worker, runtime.GOMAXPROCS(0))}
	// Handshake: an unknown signature must come back as "not found", which
	// proves the worker loaded the library and speaks the protocol.
	if _, _, err := m.roundTrip(ctx, opSignature, "__ferrogo_handshake", nil); !errors.Is(err, errdefs.ErrFunctionNotFound) {
		m.Close()
		if err == nil {
			err = errors.New("unexpected handshake reply")
		}
		return nil, fmt.Errorf("native: starting worker: %w", err)
	}
	return m, nil
}

func (m *WorkerModule) spawn() (*worker, error) {
	dir, err := os.MkdirTemp("", "ferrogo-worker-")
	if err != nil {
		return nil, fmt.Errorf("native: %w", err)
	}
	path, args, jobMemory, err := command(m.spec.Worker, []string{m.spec.Library}, dir, m.spec.MemoryLimit, m.spec.Sandbox, m.spec.Library)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	proc, err := process.Start(process.Spec{
		Path: path, Args: args, Env: m.spec.Env, Dir: dir, MaxOutput: maxWorkerStderr, MemoryLimit: jobMemory,
	})
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return &worker{proc: proc, r: bufio.NewReader(proc.Stdout), w: bufio.NewWriter(proc.Stdin), dir: dir}, nil
}

// stop ends a worker: closing stdin lets it exit cleanly; the tree is then
// killed regardless.
func (w *worker) stop() {
	_ = w.proc.Stdin.Close() // the worker exits on EOF
	select {
	case <-w.proc.Done():
	case <-time.After(time.Second):
		_ = w.proc.Kill() // still running: force it
		<-w.proc.Done()
	}
	_ = os.RemoveAll(w.dir) // best effort: the library may have left files
}

func (m *WorkerModule) acquire() (*worker, error) {
	select {
	case w := <-m.idle:
		return w, nil
	default:
		return m.spawn()
	}
}

func (m *WorkerModule) release(w *worker) {
	select {
	case m.idle <- w:
	default:
		w.stop()
	}
}

// Functions lists the library's FerroGo ABI functions.
func (m *WorkerModule) Functions() []string { return m.functions }

// Call invokes ferrogo_v1_call_<name> in a worker.
func (m *WorkerModule) Call(ctx context.Context, name string, input []byte) (uint32, []byte, error) {
	return m.roundTrip(ctx, opCall, name, input)
}

// Signature returns the Rust signature of an exported function.
func (m *WorkerModule) Signature(ctx context.Context, name string) (string, error) {
	_, out, err := m.roundTrip(ctx, opSignature, name, nil)
	return string(out), err
}

type reply struct {
	status uint32
	out    []byte
	err    error
}

func (m *WorkerModule) roundTrip(ctx context.Context, op byte, name string, input []byte) (uint32, []byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return 0, nil, errdefs.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	callCtx := ctx
	if m.spec.CallTimeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, m.spec.CallTimeout)
		defer cancel()
	}
	w, err := m.acquire()
	if err != nil {
		return 0, nil, err
	}
	done := make(chan reply, 1)
	go func() { done <- w.exchange(op, name, input, m.spec.MaxPayload) }()
	select {
	case r := <-done:
		if r.err != nil {
			return 0, nil, m.crashed(w, name, r.err)
		}
		m.release(w)
		if r.status == statusNotFound {
			return 0, nil, fmt.Errorf("%w: %s", errdefs.ErrFunctionNotFound, name)
		}
		return r.status, r.out, nil
	case <-callCtx.Done():
		// A running native call cannot be interrupted: kill its worker.
		_ = w.proc.Kill()
		<-done
		w.stop()
		if ctx.Err() != nil {
			return 0, nil, ctx.Err()
		}
		return 0, nil, fmt.Errorf("%w: call exceeded %s", errdefs.ErrTimeout, m.spec.CallTimeout)
	}
}

// exchange sends one request and reads the reply.
func (w *worker) exchange(op byte, name string, input []byte, maxPayload int) reply {
	var hdr [4]byte
	_ = w.w.WriteByte(op)
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(name)))
	_, _ = w.w.Write(hdr[:])
	_, _ = w.w.WriteString(name)
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(input)))
	_, _ = w.w.Write(hdr[:])
	_, _ = w.w.Write(input)
	if err := w.w.Flush(); err != nil { // buffered write errors surface here
		return reply{err: err}
	}
	var resp [8]byte
	if _, err := io.ReadFull(w.r, resp[:]); err != nil {
		return reply{err: err}
	}
	status := binary.LittleEndian.Uint32(resp[:4])
	n := binary.LittleEndian.Uint32(resp[4:])
	if int64(n) > int64(maxPayload) {
		return reply{err: fmt.Errorf("%w: result is %d bytes; the limit is %d", errdefs.ErrABIMismatch, n, maxPayload)}
	}
	out := make([]byte, n)
	if _, err := io.ReadFull(w.r, out); err != nil {
		return reply{err: err}
	}
	return reply{status: status, out: out}
}

// crashed reports a worker that died mid-call and discards it.
func (m *WorkerModule) crashed(w *worker, name string, cause error) error {
	_ = w.proc.Kill()
	select {
	case <-w.proc.Done():
	case <-time.After(2 * time.Second):
	}
	stderr := strings.TrimSpace(string(w.proc.Stderr()))
	defer w.stop()
	if m.spec.MemoryLimit > 0 && memoryExhausted([]byte(stderr)) {
		return fmt.Errorf("%w: %s: the worker is limited to %d bytes", errdefs.ErrMemoryLimit, name, m.spec.MemoryLimit)
	}
	if len(stderr) > 2000 {
		stderr = "…" + stderr[len(stderr)-2000:]
	}
	if errors.Is(cause, errdefs.ErrABIMismatch) {
		return cause
	}
	return fmt.Errorf("native: worker for %s crashed (%v): %s", name, cause, stderr)
}

// Close stops every worker. In-flight calls finish first.
func (m *WorkerModule) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	close(m.idle)
	for w := range m.idle {
		w.stop()
	}
	return nil
}
