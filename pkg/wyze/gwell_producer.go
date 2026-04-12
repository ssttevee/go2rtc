package wyze

import (
	"fmt"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h264/annexb"
	"github.com/pion/rtp"
)

type GWellProducer struct {
	core.Connection
	client *GWellClient
}

func NewGWellProducer(rawURL string) (*GWellProducer, error) {
	client, err := DialGWell(rawURL)
	if err != nil {
		return nil, err
	}

	medias, err := probeGWell(client)
	if err != nil {
		_ = client.Close()
		return nil, err
	}

	prod := &GWellProducer{
		Connection: core.Connection{
			ID:         core.NewID(),
			FormatName: "wyze/gwell",
			Protocol:   client.Protocol(),
			RemoteAddr: client.RemoteAddr().String(),
			Source:     rawURL,
			Medias:     medias,
			Transport:  client,
		},
		client: client,
	}

	return prod, nil
}

func (p *GWellProducer) Start() error {
	for {
		_ = p.client.SetDeadline(time.Now().Add(core.ConnDeadline))
		frame, ts, err := p.client.ReadFrame()
		if err != nil {
			return fmt.Errorf("wyze/gwell: read frame: %w", err)
		}

		avcc := annexb.EncodeToAVCC(frame)
		if len(avcc) < 5 {
			continue
		}
		pkt := &core.Packet{
			Header:  rtp.Header{SequenceNumber: uint16(ts), Timestamp: ts},
			Payload: avcc,
		}

		for _, recv := range p.Receivers {
			if recv.Codec.Name != core.CodecH264 {
				continue
			}
			recv.WriteRTP(pkt)
		}
	}
}

func probeGWell(client *GWellClient) ([]*core.Media, error) {
	errCh := client.StartAsync()

	if err := client.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return nil, err
	}
	frame, _, err := client.ReadFrame()
	_ = client.SetDeadline(time.Time{})
	if err != nil {
		select {
		case runErr := <-errCh:
			if runErr != nil {
				return nil, runErr
			}
		default:
		}
		return nil, fmt.Errorf("wyze/gwell: probe: %w", err)
	}

	buf := annexb.EncodeToAVCC(frame)
	if len(buf) < 5 {
		return nil, fmt.Errorf("wyze/gwell: probe: first frame too short")
	}

	var vcodec *core.Codec
	if h264.NALUType(buf) == h264.NALUTypeSPS {
		vcodec = h264.AVCCToCodec(buf)
	} else {
		vcodec = &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}
	}

	client.writer.mu.Lock()
	client.writer.queue = append([]gwellFrame{{payload: frame}}, client.writer.queue...)
	client.writer.mu.Unlock()
	return []*core.Media{{
		Kind:      core.KindVideo,
		Direction: core.DirectionRecvonly,
		Codecs:    []*core.Codec{vcodec},
	}}, nil
}
