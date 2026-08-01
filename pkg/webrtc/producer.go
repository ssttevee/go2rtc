package webrtc

import (
	"strings"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/webrtc/v4"
)

func (c *Conn) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	core.Assert(media.Direction == core.DirectionRecvonly)

	for _, track := range c.Receivers {
		if track.Codec == codec {
			return track, nil
		}
	}

	switch c.Mode {
	case core.ModePassiveConsumer: // backchannel from browser
		// set codec for consumer recv track so remote peer should send media with this codec
		tr := c.getTranseiver(media.ID)
		if tr == nil {
			return nil, core.ErrCantGetTrack
		}
		if err := setCodecPreferences(tr, []*core.Codec{codec}); err != nil {
			return nil, err
		}

	case core.ModePassiveProducer, core.ModeActiveProducer:
		// Passive producers: OBS Studio via WHIP or Browser
		// Active producers: go2rtc as WebRTC client or WebTorrent

	default:
		panic(core.Caller())
	}

	track := core.NewReceiver(media, codec)
	c.Receivers = append(c.Receivers, track)
	return track, nil
}

func setCodecPreferences(tr *webrtc.RTPTransceiver, requested []*core.Codec) error {
	var codecs []webrtc.RTPCodecParameters
	if sender := tr.Sender(); sender != nil {
		codecs = sender.GetParameters().Codecs
	} else if receiver := tr.Receiver(); receiver != nil {
		codecs = receiver.GetParameters().Codecs
	}

	var preferences []webrtc.RTPCodecParameters
	for _, params := range codecs {
		for _, codec := range requested {
			if strings.EqualFold(params.MimeType, MimeType(codec)) &&
				(codec.ClockRate == 0 || params.ClockRate == codec.ClockRate) &&
				(codec.Channels == 0 || params.Channels == uint16(codec.Channels)) {
				preferences = append(preferences, params)
				break
			}
		}
	}
	if len(preferences) == 0 {
		return webrtc.ErrCodecNotFound
	}
	return tr.SetCodecPreferences(preferences)
}

func (c *Conn) Start() error {
	c.closed.Wait()
	return nil
}
