package wasmwazy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/imports/wasi_snapshot_preview1"

	"github.com/alirezahematidev/ferrogo/internal/backend"
	"github.com/alirezahematidev/ferrogo/internal/errdefs"
	"github.com/alirezahematidev/ferrogo/internal/wire"
)

const (
	callPrefix = "ferrogo_v1_call_"
	sigPrefix  = "ferrogo_v1_sig_"
)

// maxStderr bounds the per-instance stderr kept for diagnosing aborts.
const maxStderr = 64 << 10

// Module is a compiled WASM library. Each instance has its own linear
// memory and statics; concurrent calls use separate instances from a pool.
type Module struct {
	rt       wazy.Runtime
	compiled wazy.CompiledModule
	spec     backend.ModuleSpec
	pages    uint32
	ferrogo  bool // exports the FerroGo ABI
	// exports is computed once: wazy rebuilds the map on every call.
	exports map[string]api.FunctionDefinition

	mu     sync.RWMutex // held for reading by calls, for writing by Close
	closed bool
	idle   chan *instance
}

var (
	_ backend.Module    = (*Module)(nil)
	_ backend.RawCaller = (*Module)(nil)
)

type instance struct {
	mod    api.Module
	stderr *boundedBuffer
	// fns caches function handles: wazy allocates a new call engine (with
	// its own stack) per ExportedFunction lookup. An instance is used by one
	// goroutine at a time, so the cache needs no lock.
	fns map[string]api.Function
	// callCtx is the context of the call currently running on the instance,
	// so guest sleeps end when that call is cancelled or times out.
	callCtx atomic.Pointer[context.Context]
	// Reusable guest buffers. Every guest call made with interruption checks
	// costs wazy a watcher goroutine, so keeping these across calls cuts a
	// call from six guest calls to two (the function and the result free).
	// They are released with the instance's memory.
	outSlot uint32
	inBuf   uint32
	inCap   uint32
}

// fn returns a cached handle for an exported function, or nil.
func (inst *instance) fn(name string) api.Function {
	if f, ok := inst.fns[name]; ok {
		return f
	}
	f := inst.mod.ExportedFunction(name)
	inst.fns[name] = f
	return f
}

// sleep implements WASI sleeps bounded by the current call's context.
func (inst *instance) sleep(ns int64) {
	ctx := context.Background()
	if p := inst.callCtx.Load(); p != nil {
		ctx = *p
	}
	t := time.NewTimer(time.Duration(ns))
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// LoadModule compiles WASM code into a module. The module is validated by
// instantiating it once.
func (r *Runner) LoadModule(ctx context.Context, code []byte, spec backend.ModuleSpec) (*Module, error) {
	pages, err := MemoryPages(spec.MemoryLimit)
	if err != nil {
		return nil, err
	}
	if spec.MaxPayload <= 0 {
		spec.MaxPayload = wire.DefaultMaxPayload
	}
	// The runtime outlives this call, so it must not inherit ctx's cancellation.
	rt := wazy.NewRuntimeWithConfig(context.WithoutCancel(ctx), wazy.NewRuntimeConfig().
		WithCompilationCache(r.cache).
		WithMemoryLimitPages(pages))
	m := &Module{rt: rt, spec: spec, pages: pages, idle: make(chan *instance, runtime.GOMAXPROCS(0))}
	fail := func(err error) (*Module, error) {
		_ = rt.Close(context.Background()) // already failing; report the original error
		return nil, err
	}
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		return fail(fmt.Errorf("wasm: instantiating WASI: %w", err))
	}
	if err := instantiateHost(ctx, rt, spec); err != nil {
		return fail(err)
	}
	if m.compiled, err = r.compile(ctx, rt, code, ""); err != nil {
		return fail(err)
	}
	m.exports = m.compiled.ExportedFunctions()
	exports := m.exports
	if _, ok := exports["ferrogo_abi_version"]; ok {
		for _, required := range []string{"ferrogo_alloc", "ferrogo_free", "ferrogo_v1_last_panic"} {
			if _, ok := exports[required]; !ok {
				return fail(fmt.Errorf("%w: module exports ferrogo_abi_version but not %s", errdefs.ErrABIMismatch, required))
			}
		}
		m.ferrogo = true
	}
	inst, err := m.newInstance(ctx)
	if err != nil {
		return fail(err)
	}
	if m.ferrogo {
		res, err := inst.fn("ferrogo_abi_version").Call(ctx)
		if err != nil {
			return fail(fmt.Errorf("wasm: reading ABI version: %w", err))
		}
		if v := uint32(res[0]); v != 1 {
			return fail(fmt.Errorf("%w: module uses FerroGo ABI v%d; this FerroGo supports v1", errdefs.ErrABIMismatch, v))
		}
	}
	m.release(inst)
	return m, nil
}

