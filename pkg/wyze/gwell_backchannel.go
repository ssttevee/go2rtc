package wyze

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	ffmpegpkg "github.com/AlexxIT/go2rtc/pkg/ffmpeg"
	"github.com/AlexxIT/go2rtc/pkg/shell"
	"github.com/pion/rtp"
)

func (p *GWellProducer) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
	if media == nil || media.Kind != core.KindAudio || media.Direction != core.DirectionSendonly {
		return core.ErrCantGetTrack
	}
	if p == nil || p.client == nil {
		return fmt.Errorf("wyze/gwell: producer is nil")
	}

	stdinCodec := ffmpegInputCodec(track.Codec)
	if stdinCodec == "" {
		return fmt.Errorf("wyze/gwell: unsupported backchannel codec: %s", track.Codec.String())
	}

	cmd := shell.NewCommand(buildTalkbackFFmpegCmd(stdinCodec, track.Codec))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err = cmd.Start(); err != nil {
		return err
	}
	fmt.Printf("[wyze/gwell] backchannel ffmpeg start: codec=%s cmd=%s\n", track.Codec.String(), cmd.String())

	go func() {
		defer cmd.Close()
		defer stdin.Close()
		debugPath := "/tmp/gwell_backchannel_live.amr"
		debugOut, debugErr := os.Create(debugPath)
		if debugErr != nil {
			fmt.Printf("[wyze/gwell] backchannel debug create failed: %v\n", debugErr)
		}
		var amrReader io.Reader = stdout
		if debugOut != nil {
			defer debugOut.Close()
			amrReader = io.TeeReader(stdout, debugOut)
			fmt.Printf("[wyze/gwell] backchannel debug tee: %s\n", debugPath)
		}

		if err := p.client.PlayDuoTalkAMR(amrReader); err != nil {
			if stderr.Len() > 0 {
				fmt.Printf("[wyze/gwell] backchannel talk failed: %v stderr=%s\n", err, strings.TrimSpace(stderr.String()))
			} else {
				fmt.Printf("[wyze/gwell] backchannel talk failed: %v\n", err)
			}
		}
		if waitErr := cmd.Wait(); waitErr != nil {
			if stderr.Len() > 0 {
				fmt.Printf("[wyze/gwell] backchannel ffmpeg exit: %v stderr=%s\n", waitErr, strings.TrimSpace(stderr.String()))
			} else {
				fmt.Printf("[wyze/gwell] backchannel ffmpeg exit: %v\n", waitErr)
			}
		}
	}()

	sender := core.NewSender(media, track.Codec)
	var packetCount int
	sender.Handler = func(pkt *rtp.Packet) {
		packetCount++
		if packetCount <= 3 || packetCount%100 == 0 {
			fmt.Printf("[wyze/gwell] backchannel input packet %d: payload=%d bytes ts=%d\n", packetCount, len(pkt.Payload), pkt.Timestamp)
		}
		if n, err := stdin.Write(pkt.Payload); err == nil {
			p.Send += n
		} else {
			fmt.Printf("[wyze/gwell] backchannel stdin write failed after %d packets: %v\n", packetCount, err)
		}
	}
	sender.HandleRTP(track)
	p.Senders = append(p.Senders, sender)
	return nil
}

func ffmpegInputCodec(codec *core.Codec) string {
	if codec == nil {
		return ""
	}
	switch codec.Name {
	case core.CodecPCMU:
		return "mulaw"
	case core.CodecPCMA:
		return "alaw"
	case core.CodecPCML:
		return "s16le"
	case core.CodecPCM:
		return "s16be"
	default:
		return ""
	}
}

func buildTalkbackFFmpegCmd(stdinCodec string, codec *core.Codec) string {
	rate := codec.ClockRate
	if rate == 0 {
		rate = 8000
	}
	channels := codec.Channels
	if channels == 0 {
		channels = 1
	}
	args := ffmpegpkg.Args{
		Bin:    "ffmpeg",
		Global: "-hide_banner -loglevel error -nostdin",
		Input: fmt.Sprintf("-fflags +genpts -f %s -ar %d -ac %d -i pipe:0",
			stdinCodec, rate, channels,
		),
		Output: "-vn -c:a libopencore_amrnb -ar:a 8000 -ac:a 1 -f amr pipe:1",
	}
	return strings.TrimSpace(args.String())
}

func (c *GWellClient) PlayDuoTalkAMR(rd io.Reader) error {
	frames := make(chan []byte, 24)
	errCh := make(chan error, 1)

	go func() {
		defer close(frames)
		if err := streamAMRNB(rd, func(frame []byte) error {
			frames <- frame
			return nil
		}); err != nil {
			errCh <- err
		}
	}()

	prebuffer := make([][]byte, 0, 3)
	for len(prebuffer) < 3 {
		select {
		case frame, ok := <-frames:
			if !ok {
				select {
				case err := <-errCh:
					return err
				default:
				}
				if len(prebuffer) == 0 {
					return fmt.Errorf("wyze/gwell: no AMR frames produced")
				}
				goto startTalk
			}
			prebuffer = append(prebuffer, frame)
		case err := <-errCh:
			return err
		}
	}

startTalk:
	if err := c.SendDefaultDuoTalkHeader(); err != nil {
		return err
	}
	time.Sleep(40 * time.Millisecond)
	fmt.Printf("[wyze/gwell] talk_on\n")
	if err := c.SetDuoTalkEnabled(true); err != nil {
		return err
	}
	defer func() { _ = c.SetDuoTalkEnabled(false) }()
	time.Sleep(100 * time.Millisecond)

	fmt.Printf("[wyze/gwell] talk_header\n")
	if err := c.SendDefaultDuoTalkHeader(); err != nil {
		return err
	}
	time.Sleep(40 * time.Millisecond)

	const (
		batchStep       = 60 * time.Millisecond
		audioPTSStepNB = 160
	)
	var audioPTS uint64
	var frameCount int
	sendBatch := func(batch [][]byte) error {
		frameCount += len(batch)
		if frameCount <= 3 || frameCount%100 == 0 {
			fmt.Printf("[wyze/gwell] AMR batch: frames=%d last_count=%d pts=%d\n", len(batch), frameCount, audioPTS)
		}
		if err := c.SendDuoTalkAudioFrames(batch, audioPTS); err != nil {
			return err
		}
		audioPTS += uint64(len(batch) * audioPTSStepNB)
		time.Sleep(batchStep)
		return nil
	}

	for len(prebuffer) >= 3 {
		if err := sendBatch(prebuffer[:3]); err != nil {
			return err
		}
		prebuffer = prebuffer[3:]
	}

	batch := append([][]byte(nil), prebuffer...)
	for frame := range frames {
		batch = append(batch, frame)
		if len(batch) < 3 {
			continue
		}
		if err := sendBatch(batch[:3]); err != nil {
			return err
		}
		batch = batch[:0]
	}

	select {
	case err := <-errCh:
		return err
	default:
	}

	if len(batch) > 0 {
		if err := sendBatch(batch); err != nil {
			return err
		}
	}

	if frameCount == 0 {
		return fmt.Errorf("wyze/gwell: no AMR frames produced")
	}
	fmt.Printf("[wyze/gwell] backchannel sent %d AMR frames\n", frameCount)
	return nil
}
