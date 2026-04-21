package wyze

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/aac"
)

var amrNBFrameSizes = [16]int{13, 14, 16, 18, 20, 21, 27, 32, 6, 0, 0, 0, 0, 0, 0, 1}

func (c *GWellClient) PlayDuoTalkAMRFile(path string) error {
	frames, err := readAMRNBFile(path)
	if err != nil {
		return err
	}
	if len(frames) == 0 {
		return fmt.Errorf("wyze/gwell: no AMR frames found in %s", path)
	}
	if err = c.SendDefaultDuoTalkHeader(); err != nil {
		return err
	}
	time.Sleep(40 * time.Millisecond)

	if err = c.SetDuoTalkEnabled(true); err != nil {
		return err
	}
	defer func() { _ = c.SetDuoTalkEnabled(false) }()
	time.Sleep(100 * time.Millisecond)

	if err = c.SendDefaultDuoTalkHeader(); err != nil {
		return err
	}
	time.Sleep(40 * time.Millisecond)

	const (
		frameStep = 120 * time.Millisecond
	)
	startPTS := time.Now()
	for i := 0; i < len(frames); i += 6 {
		batch := make([][]byte, 0, 3)
		for j := i; j < len(frames) && j < i+6; j += 2 {
			entry := frames[j : j+1]
			if j+1 < len(frames) {
				merged := append(append([]byte(nil), frames[j]...), frames[j+1]...)
				entry = [][]byte{merged}
			}
			batch = append(batch, entry[0])
		}
		audioPTS := uint64(time.Since(startPTS).Milliseconds())
		if err = c.SendDuoTalkAudioFramesSplit(batch, audioPTS); err != nil {
			return fmt.Errorf("wyze/gwell: send AMR batch starting at frame %d: %w", i, err)
		}
		if i+6 < len(frames) {
			time.Sleep(frameStep)
		}
	}

	return nil
}

func (p *GWellProducer) PlayDuoTalkAMRFile(path string) error {
	if p == nil || p.client == nil {
		return fmt.Errorf("wyze/gwell: client is nil")
	}
	return p.client.PlayDuoTalkAMRFile(path)
}

func (p *GWellProducer) PlayDuoTalkFile(path string, bitrate int) error {
	if p == nil || p.client == nil {
		return fmt.Errorf("wyze/gwell: client is nil")
	}
	return p.client.PlayDuoTalkFile(path, bitrate)
}

func (c *GWellClient) PlayDuoTalkFile(path string, bitrate int) error {
	if filepath.Ext(path) == ".amr" && bitrate <= 0 {
		return c.PlayDuoTalkAMRFile(path)
	}
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-i", path,
		"-vn",
		"-c:a", "aac",
		"-profile:a", "aac_low",
		"-ar", "16000",
		"-ac", "1",
	}
	if bitrate > 0 {
		args = append(args, "-b:a", fmt.Sprintf("%d", bitrate))
	}
	args = append(args, "-write_mpeg2", "1", "-f", "adts", "pipe:1")
	cmd := exec.Command("/opt/homebrew/bin/ffmpeg", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		return err
	}
	playErr := c.PlayDuoTalkAAC(stdout)
	waitErr := cmd.Wait()
	if playErr != nil {
		return playErr
	}
	if waitErr != nil {
		return waitErr
	}
	return nil
}

func streamADTSAAC(rd io.Reader, fn func(frame []byte) error) error {
	for off := 0; ; off++ {
		header := make([]byte, aac.ADTSHeaderSize)
		_, err := io.ReadFull(rd, header)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("wyze/gwell: read ADTS header %d: %w", off, err)
		}
		if !aac.IsADTS(header) {
			return fmt.Errorf("wyze/gwell: unsupported ADTS stream header at frame %d", off)
		}

		headerLen := aac.ADTSHeaderLen(header)
		frameLen := int(aac.ReadADTSSize(header))
		if frameLen < headerLen {
			return fmt.Errorf("wyze/gwell: invalid ADTS frame length %d at frame %d", frameLen, off)
		}

		frame := make([]byte, frameLen)
		copy(frame, header)
		if headerLen > len(header) {
			if _, err = io.ReadFull(rd, frame[len(header):headerLen]); err != nil {
				return fmt.Errorf("wyze/gwell: read ADTS extended header %d: %w", off, err)
			}
		}
		if _, err = io.ReadFull(rd, frame[headerLen:]); err != nil {
			return fmt.Errorf("wyze/gwell: read ADTS frame %d: %w", off, err)
		}
		if err = fn(frame); err != nil {
			return err
		}
	}
}