func (m *Module) newInstance(ctx context.Context) (*instance, error) {
	inst := &instance{stderr: &boundedBuffer{limit: maxStderr}, fns: map[string]api.Function{}}
	fsCfg := wazy.NewFSConfig()
	for _, mt := range m.spec.Mounts {
		if mt.ReadOnly {
			fsCfg = fsCfg.WithReadOnlyDirMount(mt.HostPath, mt.GuestPath)
		} else {
			fsCfg = fsCfg.WithDirMount(mt.HostPath, mt.GuestPath)
		}
	}
	cfg := wazy.NewModuleConfig().
		WithName("").
		WithStartFunctions("_initialize"). // WASI reactor initialisation, if present
		WithStdout(io.Discard).
		WithStderr(inst.stderr).
		WithFSConfig(fsCfg).
		WithNanosleep(inst.sleep)
	cfg = withEntropy(cfg, m.spec.Deterministic, m.spec.Seed)
	for _, kv := range m.spec.Env {
		k, v, _ := strings.Cut(kv, "=")
		cfg = cfg.WithEnv(k, v)
	}
	mod, err := m.rt.InstantiateModule(context.WithoutCancel(ctx), m.compiled, cfg)
	if err != nil {
		return nil, fmt.Errorf("wasm: instantiating module: %w", err)
	}
	inst.mod = mod
	return inst, nil
}

func (m *Module) acquire(ctx context.Context) (*instance, error) {
	select {
	case inst := <-m.idle:
		return inst, nil
	default:
		return m.newInstance(ctx)
	}
}

// release returns a healthy instance to the pool, or closes it if the pool
// is full.
func (m *Module) release(inst *instance) {
	select {
	case m.idle <- inst:
	default:
		discard(inst)
	}
}

func discard(inst *instance) {
	// The instance is being thrown away (often after a trap); a close error
	// carries no information the caller can act on.
	_ = inst.mod.Close(context.Background())
}

// Functions lists FerroGo ABI exports.
func (m *Module) Functions() []string {
	var names []string
	for name := range m.exports {
		if fn, ok := strings.CutPrefix(name, callPrefix); ok {
			names = append(names, fn)
		}
	}
	sort.Strings(names)
	return names
}

// Call invokes an envelope-convention function.
func (m *Module) Call(ctx context.Context, name string, input []byte) (uint32, []byte, error) {
	if !m.ferrogo {
		return 0, nil, fmt.Errorf("%w: %s", errdefs.ErrFunctionNotFound, name)
	}
	if _, ok := m.exports[callPrefix+name]; !ok {
		return 0, nil, fmt.Errorf("%w: %s", errdefs.ErrFunctionNotFound, name)
	}
	return m.invoke(ctx, callPrefix+name, input, true)
}

// Signature returns the Rust signature of a FerroGo ABI export.
func (m *Module) Signature(ctx context.Context, name string) (string, error) {
	if _, ok := m.exports[sigPrefix+name]; !ok {
		return "", fmt.Errorf("%w: %s", errdefs.ErrFunctionNotFound, name)
	}
	_, out, err := m.invoke(ctx, sigPrefix+name, nil, false)
	return string(out), err
}

// invoke runs one ABI call on a pooled instance. withInput selects the
// (in, len, out) calling convention; otherwise the function takes (out).
func (m *Module) invoke(ctx context.Context, export string, input []byte, withInput bool) (uint32, []byte, error) {
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
	inst, err := m.acquire(ctx)
	if err != nil {
		return 0, nil, err
	}
	inst.stderr.Reset()
	inst.callCtx.Store(&callCtx)
	status, out, healthy, err := m.call(callCtx, inst, export, input, withInput)
	inst.callCtx.Store(nil)
	if healthy && !endedBefore(callCtx, time.Now()) {
		m.release(inst)
		return status, out, err
	}
	if healthy {
		// The call finished only after its deadline (for example after a
		// sleep cut short by the deadline): its result cannot be trusted.
		healthy = false
	}
	// The instance trapped or was interrupted: never reuse it.
	defer discard(inst)
	if callCtx.Err() != nil {
		if ctx.Err() != nil {
			return 0, nil, ctx.Err()
		}
		return 0, nil, fmt.Errorf("%w: call exceeded %s", errdefs.ErrTimeout, m.spec.CallTimeout)
	}
	if panicMsg := lastPanic(ctx, inst); panicMsg != "" {
		return wire.StatusPanic, []byte(panicMsg), nil
	}
	if strings.Contains(inst.stderr.String(), "memory allocation of ") {
		return 0, nil, fmt.Errorf("%w: guest memory is limited to %d bytes", errdefs.ErrMemoryLimit, int64(m.pages)*pageSize)
	}
	return 0, nil, err
}

