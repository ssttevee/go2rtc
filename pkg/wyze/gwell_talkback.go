package wyze

import (
	"bytes"
	"fmt"
	"io"
)

var amrNBFrameSizes = [16]int{13, 14, 16, 18, 20, 21, 27, 32, 6, 0, 0, 0, 0, 0, 0, 1}

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
