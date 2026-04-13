package wyze

import (
	"io"
	"os"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/h264"
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
		frame = trimInvalidLeadingNALUs(frame)
		if len(frame) < 5 {
			continue
		}
		w.seq++
		w.queue = append(w.queue, gwellFrame{payload: frame, timestamp: w.seq * 3000})
	}
	w.cond.Broadcast()
	return len(p), nil
}

func trimInvalidLeadingNALUs(b []byte) []byte {
	for len(b) >= 5 {
		if !(len(b) >= 4 && b[0] == 0 && b[1] == 0 && (b[2] == 1 || (b[2] == 0 && b[3] == 1))) {
			return b
		}
		start := 4
		if b[2] == 1 {
			start = 3
		}
		if start >= len(b) {
			return nil
		}
		naluType := b[start] & 0x1F
		if naluType != 0 {
			return b
		}
		next := annexb.IndexFrame(append([]byte{0, 0, 0, 1, 0x09, 0xF0}, b...))
		if next <= 6 || next-6 >= len(b) {
			return nil
		}
		b = b[next-6:]
		if len(b) >= 5 && h264.NALUType(annexb.EncodeToAVCC(b)) != 0 {
			return b
		}
	}
	return b
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