func (c *GWellClient) PlayDuoTalkAAC(rd io.Reader) error {
	frames := make(chan []byte, 24)
	errCh := make(chan error, 1)

	go func() {
		defer close(frames)
		if err := streamADTSAAC(rd, func(frame []byte) error {
			frames <- frame
			return nil
		}); err != nil {
			errCh <- err
		}
	}()

	firstFrame, ok := <-frames
	if !ok {
		select {
		case err := <-errCh:
			return err
		default:
		}
		return fmt.Errorf("wyze/gwell: no AAC frames produced")
	}

	if err := c.SendObservedDuoAACHeader(); err != nil {
		return err
	}
	time.Sleep(40 * time.Millisecond)

	if err := c.SetDuoTalkEnabled(true); err != nil {
		return err
	}
	defer func() { _ = c.SetDuoTalkEnabled(false) }()
	time.Sleep(100 * time.Millisecond)

	if err := c.SendObservedDuoAACHeader(); err != nil {
		return err
	}
	time.Sleep(40 * time.Millisecond)

	const sampleRate = 16000
	frameDur := time.Duration(aac.AUTime) * time.Second / sampleRate
	startPTS := time.Now()
	frameCount := 0
	sendFrame := func(frame []byte) error {
		audioPTS := uint64(time.Since(startPTS).Microseconds())
		if err := c.SendObservedDuoAACFrame(frame, audioPTS); err != nil {
			return err
		}
		frameCount++
		if frameCount <= 3 || frameCount%50 == 0 {
			fmt.Printf("[wyze/gwell] AAC frame: count=%d len=%d pts=%d\n", frameCount, len(frame), audioPTS)
		}
		time.Sleep(frameDur)
		return nil
	}

	if err := sendFrame(firstFrame); err != nil {
		return err
	}
	for frame := range frames {
		if err := sendFrame(frame); err != nil {
			return err
		}
	}

	select {
	case err := <-errCh:
		return err
	default:
	}

	fmt.Printf("[wyze/gwell] backchannel sent %d AAC frames\n", frameCount)
	return nil
}

func readAMRNBFile(path string) ([][]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("wyze/gwell: read AMR file: %w", err)
	}
	return parseAMRNBBytes(data, path)
}

func readAMRNBStream(rd io.Reader) ([][]byte, error) {
	data, err := io.ReadAll(rd)
	if err != nil {
		return nil, fmt.Errorf("wyze/gwell: read AMR stream: %w", err)
	}
	return parseAMRNBBytes(data, "stream")
}

func streamAMRNB(rd io.Reader, fn func(frame []byte) error) error {
	const amrMagic = "#!AMR\n"
	header := make([]byte, len(amrMagic))
	if _, err := io.ReadFull(rd, header); err != nil {
		return fmt.Errorf("wyze/gwell: read AMR header: %w", err)
	}
	if !bytes.Equal(header, []byte(amrMagic)) {
		return fmt.Errorf("wyze/gwell: unsupported AMR stream header")
	}

	for off := 0; ; off++ {
		frameHeader := make([]byte, 1)
		_, err := io.ReadFull(rd, frameHeader)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("wyze/gwell: read AMR frame header %d: %w", off, err)
		}

		ft := (frameHeader[0] >> 3) & 0x0F
		size := amrNBFrameSizes[ft]
		if size == 0 {
			return fmt.Errorf("wyze/gwell: unsupported AMR-NB frame type %d at frame %d", ft, off)
		}
		frame := make([]byte, size)
		frame[0] = frameHeader[0]
		if size > 1 {
			if _, err = io.ReadFull(rd, frame[1:]); err != nil {
				return fmt.Errorf("wyze/gwell: read AMR frame %d: %w", off, err)
			}
		}
		if err = fn(frame); err != nil {
			return err
		}
	}
}

func parseAMRNBBytes(data []byte, label string) ([][]byte, error) {
	const amrMagic = "#!AMR\n"
	if !bytes.HasPrefix(data, []byte(amrMagic)) {
		return nil, fmt.Errorf("wyze/gwell: unsupported AMR header in %s", label)
	}

	data = data[len(amrMagic):]
	frames := make([][]byte, 0, len(data)/16)
	for off := 0; off < len(data); {
		header := data[off]
		ft := (header >> 3) & 0x0F
		size := amrNBFrameSizes[ft]
		if size == 0 {
			return nil, fmt.Errorf("wyze/gwell: unsupported AMR-NB frame type %d at offset %d in %s", ft, off, label)
		}
		if off+size > len(data) {
			return nil, fmt.Errorf("wyze/gwell: truncated AMR frame at offset %d in %s", off, label)
		}
		frame := append([]byte(nil), data[off:off+size]...)
		frames = append(frames, frame)
		off += size
	}

	return frames, nil
}
