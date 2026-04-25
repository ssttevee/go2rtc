package onvif

import (
	"testing"

	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/wyze"
)

func TestGetLocalRTSPStreamName(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{name: "loopback rtsp", source: "rtsp://127.0.0.1:8554/duo?backchannel=1", want: "duo"},
		{name: "localhost rtsps", source: "rtsps://localhost:8554/duo_boosted", want: "duo_boosted"},
		{name: "remote rtsp ignored", source: "rtsp://192.168.1.10:8554/duo", want: ""},
		{name: "non rtsp ignored", source: "wyze://camera", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getLocalRTSPStreamName(tt.source); got != tt.want {
				t.Fatalf("getLocalRTSPStreamName(%q) = %q, want %q", tt.source, got, tt.want)
			}
		})
	}
}

func TestGetActiveGWellProducerDirect(t *testing.T) {
	resetTestStreams()
	registerTestHandlers()

	stream, err := streams.New("duo", "test://placeholder")
	if err != nil {
		t.Fatalf("streams.New direct: %v", err)
	}
	stream.AddProducer(&wyze.GWellProducer{})

	prod, err := getActiveGWellProducer("duo")
	if err != nil {
		t.Fatalf("getActiveGWellProducer direct: %v", err)
	}
	if prod == nil {
		t.Fatal("expected gwell producer, got nil")
	}
}

func TestGetActiveGWellProducerThroughLoopback(t *testing.T) {
	resetTestStreams()
	registerTestHandlers()

	native, err := streams.New("duo", "test://placeholder")
	if err != nil {
		t.Fatalf("streams.New native: %v", err)
	}
	native.AddProducer(&wyze.GWellProducer{})

	if _, err = streams.New("duo_boosted",
		"exec:test",
		"rtsp://127.0.0.1:8554/duo?backchannel=1",
	); err != nil {
		t.Fatalf("streams.New boosted: %v", err)
	}

	prod, err := getActiveGWellProducer("duo_boosted")
	if err != nil {
		t.Fatalf("getActiveGWellProducer loopback: %v", err)
	}
	if prod == nil {
		t.Fatal("expected gwell producer, got nil")
	}
}

func TestGetActiveGWellProducerThroughNestedLoopback(t *testing.T) {
	resetTestStreams()
	registerTestHandlers()

	native, err := streams.New("duo", "test://placeholder")
	if err != nil {
		t.Fatalf("streams.New native: %v", err)
	}
	native.AddProducer(&wyze.GWellProducer{})

	if _, err = streams.New("duo_boosted", "rtsp://127.0.0.1:8554/duo?backchannel=1"); err != nil {
		t.Fatalf("streams.New boosted: %v", err)
	}
	if _, err = streams.New("duo_overlay", "rtsp://127.0.0.1:8554/duo_boosted"); err != nil {
		t.Fatalf("streams.New overlay: %v", err)
	}

	prod, err := getActiveGWellProducer("duo_overlay")
	if err != nil {
		t.Fatalf("getActiveGWellProducer nested: %v", err)
	}
	if prod == nil {
		t.Fatal("expected gwell producer, got nil")
	}
}

func TestGetActiveGWellProducerCycleDoesNotLoop(t *testing.T) {
	resetTestStreams()
	registerTestHandlers()

	if _, err := streams.New("a", "rtsp://127.0.0.1:8554/b"); err != nil {
		t.Fatalf("streams.New a: %v", err)
	}
	if _, err := streams.New("b", "rtsp://127.0.0.1:8554/a"); err != nil {
		t.Fatalf("streams.New b: %v", err)
	}

	prod, err := getActiveGWellProducer("a")
	if err == nil {
		t.Fatal("expected error for cycle without gwell producer")
	}
	if prod != nil {
		t.Fatal("expected nil producer for cycle without gwell producer")
	}
	if got, want := err.Error(), "onvif/ptz: active gwell producer not found for stream: a"; got != want {
		t.Fatalf("unexpected error: got %q want %q", got, want)
	}
}

func resetTestStreams() {
	for _, name := range streams.GetAllNames() {
		streams.Delete(name)
	}
}

func registerTestHandlers() {
	streams.HandleFunc("test", func(string) (core.Producer, error) { return nil, nil })
	streams.HandleFunc("rtsp", func(string) (core.Producer, error) { return nil, nil })
	streams.HandleFunc("rtsps", func(string) (core.Producer, error) { return nil, nil })
	streams.HandleFunc("rtspx", func(string) (core.Producer, error) { return nil, nil })
	streams.HandleFunc("exec", func(string) (core.Producer, error) { return nil, nil })
}
