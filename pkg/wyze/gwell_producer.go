package wyze

import (
	"fmt"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/aac"
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

func (p *GWellProducer) SetDuoTalkEnabled(enabled bool) error {
	if p == nil || p.client == nil {
		return fmt.Errorf("wyze/gwell: client is nil")
	}
	return p.client.SetDuoTalkEnabled(enabled)
}

func (p *GWellProducer) SendDuoTalkHeader(packedHeader20 []byte) error {
	if p == nil || p.client == nil {
		return fmt.Errorf("wyze/gwell: client is nil")
	}
	return p.client.SendDuoTalkHeader(packedHeader20)
}

func (p *GWellProducer) SendDefaultDuoTalkHeader() error {
	if p == nil || p.client == nil {
		return fmt.Errorf("wyze/gwell: client is nil")
	}
	return p.client.SendDefaultDuoTalkHeader()
}

func (p *GWellProducer) SendDuoTalkAudioFrames(audioFrames [][]byte, audioPTS uint64) error {
	if p == nil || p.client == nil {
		return fmt.Errorf("wyze/gwell: client is nil")
	}
	return p.client.SendDuoTalkAudioFrames(audioFrames, audioPTS)
}

func NewGWellProducer(rawURL string) (*GWellProducer, error) {
	client, err := DialGWell(rawURL)
	if err != nil {
		return nil, err
	}

	medias, err := probeGWell(client)
	if err != nil {
		_ = client.Close()
		if _, _, refreshErr := resolveGWellAccessCredentials(rawURL, true); refreshErr == nil {
			client, err = DialGWell(rawURL)
			if err == nil {
				medias, err = probeGWell(client)
			}
		}
		if err != nil {
			return nil, err
		}
	}

	prod := &GWellProducer{
		Connection: core.Connection{
			ID:         core.NewID(),
			FormatName: "wyze/gwell",
			Protocol:   client.Protocol(),
			RemoteAddr: client.RemoteAddr().String(),
			Source:     rawURL,
			Medias:     append(medias, &core.Media{Kind: core.KindAudio, Direction: core.DirectionSendonly, Codecs: []*core.Codec{{Name: core.CodecPCMU, ClockRate: 8000}, {Name: core.CodecPCMA, ClockRate: 8000}, {Name: core.CodecPCML, ClockRate: 8000}}}),
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
	audioCodec := p.audioCodec()
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
				if !ok {
					goto nextFrame
				}
				if payload == nil || len(payload.Audio) == 0 {
					continue
				}
				audioPayload := payload.Audio
				if len(audioPayload) >= 7 && audioPayload[0] == 0xFF && (audioPayload[1]&0xF0) == 0xF0 {
					audioPayload = stripADTSAAC(audioPayload)
				}
				if len(audioPayload) == 0 {
					goto nextFrame
				}
				for _, recv := range p.Receivers {
					if audioCodec == nil || !recv.Codec.Match(audioCodec) {
						continue
					}
					recv.WriteRTP(&core.Packet{
						Header:  rtp.Header{Version: aac.RTPPacketVersionAAC, Marker: true, SequenceNumber: audioSeq, Timestamp: audioTS},
						Payload: append([]byte(nil), audioPayload...),
					})
				}
				audioSeq++
				audioTS += 1024
			default:
				goto nextFrame
			}
		}
	nextFrame:
	}
}

func (p *GWellProducer) audioCodec() *core.Codec {
	if p == nil {
		return nil
	}
	for _, media := range p.Medias {
		if media.Kind != core.KindAudio || len(media.Codecs) == 0 {
			continue
		}
		return media.Codecs[0]
	}
	return nil
}

func stripADTSAAC(b []byte) []byte {
	if len(b) < 7 {
		return nil
	}
	headerLen := 7
	if b[1]&0x01 == 0 {
		headerLen = 9
	}
	if len(b) <= headerLen {
		return nil
	}
	return b[headerLen:]
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
		acodec := &core.Codec{Name: core.CodecAAC}
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
			Codecs:    []*core.Codec{acodec},
		}}, nil
	}

	var vcodec *core.Codec
	if h264.NALUType(buf) == h264.NALUTypeSPS {
		vcodec = h264.AVCCToCodec(buf)
	} else {
		vcodec = &core.Codec{Name: core.CodecH264, ClockRate: 90000, PayloadType: core.PayloadTypeRAW}
	}
	acodec := probeGWellAudioCodec(client, 1500*time.Millisecond)
	if acodec == nil {
		acodec = &core.Codec{Name: core.CodecAAC}
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
		Codecs:    []*core.Codec{acodec},
	}}, nil
}

func probeGWellAudioCodec(client *GWellClient, timeout time.Duration) *core.Codec {
	if client == nil {
		return nil
	}
	deadline := time.Now().Add(timeout)
	raw := client.RawData()
	for time.Now().Before(deadline) {
		select {
		case payload, ok := <-raw:
			if !ok || payload == nil || len(payload.Audio) == 0 {
				continue
			}
			if aac.IsADTS(payload.Audio) {
				codec := aac.ADTSToCodec(payload.Audio)
				if codec != nil {
					codec.PayloadType = core.PayloadTypeRAW
					return codec
				}
			}
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	return nil
}
