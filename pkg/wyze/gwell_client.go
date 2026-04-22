package wyze

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"sync"
	"time"

	gwelllib "github.com/wlatic/wyze-gwell-bridge/wyze-p2p/pkg/gwell"
)

type GWellClient struct {
	session *gwelllib.Session
	writer  *gwellAnnexBWriter
	rawData chan *gwelllib.DecodedPayload
	host    string
	mac     string
	account string
	verbose bool
	talkHeaderMu     sync.Mutex
	talkHeaderPrimed bool
}

func DialGWell(rawURL string) (*GWellClient, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("wyze/gwell: invalid URL: %w", err)
	}

	query := u.Query()
	account := query.Get("account")
	mac := query.Get("mac")
	accessID, accessToken, err := resolveGWellAccessCredentialsFromURL(u, false)
	if err != nil {
		return nil, err
	}
	if accessID == "" || accessToken == "" {
		return nil, fmt.Errorf("wyze/gwell: access credentials unavailable")
	}

	token, err := gwelllib.ParseAccessToken(accessID, accessToken)
	if err != nil {
		return nil, fmt.Errorf("wyze/gwell: parse access token: %w", err)
	}

	host := u.Host
	if host == "" {
		host = query.Get("mac")
	}

	var cameraLANIP string
	if ip := net.ParseIP(host); ip != nil {
		cameraLANIP = ip.String()
	}

	writer := newGWellAnnexBWriter()
	rawData := make(chan *gwelllib.DecodedPayload, 256)
	cfg := gwelllib.SessionConfig{
		Token:       token,
		CameraLanIP: cameraLANIP,
		DeviceName:  mac,
		H264Writer:  writer,
		RawDataHandler: func(p *gwelllib.DecodedPayload) {
			select {
			case rawData <- p:
			default:
			}
		},
	}
	if account != "" && mac != "" {
		if cache, cacheErr := loadGWellSessionCache(account, mac); cacheErr == nil {
			cfg.ServerAddr = cache.ServerAddr
			cfg.Devices = append([]gwelllib.DeviceInfo(nil), cache.Devices...)
		}
	}
	sess := gwelllib.NewSession(cfg)

	return &GWellClient{
		session: sess,
		writer:  writer,
		rawData: rawData,
		host:    host,
		mac:     mac,
		account: account,
		verbose: query.Get("verbose") == "true",
	}, nil
}

func (c *GWellClient) RawData() <-chan *gwelllib.DecodedPayload {
	return c.rawData
}

func (c *GWellClient) ReadFrame() ([]byte, uint32, error) {
	return c.writer.ReadFrame()
}

func (c *GWellClient) Start() error {
	return c.session.Run(c.mac)
}

func (c *GWellClient) StartAsync() <-chan error {
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.session.Run(c.mac)
	}()
	return errCh
}

func (c *GWellClient) Close() error {
	if c.writer != nil {
		_ = c.writer.Close()
	}
	if c.session != nil {
		return c.session.Close()
	}
	return nil
}

func (c *GWellClient) SetDeadline(t time.Time) error {
	return c.writer.SetDeadline(t)
}

func (c *GWellClient) Protocol() string {
	return "wyze/gwell"
}

func (c *GWellClient) SendUserData(payload []byte) error {
	if c == nil || c.session == nil {
		return fmt.Errorf("wyze/gwell: session is nil")
	}
	playerPayload := make([]byte, 8+len(payload))
	playerPayload[0] = 1
	playerPayload[1] = 0
	playerPayload[2] = 0
	binary.LittleEndian.PutUint32(playerPayload[4:8], uint32(time.Now().UnixMilli()))
	copy(playerPayload[8:], payload)
	return c.session.SendUserData(playerPayload)
}

func (c *GWellClient) SendInnerUserData(cmd byte, payload []byte, expectResponse bool) error {
	if c == nil || c.session == nil {
		return fmt.Errorf("wyze/gwell: session is nil")
	}
	playerPayload := gwelllib.BuildPlayerUserDataHeader(0, cmd, 0, uint32(time.Now().UnixMilli()), payload)
	return c.session.SendInnerUserData(playerPayload, expectResponse)
}

func (c *GWellClient) SetDuoTalkEnabled(enabled bool) error {
	state := byte(0)
	if enabled {
		state = 1
	}
	return c.SendInnerUserData(0x32, []byte{state}, true)
}

