package wyze

import (
	"io"
	"os"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/h264/annexb"
)

type gwellFrame struct {
	payload   []byte
	timestamp uint32
}

type gwellAnnexBWriter struct {
	mu       sync.Mutex
	cond     *sync.Cond
	buf      []byte
	queue    []gwellFrame
	closed   bool
	deadline time.Time
	seq      uint32
}

func newGWellAnnexBWriter() *gwellAnnexBWriter {
	w := &gwellAnnexBWriter{}
	w.cond = sync.NewCond(&w.mu)
	return w
}

func (w *gwellAnnexBWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, io.ErrClosedPipe
	}

	w.buf = append(w.buf, p...)
	for {
		i := annexb.IndexFrame(w.buf)
		if i < 0 {
			break
		}
		frame := append([]byte(nil), w.buf[:i]...)
		w.buf = append(w.buf[:0], w.buf[i:]...)
		if len(frame) == 0 {
			continue
		}
		w.seq++
		w.queue = append(w.queue, gwellFrame{payload: frame, timestamp: w.seq * 3000})
	}
	w.cond.Broadcast()
	return len(p), nil
}

func (w *gwellAnnexBWriter) ReadFrame() ([]byte, uint32, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for len(w.queue) == 0 {
		if w.closed {
			return nil, 0, io.EOF
		}
		if !w.deadline.IsZero() && time.Now().After(w.deadline) {
			return nil, 0, osErrDeadlineExceeded()
		}
		if w.deadline.IsZero() {
			w.cond.Wait()
			continue
		}
		wait := time.Until(w.deadline)
		if wait <= 0 {
			return nil, 0, osErrDeadlineExceeded()
		}
		timedOut := false
		timer := time.AfterFunc(wait, func() {
			w.mu.Lock()
			timedOut = true
			w.cond.Broadcast()
			w.mu.Unlock()
		})
		w.cond.Wait()
		timer.Stop()
		if timedOut && len(w.queue) == 0 {
			return nil, 0, osErrDeadlineExceeded()
		}
	}

	frame := w.queue[0]
	w.queue = w.queue[1:]
	return frame.payload, frame.timestamp, nil
}

func (w *gwellAnnexBWriter) SetDeadline(t time.Time) error {
	w.mu.Lock()
	w.deadline = t
	w.cond.Broadcast()
	w.mu.Unlock()
	return nil
}

func (w *gwellAnnexBWriter) Close() error {
	w.mu.Lock()
	w.closed = true
	w.cond.Broadcast()
	w.mu.Unlock()
	return nil
}

func osErrDeadlineExceeded() error {
	return os.ErrDeadlineExceeded
}
