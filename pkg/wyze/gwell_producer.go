package wyze

import (
	"fmt"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/h264/annexb"
	"github.com/pion/rtp"
	gwelllib "github.com/wlatic/wyze-gwell-bridge/wyze-p2p/pkg/gwell"
)

type GWellProducer struct {
	core.Connection
	client *GWellClient
}

var gwellAudioCodec = &core.Codec{Name: core.CodecPCMA, ClockRate: 8000, Channels: 1, PayloadType: 8}

type DecodedPayload = gwelllib.DecodedPayload

type ParsedCMDPayload struct {
	Magic      uint32
	Type       uint16
	Code       uint16
	Value0     uint32
	Value1     uint32
	Value2     uint32
	RepeatType uint16
	RepeatCode uint16
	Tail       []byte
}

func ParseCMDPayload(payload []byte) *ParsedCMDPayload {
	if len(payload) < 24 {
		return nil
	}
	parsed := &ParsedCMDPayload{
		Magic:  uint32(payload[0])<<24 | uint32(payload[1])<<16 | uint32(payload[2])<<8 | uint32(payload[3]),
		Type:   uint16(payload[4])<<8 | uint16(payload[5]),
		Code:   uint16(payload[6])<<8 | uint16(payload[7]),
		Value0: uint32(payload[8]) | uint32(payload[9])<<8 | uint32(payload[10])<<16 | uint32(payload[11])<<24,
		Value1: uint32(payload[12]) | uint32(payload[13])<<8 | uint32(payload[14])<<16 | uint32(payload[15])<<24,
		Value2: uint32(payload[16]) | uint32(payload[17])<<8 | uint32(payload[18])<<16 | uint32(payload[19])<<24,
	}
	if len(payload) >= 28 {
		parsed.RepeatType = uint16(payload[20])<<8 | uint16(payload[21])
		parsed.RepeatCode = uint16(payload[22])<<8 | uint16(payload[23])
		parsed.Tail = append([]byte(nil), payload[24:]...)
	} else {
		parsed.Tail = append([]byte(nil), payload[20:]...)
	}
	return parsed
}

func (p *GWellProducer) RawData() <-chan *gwelllib.DecodedPayload {
	if p == nil || p.client == nil {
		return nil
	}
	return p.client.RawData()
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
	var audioTS uint32
	var audioSeq uint16
	rawData := p.client.RawData()
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
		size := int(avcc[0])<<24 | int(avcc[1])<<16 | int(avcc[2])<<8 | int(avcc[3])
		if size <= 0 || size+4 > len(avcc) {
			continue
		}
		pkt := &core.Packet{
			Header:  rtp.Header{SequenceNumber: uint16(ts), Timestamp: ts},
			Payload: avcc,
		}
		if len(pkt.Payload) < 5 {
			continue
		}

		for _, recv := range p.Receivers {
			if recv.Codec.Name != core.CodecH264 {
				continue
			}
			recv.WriteRTP(pkt)
		}

		for {
			select {
			case payload, ok := <-rawData:
				if !ok || payload == nil || len(payload.Audio) == 0 {
					goto nextFrame
				}
				for _, recv := range p.Receivers {
					if !recv.Codec.Match(gwellAudioCodec) {
						continue
					}
					recv.WriteRTP(&core.Packet{
						Header:  rtp.Header{Version: 2, Marker: true, SequenceNumber: audioSeq, Timestamp: audioTS},
						Payload: append([]byte(nil), payload.Audio...),
					})
				}
				audioSeq++
				audioTS += uint32(len(payload.Audio))
			default:
				goto nextFrame
			}
		}
	nextFrame:
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
		vcodec := &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}
		client.writer.mu.Lock()
		client.writer.queue = append([]gwellFrame{{payload: frame}}, client.writer.queue...)
		client.writer.mu.Unlock()
		return []*core.Media{{
			Kind:      core.KindVideo,
			Direction: core.DirectionRecvonly,
			Codecs:    []*core.Codec{vcodec},
		}, {
			Kind:      core.KindAudio,
			Direction: core.DirectionRecvonly,
			Codecs:    []*core.Codec{gwellAudioCodec.Clone()},
		}}, nil
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
	}, {
		Kind:      core.KindAudio,
		Direction: core.DirectionRecvonly,
		Codecs:    []*core.Codec{gwellAudioCodec.Clone()},
	}}, nil
}
