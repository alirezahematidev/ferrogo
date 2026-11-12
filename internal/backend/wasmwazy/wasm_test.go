package wasmwazy

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestMemoryPages(t *testing.T) {
	cases := []struct {
		limit int64
		want  uint32
		ok    bool
	}{
		{0, DefaultMemoryLimit / pageSize, true},
		{64 << 10, 1, true},
		{100 << 10, 1, true}, // rounds down
		{8 << 30, maxPages, true},
		{1000, 0, false},
	}
	for _, c := range cases {
		got, err := MemoryPages(c.limit)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("MemoryPages(%d) = %d, %v; want %d ok=%v", c.limit, got, err, c.want, c.ok)
		}
	}
}

func TestTrapReason(t *testing.T) {
	err := errors.New("module[] function[_start] failed: wasm error: unreachable\nwasm stack trace:\n\t.$1()")
	if got := trapReason(err); got != "unreachable" {
		t.Errorf("trapReason = %q", got)
	}
	if got := trapReason(errors.New("something else")); got != "something else" {
		t.Errorf("trapReason = %q", got)
	}
}

// blockingReader never returns until released.
type blockingReader struct{ release chan struct{} }

func (b *blockingReader) Read(p []byte) (int, error) {
	<-b.release
	return 0, io.EOF
}

func TestContextReaderUnblocksOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	br := &blockingReader{release: make(chan struct{})}
	defer close(br.release)
	r := &contextReader{ctx: ctx, r: br}
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	if _, err := r.Read(make([]byte, 8)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("read was not interrupted")
	}
}

func TestContextReaderPassesDataThrough(t *testing.T) {
	r := &contextReader{ctx: context.Background(), r: strings.NewReader("hello, world")}
	// Read with a buffer smaller than the first chunk to exercise the remainder path.
	var got []byte
	small := make([]byte, 5)
	for {
		n, err := r.Read(small)
		got = append(got, small[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if string(got) != "hello, world" {
		t.Errorf("got %q", got)
	}
}

func TestContextSleep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sleep := contextSleep(ctx)
	start := time.Now()
	sleep(int64(10 * time.Millisecond))
	if time.Since(start) < 10*time.Millisecond {
		t.Error("sleep returned early")
	}
	cancel()
	start = time.Now()
	sleep(int64(time.Hour))
	if time.Since(start) > time.Second {
		t.Error("sleep ignored cancellation")
	}
}
