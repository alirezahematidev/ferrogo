// Package outcap captures program output under a combined byte limit,
// optionally forwarding it live to another writer.
package outcap

import (
	"bytes"
	"io"
	"sync"
)

// Capture enforces one byte limit across several output streams.
type Capture struct {
	mu       sync.Mutex
	limit    int64
	total    int64
	exceeded bool
	onExceed func()
}

// New returns a Capture allowing limit bytes in total (0 = unlimited).
// onExceed is called once, outside any lock, when the limit is first exceeded.
func New(limit int64, onExceed func()) *Capture {
	return &Capture{limit: limit, onExceed: onExceed}
}

// Exceeded reports whether the limit was exceeded.
func (c *Capture) Exceeded() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exceeded
}

// Stream returns a writer for one output stream. tee, if non-nil, receives
// a live copy of the bytes that fit within the limit.
func (c *Capture) Stream(tee io.Writer) *Stream {
	return &Stream{c: c, tee: tee}
}

// Stream captures one output stream.
type Stream struct {
	c      *Capture
	buf    bytes.Buffer
	tee    io.Writer
	teeErr error
}

// Write captures p up to the remaining limit. It always reports success so
// that producers keep draining their output until they are stopped.
func (s *Stream) Write(p []byte) (int, error) {
	c := s.c
	c.mu.Lock()
	allowed := int64(len(p))
	trigger := false
	if c.limit > 0 && c.total+allowed > c.limit {
		allowed = max(0, c.limit-c.total)
		if !c.exceeded {
			c.exceeded = true
			trigger = true
		}
	}
	s.buf.Write(p[:allowed])
	c.total += allowed
	tee := s.tee
	if s.teeErr != nil {
		tee = nil
	}
	c.mu.Unlock()

	if tee != nil && allowed > 0 {
		if _, err := tee.Write(p[:allowed]); err != nil {
			// A failing live sink must not lose captured output: stop
			// forwarding and keep capturing.
			c.mu.Lock()
			s.teeErr = err
			c.mu.Unlock()
		}
	}
	if trigger && c.onExceed != nil {
		c.onExceed()
	}
	return len(p), nil
}

// Bytes returns the captured bytes. Call it only after all writers are done.
func (s *Stream) Bytes() []byte { return s.buf.Bytes() }