func (c *GWellClient) SendDataPayload(payload []byte) error {
	if c == nil || c.session == nil {
		return fmt.Errorf("wyze/gwell: session is nil")
	}
	return c.session.SendDataPayload(payload)
}

func (c *GWellClient) SendDuoTalkHeader(packedHeader20 []byte) error {
	payload, err := gwelllib.BuildGWELLAVHeaderPacket(0x02, packedHeader20)
	if err != nil {
		return fmt.Errorf("wyze/gwell: build talk header: %w", err)
	}
	return c.SendDataPayload(payload)
}

func (c *GWellClient) SendDefaultDuoTalkHeader() error {
	header := gwelllib.BuildGWELLPackedAVHeader20(gwelllib.GWELLAVHeader{
		AudioType:            5,
		AudioCodecOption:     7,
		AudioMode:            0,
		AudioBitWidth:        1,
		AudioSampleRate:      8000,
		AudioSamplesPerFrame: 0x140,
		VideoType:            0,
		FrameRate:            15,
		Width:                240,
		Height:               320,
	})
	return c.SendDuoTalkHeader(header)
}

func (c *GWellClient) PrimeDuoTalkHeader() error {
	c.talkHeaderMu.Lock()
	defer c.talkHeaderMu.Unlock()
	if c.talkHeaderPrimed {
		return nil
	}
	if err := c.SendDefaultDuoTalkHeader(); err != nil {
		return err
	}
	c.talkHeaderPrimed = true
	return nil
}

func (c *GWellClient) SendDuoTalkAudioFrames(audioFrames [][]byte, audioPTS uint64) error {
	if len(audioFrames) == 0 {
		return fmt.Errorf("wyze/gwell: no audio frames")
	}
	payload := gwelllib.BuildGWELLAVAudioPacket(0x02, audioFrames, nil, 0, audioPTS)
	return c.SendDataPayload(payload)
}

func (c *GWellClient) SendUserDataJSON(v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("wyze/gwell: marshal user data json: %w", err)
	}
	return c.SendUserData(payload)
}

func (c *GWellClient) SendDuoPTZ(angle float32) error {
	return c.SendUserDataJSON(map[string]any{
		"cmd":   8,
		"angle": angle,
	})
}

func (c *GWellClient) HoldDuoPTZ(angle float32, hold time.Duration) error {
	if err := c.SendDuoPTZ(angle); err != nil {
		return err
	}
	if hold <= 0 {
		return nil
	}

	const resendInterval = 200 * time.Millisecond
	deadline := time.Now().Add(hold)
	ticker := time.NewTicker(resendInterval)
	defer ticker.Stop()

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return c.StopDuoPTZ()
		}

		wait := remaining
		if wait > resendInterval {
			wait = resendInterval
		}

		select {
		case <-ticker.C:
			if time.Now().Before(deadline) {
				if err := c.SendDuoPTZ(angle); err != nil {
					return err
				}
			}
		case <-time.After(wait):
			if time.Now().After(deadline) || time.Now().Equal(deadline) {
				return c.StopDuoPTZ()
			}
		}
	}
}

func (c *GWellClient) SendDuoPTZOnce(angle float32) error {
	return c.SendUserDataJSON(map[string]any{
		"cmd":   8,
		"angle": angle,
		"once":  1,
	})
}

func (c *GWellClient) StopDuoPTZ() error {
	return c.SendUserDataJSON(map[string]any{
		"cmd":  8,
		"stop": 1,
	})
}

func (c *GWellClient) GetDuoPTZLocation() error {
	return c.SendUserDataJSON(map[string]any{
		"cmd": 9,
		"op":  "get",
	})
}

func (c *GWellClient) RemoteAddr() net.Addr {
	if c.host == "" {
		return nil
	}
	return gwellAddr(c.host)
}

func (c *GWellClient) CacheDiscoveryState() error {
	if c == nil || c.session == nil || c.account == "" || c.mac == "" {
		return nil
	}
	state := c.session.DiscoveryState()
	if state == nil || state.ServerAddr == "" || len(state.Devices) == 0 {
		return nil
	}
	return saveGWellSessionCache(c.account, c.mac, &GWellSessionCache{
		ServerAddr: state.ServerAddr,
		Devices:    append([]gwelllib.DeviceInfo(nil), state.Devices...),
		UpdatedAt:  time.Now().UTC(),
	})
}

type gwellAddr string

func (a gwellAddr) Network() string { return "udp" }
func (a gwellAddr) String() string  { return string(a) }
