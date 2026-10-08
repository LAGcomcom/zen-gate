package lane

import (
	"bytes"
	"context"
	"io"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// Each newStreamReader used to leave one goroutine parked on ctx.Done(). The
// probe path calls through with context.Background(), whose Done() is a nil
// channel — so every probe round added a goroutine that could never exit, and
// it kept the whole streamReader alive with it, including the 512-chunk buffer.
// The pump must also stop when the reader is closed while its channel is full,
// or the connection it owns is never released.
func TestStreamReaderReleasesItsGoroutines(t *testing.T) {
	cases := []struct {
		name string
		src  func() io.ReadCloser
		// armed is the state the leak needs: a running watcher, or a pump
		// actually parked on a full chunk buffer.
		armed func(s *streamReader, before int) bool
	}{
		{
			name: "watcher parks on a nil Done channel",
			src: func() io.ReadCloser {
				r, _ := io.Pipe()
				return r
			},
			armed: func(s *streamReader, before int) bool {
				return readerGoroutines() >= before+1
			},
		},
		{
			name:  "pump blocks once the chunk buffer is full",
			src:   func() io.ReadCloser { return &endlessReader{} },
			armed: func(s *streamReader, before int) bool { return len(s.ch) == cap(s.ch) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := readerGoroutines()
			s := newStreamReader(context.Background(), tc.src(), 20*time.Millisecond)
			if !waitFor(func() bool { return tc.armed(s, before) }) {
				t.Fatal("the stream never reached the state under test")
			}
			s.Close()
			if !waitFor(func() bool { return readerGoroutines() <= before }) {
				t.Errorf("%d stream goroutines still running after Close, want them gone", readerGoroutines()-before)
			}
		})
	}
}

func readerGoroutines() int {
	size := 1 << 20
	for {
		buf := make([]byte, size)
		n := runtime.Stack(buf, true)
		if n < size {
			return bytes.Count(buf[:n], []byte("lane.newStreamReader.func"))
		}
		size *= 2
	}
}

func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// endlessReader answers every Read with a full buffer, like an upstream that
// keeps streaming while nobody drains the pump.
type endlessReader struct {
	closed atomic.Bool
}

func (e *endlessReader) Read(p []byte) (int, error) {
	if e.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	copy(p, bytes.Repeat([]byte("x"), len(p)))
	return len(p), nil
}

func (e *endlessReader) Close() error {
	e.closed.Store(true)
	return nil
}
