package wyze

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"time"
)

type DuoPTZStatus struct {
	Pan  float32 `json:"pan,omitempty"`
	Tilt float32 `json:"tilt,omitempty"`
	Raw  any     `json:"raw,omitempty"`
}

type ptzMoveState struct {
	stopCh chan struct{}
	doneCh chan struct{}
}

var duoPTZMoves sync.Map

func (p *GWellProducer) TapPTZ(pan, tilt float32) error {
	if p == nil || p.client == nil {
		return fmt.Errorf("wyze/gwell: producer is nil")
	}
	angle, ok := vectorToAngle(pan, tilt)
	if !ok {
		return fmt.Errorf("wyze/gwell: unsupported zero vector")
	}
	return p.client.SendDuoPTZOnce(angle)
}

func (p *GWellProducer) StartPTZ(pan, tilt float32, hold time.Duration) error {
	if p == nil || p.client == nil {
		return fmt.Errorf("wyze/gwell: producer is nil")
	}
	angle, ok := vectorToAngle(pan, tilt)
	if !ok {
		return fmt.Errorf("wyze/gwell: unsupported zero vector")
	}
	if hold <= 0 {
		hold = time.Second
	}
	if err := p.StopPTZ(); err != nil {
		return err
	}

	state := &ptzMoveState{
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	duoPTZMoves.Store(p, state)

	go func() {
		defer close(state.doneCh)
		deadline := time.NewTimer(hold)
		defer deadline.Stop()
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()

		_ = p.client.SendDuoPTZ(angle)
		for {
			select {
			case <-state.stopCh:
				_ = p.client.StopDuoPTZ()
				duoPTZMoves.Delete(p)
				return
			case <-ticker.C:
				_ = p.client.SendDuoPTZ(angle)
			case <-deadline.C:
				_ = p.client.StopDuoPTZ()
				duoPTZMoves.Delete(p)
				return
			}
		}
	}()

	return nil
}

func (p *GWellProducer) StopPTZ() error {
	if p == nil || p.client == nil {
		return fmt.Errorf("wyze/gwell: producer is nil")
	}
	if v, ok := duoPTZMoves.Load(p); ok {
		state := v.(*ptzMoveState)
		select {
		case <-state.stopCh:
		default:
			close(state.stopCh)
		}
		<-state.doneCh
	}
	return p.client.StopDuoPTZ()
}

func (p *GWellProducer) GetPTZStatus(timeout time.Duration) (*DuoPTZStatus, error) {
	if p == nil || p.client == nil {
		return nil, fmt.Errorf("wyze/gwell: producer is nil")
	}
	if err := p.client.GetDuoPTZLocation(); err != nil {
		return nil, err
	}
	responses, err := collectPTZResponses(p.client.RawData(), timeout)
	if err != nil {
		return nil, err
	}
	for _, raw := range responses {
		var v struct {
			X json.Number `json:"x"`
			Y json.Number `json:"y"`
		}
		if err = json.Unmarshal(raw, &v); err != nil {
			continue
		}
		pan, err1 := v.X.Float64()
		tilt, err2 := v.Y.Float64()
		if err1 == nil && err2 == nil {
			return &DuoPTZStatus{Pan: float32(pan), Tilt: float32(tilt), Raw: json.RawMessage(raw)}, nil
		}
	}
	if len(responses) > 0 {
		return &DuoPTZStatus{Raw: json.RawMessage(responses[len(responses)-1])}, nil
	}
	return nil, fmt.Errorf("wyze/gwell: no PTZ status response")
}

func collectPTZResponses(rawData <-chan *DecodedPayload, timeout time.Duration) ([]json.RawMessage, error) {
	deadline := time.After(timeout)
	responses := make([]json.RawMessage, 0, 4)
	for {
		select {
		case payload, ok := <-rawData:
			if !ok {
				return responses, nil
			}
			if payload == nil || payload.FrameType != 0x02 || len(payload.Payload) == 0 {
				continue
			}
			if raw := extractPTZJSON(payload.Payload); raw != nil {
				responses = append(responses, raw)
			}
		case <-deadline:
			return responses, nil
		}
	}
}

func extractPTZJSON(payload []byte) json.RawMessage {
	start := bytes.IndexByte(payload, '{')
	end := bytes.LastIndexByte(payload, '}')
	if start < 0 || end < start {
		return nil
	}
	raw := append(json.RawMessage(nil), payload[start:end+1]...)
	if !json.Valid(raw) {
		return nil
	}
	return raw
}

func vectorToAngle(pan, tilt float32) (float32, bool) {
	if pan == 0 && tilt == 0 {
		return 0, false
	}
	degrees := math.Atan2(float64(tilt), float64(pan)) * 180 / math.Pi
	if degrees < 0 {
		degrees += 360
	}
	return float32(degrees), true
}