// call performs the guest calls. healthy is false if the instance must be
// discarded.
func (m *Module) call(ctx context.Context, inst *instance, export string, input []byte, withInput bool) (status uint32, out []byte, healthy bool, err error) {
	mem := inst.mod.Memory()
	alloc := inst.fn("ferrogo_alloc")
	free := inst.fn("ferrogo_free")
	fn := inst.fn(export)

	// Allocator calls are short and bounded: run them without a cancellable
	// context, because wazy starts a watcher goroutine per call that has
	// one. Only the user function needs to be interruptible.
	quick := context.WithoutCancel(ctx)
	allocate := func(n int) (uint32, error) {
		res, err := alloc.Call(quick, uint64(n))
		if err != nil {
			return 0, err
		}
		if res[0] == 0 {
			return 0, fmt.Errorf("%w: cannot allocate %d bytes in the guest", errdefs.ErrMemoryLimit, n)
		}
		return uint32(res[0]), nil
	}
	if inst.outSlot == 0 {
		if inst.outSlot, err = allocate(8); err != nil {
			return 0, nil, false, err
		}
	}
	outSlot := inst.outSlot
	args := []uint64{uint64(outSlot)}
	if withInput {
		if uint32(len(input)) > inst.inCap || inst.inBuf == 0 {
			if inst.inBuf != 0 {
				if _, err := free.Call(quick, uint64(inst.inBuf), uint64(inst.inCap)); err != nil {
					return 0, nil, false, fmt.Errorf("wasm: freeing guest memory: %w", err)
				}
				inst.inBuf, inst.inCap = 0, 0
			}
			size := max(len(input), 256)
			if inst.inBuf, err = allocate(size); err != nil {
				return 0, nil, false, err
			}
			inst.inCap = uint32(size)
		}
		if !mem.Write(inst.inBuf, input) {
			return 0, nil, false, errors.New("wasm: input buffer out of range")
		}
		args = []uint64{uint64(inst.inBuf), uint64(len(input)), uint64(outSlot)}
	}
	res, err := fn.Call(ctx, args...)
	if err != nil {
		return 0, nil, false, fmt.Errorf("wasm: trap in %s: %s", export, trapReason(err))
	}
	ptr, ok1 := mem.ReadUint32Le(outSlot)
	n, ok2 := mem.ReadUint32Le(outSlot + 4)
	if !ok1 || !ok2 {
		return 0, nil, false, errors.New("wasm: result descriptor out of range")
	}
	if int64(n) > int64(m.spec.MaxPayload) {
		return 0, nil, false, fmt.Errorf("%w: result is %d bytes; the limit is %d", errdefs.ErrABIMismatch, n, m.spec.MaxPayload)
	}
	view, ok := mem.Read(ptr, n)
	if !ok {
		return 0, nil, false, errors.New("wasm: result buffer out of range")
	}
	out = bytes.Clone(view) // the guest frees its copy below
	if n > 0 {
		if _, err := free.Call(quick, uint64(ptr), uint64(n)); err != nil {
			return 0, nil, false, fmt.Errorf("wasm: freeing guest memory: %w", err)
		}
	}
	return uint32(res[0]), out, true, nil
}

// lastPanic retrieves the panic recorded by the guest's panic hook. After a
// trap the instance's memory is intact, so this read is safe; the instance
// is discarded afterwards.
func lastPanic(ctx context.Context, inst *instance) string {
	fn := inst.fn("ferrogo_v1_last_panic")
	alloc := inst.fn("ferrogo_alloc")
	if fn == nil || alloc == nil || inst.mod.IsClosed() {
		return ""
	}
	ctx = context.WithoutCancel(ctx)
	res, err := alloc.Call(ctx, 8)
	if err != nil || res[0] == 0 {
		return ""
	}
	slot := uint32(res[0])
	if _, err := fn.Call(ctx, uint64(slot)); err != nil {
		return ""
	}
	mem := inst.mod.Memory()
	ptr, _ := mem.ReadUint32Le(slot)
	n, _ := mem.ReadUint32Le(slot + 4)
	view, ok := mem.Read(ptr, min(n, maxStderr))
	if !ok {
		return ""
	}
	return string(view)
}

