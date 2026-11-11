package outcap

import (
	"bytes"
	"errors"
	"sync"
	"testing"
)

func TestCombinedLimit(t *testing.T) {
	calls := 0
	c := New(10, func() { calls++ })
	var live bytes.Buffer
	out, errs := c.Stream(&live), c.Stream(nil)
	for _, w := range []struct {
		s *Stream
		p string
	}{{out, "hello"}, {errs, "wor"}, {out, "ld!!!"}, {errs, "more"}} {
		if n, err := w.s.Write([]byte(w.p)); n != len(w.p) || err != nil {
			t.Fatalf("Write must always report success: %d %v", n, err)
		}
	}
	if got := string(out.Bytes()) + "|" + string(errs.Bytes()); got != "hellold|wor" {
		t.Errorf("captured %q", got)
	}
	if live.String() != "hellold" {
		t.Errorf("tee received bytes beyond the limit: %q", live.String())
	}
	if calls != 1 || !c.Exceeded() {
		t.Errorf("onExceed calls = %d", calls)
	}
}

func TestUnlimited(t *testing.T) {
	c := New(0, func() { t.Fatal("limit triggered") })
	s := c.Stream(nil)
	data := bytes.Repeat([]byte("x"), 1<<20)
	s.Write(data)
	if len(s.Bytes()) != len(data) {
		t.Errorf("captured %d bytes", len(s.Bytes()))
	}
}

type failingWriter struct{ n int }

func (f *failingWriter) Write(p []byte) (int, error) {
	f.n++
	return 0, errors.New("sink closed")
}

func TestFailingTeeKeepsCapturing(t *testing.T) {
	sink := &failingWriter{}
	s := New(0, nil).Stream(sink)
	s.Write([]byte("a"))
	s.Write([]byte("b"))
	if string(s.Bytes()) != "ab" || sink.n != 1 {
		t.Errorf("captured %q, sink calls %d", s.Bytes(), sink.n)
	}
}

func TestConcurrentWriters(t *testing.T) {
	c := New(1000, func() {})
	a, b := c.Stream(nil), c.Stream(nil)
	var wg sync.WaitGroup
	for _, s := range []*Stream{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				s.Write([]byte("x"))
			}
		}()
	}
	wg.Wait()
	if total := len(a.Bytes()) + len(b.Bytes()); total != 1000 {
		t.Errorf("captured %d bytes, want exactly 1000", total)
	}
}
