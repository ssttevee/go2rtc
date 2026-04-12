package wyze

import (
	"fmt"
	"net"
	"net/url"
	"time"

	gwelllib "github.com/wlatic/wyze-gwell-bridge/wyze-p2p/pkg/gwell"
)

type GWellClient struct {
	session *gwelllib.Session
	writer  *gwellAnnexBWriter
	host    string
	mac     string
	verbose bool
}

func DialGWell(rawURL string) (*GWellClient, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("wyze/gwell: invalid URL: %w", err)
	}

	query := u.Query()
	accessID := query.Get("access_id")
	accessToken := query.Get("access_token")
	if accessID == "" || accessToken == "" {
		return nil, fmt.Errorf("wyze/gwell: access_id and access_token are required")
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
	sess := gwelllib.NewSession(gwelllib.SessionConfig{
		Token:       token,
		CameraLanIP: cameraLANIP,
		DeviceName:  query.Get("mac"),
		H264Writer:  writer,
	})

	return &GWellClient{
		session: sess,
		writer:  writer,
		host:    host,
		mac:     query.Get("mac"),
		verbose: query.Get("verbose") == "true",
	}, nil
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

func (c *GWellClient) RemoteAddr() net.Addr {
	if c.host == "" {
		return nil
	}
	return gwellAddr(c.host)
}

type gwellAddr string

func (a gwellAddr) Network() string { return "udp" }
func (a gwellAddr) String() string  { return string(a) }