// RawSignature reports the core types of a plain exported function.
func (m *Module) RawSignature(name string) (params, results []backend.RawType, ok bool) {
	def, ok := m.exports[name]
	if !ok || strings.HasPrefix(name, "ferrogo_") {
		return nil, nil, false
	}
	for _, t := range def.ParamTypes() {
		params = append(params, backend.RawType(t))
	}
	for _, t := range def.ResultTypes() {
		results = append(results, backend.RawType(t))
	}
	return params, results, true
}

// CallRaw calls a plain exported function with core values.
func (m *Module) CallRaw(ctx context.Context, name string, args []uint64) ([]uint64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, errdefs.ErrClosed
	}
	callCtx := ctx
	if m.spec.CallTimeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, m.spec.CallTimeout)
		defer cancel()
	}
	inst, err := m.acquire(ctx)
	if err != nil {
		return nil, err
	}
	inst.callCtx.Store(&callCtx)
	res, err := inst.fn(name).Call(callCtx, args...)
	inst.callCtx.Store(nil)
	if err == nil && endedBefore(callCtx, time.Now()) {
		err = context.DeadlineExceeded // finished only after its deadline: discard
	}
	if err == nil {
		m.release(inst)
		return res, nil
	}
	discard(inst)
	if callCtx.Err() != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: call exceeded %s", errdefs.ErrTimeout, m.spec.CallTimeout)
	}
	return nil, fmt.Errorf("wasm: trap in %s: %s", name, trapReason(err))
}

// Close waits for in-flight calls and releases every instance and the runtime.
func (m *Module) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	close(m.idle)
	for inst := range m.idle {
		discard(inst)
	}
	return m.rt.Close(context.Background())
}

// boundedBuffer keeps the first limit bytes written to it.
type boundedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.limit - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *boundedBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// instantiateHost provides the "ferrogo" import module used by
// ferrogo::import! stubs: ferrogo_host_call(name, name_len, in, in_len, out).
func instantiateHost(ctx context.Context, rt wazy.Runtime, spec backend.ModuleSpec) error {
	i32 := api.ValueTypeI32
	_, err := rt.NewHostModuleBuilder("ferrogo").
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
			stack[0] = uint64(hostCall(ctx, mod, spec,
				uint32(stack[0]), uint32(stack[1]), uint32(stack[2]), uint32(stack[3]), uint32(stack[4])))
		}), []api.ValueType{i32, i32, i32, i32, i32}, []api.ValueType{i32}).
		Export("ferrogo_host_call").
		Instantiate(ctx)
	if err != nil {
		return fmt.Errorf("wasm: instantiating host functions: %w", err)
	}
	return nil
}

// hostCall runs one host-function call and writes an owned result buffer
// (allocated with the guest's ferrogo_alloc) to the guest's out-slot.
func hostCall(ctx context.Context, mod api.Module, spec backend.ModuleSpec, namePtr, nameLen, inPtr, inLen, outPtr uint32) (status uint32) {
	mem := mod.Memory()
	result, err := func() (out []byte, err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("host function panicked: %v", p)
			}
		}()
		if spec.Host == nil {
			return nil, errors.New("no host functions are registered (use ferrogo.WithHostFunction)")
		}
		if inLen > uint32(spec.MaxPayload) || nameLen > 256 {
			return nil, errors.New("host call arguments exceed the payload limit")
		}
		name, ok1 := mem.Read(namePtr, nameLen)
		input, ok2 := mem.Read(inPtr, inLen)
		if !ok1 || !ok2 {
			return nil, errors.New("host call arguments out of range")
		}
		return spec.Host(ctx, string(name), bytes.Clone(input))
	}()
	status = wire.StatusOK
	if err != nil {
		status, result = wire.StatusError, []byte(err.Error())
	}
	var ptr uint32
	if len(result) > 0 {
		res, callErr := mod.ExportedFunction("ferrogo_alloc").Call(ctx, uint64(len(result)))
		if callErr != nil || res[0] == 0 || !mem.Write(uint32(res[0]), result) {
			return wire.StatusError // no room in the guest for the result
		}
		ptr = uint32(res[0])
	}
	if !mem.WriteUint32Le(outPtr, ptr) || !mem.WriteUint32Le(outPtr+4, uint32(len(result))) {
		return wire.StatusError
	}
	return status
}
