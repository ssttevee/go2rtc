package onvif

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/AlexxIT/go2rtc/internal/api"
	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/AlexxIT/go2rtc/internal/rtsp"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/onvif"
	"github.com/AlexxIT/go2rtc/pkg/wyze"
	"github.com/rs/zerolog"
)

func Init() {
	log = app.GetLogger("onvif")

	var cfg struct {
		Mod struct {
			Username string `yaml:"username"`
			Password string `yaml:"password"`
		} `yaml:"onvif"`
	}
	app.LoadConfig(&cfg)
	authUsername = cfg.Mod.Username
	authPassword = cfg.Mod.Password

	streams.HandleFunc("onvif", streamOnvif)

	// ONVIF server on all suburls
	api.HandleFunc("/onvif/", onvifDeviceService)

	// ONVIF client autodiscovery
	api.HandleFunc("api/onvif", apiOnvif)
}

var log zerolog.Logger
var authUsername string
var authPassword string

func streamOnvif(rawURL string) (core.Producer, error) {
	client, err := onvif.NewClient(rawURL)
	if err != nil {
		return nil, err
	}

	uri, err := client.GetURI()
	if err != nil {
		return nil, err
	}

	// Append hash-based arguments to the retrieved URI
	if i := strings.IndexByte(rawURL, '#'); i > 0 {
		uri += rawURL[i:]
	}

	log.Debug().Msgf("[onvif] new uri=%s", uri)

	if err = streams.Validate(uri); err != nil {
		return nil, err
	}

	return streams.GetProducer(uri)
}

func onvifDeviceService(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	operation := onvif.GetRequestAction(b)
	if operation == "" {
		http.Error(w, "malformed request body", http.StatusBadRequest)
		return
	}

	log.Trace().Msgf("[onvif] server request %s %s:\n%s", r.Method, r.RequestURI, b)

	if authUsername != "" && operation != onvif.DeviceGetSystemDateAndTime {
		if !onvif.ValidateUsernameToken(b, authUsername, authPassword, time.Now(), onvif.DefaultUsernameTokenAge) {
			log.Debug().Str("addr", r.RemoteAddr).Str("operation", operation).Msg("[onvif] auth failed")
			api.Response(w, onvif.NotAuthorizedResponse(), "application/soap+xml; charset=utf-8")
			return
		}
	}

	switch operation {
	case onvif.ServiceGetServiceCapabilities, // important for Hass
		onvif.DeviceGetNetworkInterfaces, // important for Hass
		onvif.DeviceGetSystemDateAndTime, // important for Hass
		onvif.DeviceSetSystemDateAndTime, // return just OK
		onvif.DeviceGetDiscoveryMode,
		onvif.DeviceGetDNS,
		onvif.DeviceGetHostname,
		onvif.DeviceGetNetworkDefaultGateway,
		onvif.DeviceGetNetworkProtocols,
		onvif.DeviceGetNTP,
		onvif.DeviceGetScopes,
		onvif.MediaGetVideoEncoderConfiguration,
		onvif.MediaGetVideoEncoderConfigurations,
		onvif.MediaGetAudioEncoderConfigurations,
		onvif.MediaGetVideoEncoderConfigurationOptions,
		onvif.MediaGetAudioSources,
		onvif.MediaGetAudioSourceConfigurations:
		b = onvif.StaticResponse(operation)

	case onvif.DeviceGetCapabilities:
		// important for Hass: Media section
		b = onvif.GetCapabilitiesResponse(r.Host)

	case onvif.DeviceGetServices:
		b = onvif.GetServicesResponse(r.Host)

	case onvif.DeviceGetDeviceInformation:
		// important for Hass: SerialNumber (unique server ID)
		b = onvif.GetDeviceInformationResponse("", "go2rtc", app.Version, r.Host)

	case onvif.DeviceSystemReboot:
		b = onvif.StaticResponse(operation)

		time.AfterFunc(time.Second, func() {
			os.Exit(0)
		})

	case onvif.MediaGetVideoSources:
		b = onvif.GetVideoSourcesResponse(streams.GetAllNames())

	case onvif.MediaGetProfiles:
		// important for Hass: H264 codec, width, height
		b = onvif.GetProfilesResponse(streams.GetAllNames())

	case onvif.MediaGetProfile:
		token := onvif.FindTagValue(b, "ProfileToken")
		b = onvif.GetProfileResponse(token)

	case onvif.MediaGetVideoSourceConfigurations:
		// important for Happytime Onvif Client
		b = onvif.GetVideoSourceConfigurationsResponse(streams.GetAllNames())

	case onvif.MediaGetVideoSourceConfiguration:
		token := onvif.FindTagValue(b, "ConfigurationToken")
		b = onvif.GetVideoSourceConfigurationResponse(token)

	case onvif.MediaGetStreamUri:
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host // in case of Host without port
		}

		uri := "rtsp://" + host + ":" + rtsp.Port + "/" + onvif.FindTagValue(b, "ProfileToken")
		b = onvif.GetStreamUriResponse(uri)

	case onvif.MediaGetSnapshotUri:
		uri := "http://" + r.Host + "/api/frame.jpeg?src=" + onvif.FindTagValue(b, "ProfileToken")
		b = onvif.GetSnapshotUriResponse(uri)

	case onvif.PTZGetConfigurations:
		b = onvif.GetPTZConfigurationsResponse(streams.GetAllNames())

	case onvif.PTZGetConfiguration:
		token := onvif.FindTagValue(b, "PTZConfigurationToken")
		if strings.HasPrefix(token, "ptz_") {
			token = strings.TrimPrefix(token, "ptz_")
		}
		b = onvif.GetPTZConfigurationResponse(token)

	case onvif.PTZGetNodes:
		b = onvif.GetPTZNodesResponse()

	case onvif.PTZGetStatus:
		token := onvif.FindTagValue(b, "ProfileToken")
		log.Info().Str("token", token).Msg("[onvif] PTZ GetStatus")
		status, err := getPTZStatus(token)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		b = onvif.GetPTZStatusResponse(status.Pan, status.Tilt)

	case onvif.PTZRelativeMove:
		token := onvif.FindTagValue(b, "ProfileToken")
		pan := parseFloatTag(b, "PanTilt.+?x")
		tilt := parseFloatTag(b, "PanTilt.+?y")
		log.Info().Str("token", token).Float32("pan", pan).Float32("tilt", tilt).Msg("[onvif] PTZ RelativeMove")
		if err := relativeMove(token, pan, tilt); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		b = onvif.RelativeMoveResponse()

	case onvif.PTZContinuousMove:
		token := onvif.FindTagValue(b, "ProfileToken")
		pan := parseFloatTag(b, "PanTilt.+?x")
		tilt := parseFloatTag(b, "PanTilt.+?y")
		timeout := parsePTZTimeout(onvif.FindTagValue(b, "Timeout"))
		log.Info().Str("token", token).Float32("pan", pan).Float32("tilt", tilt).Dur("timeout", timeout).Msg("[onvif] PTZ ContinuousMove")
		if err := continuousMove(token, pan, tilt, timeout); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		b = onvif.ContinuousMoveResponse()

	case onvif.PTZStop:
		token := onvif.FindTagValue(b, "ProfileToken")
		log.Info().Str("token", token).Msg("[onvif] PTZ Stop")
		if err := stopPTZ(token); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		b = onvif.StopResponse()

	default:
		http.Error(w, "unsupported operation", http.StatusBadRequest)
		log.Warn().Msgf("[onvif] unsupported operation: %s", operation)
		log.Debug().Msgf("[onvif] unsupported request:\n%s", b)
		return
	}

	log.Trace().Msgf("[onvif] server response:\n%s", b)

	w.Header().Set("Content-Type", "application/soap+xml; charset=utf-8")
	if _, err = w.Write(b); err != nil {
		log.Error().Err(err).Caller().Send()
	}
}

func getPTZStatus(token string) (*wyze.DuoPTZStatus, error) {
	prod, err := getActiveGWellProducer(token)
	if err != nil {
		return nil, err
	}
	return prod.GetPTZStatus(5 * time.Second)
}

func continuousMove(token string, pan, tilt float32, timeout time.Duration) error {
	prod, err := getActiveGWellProducer(token)
	if err != nil {
		return err
	}
	return prod.StartPTZ(pan, tilt, timeout)
}

func relativeMove(token string, pan, tilt float32) error {
	prod, err := getActiveGWellProducer(token)
	if err != nil {
		return err
	}
	return prod.TapPTZ(pan, tilt)
}

func stopPTZ(token string) error {
	prod, err := getActiveGWellProducer(token)
	if err != nil {
		return err
	}
	return prod.StopPTZ()
}

func parseFloatTag(body []byte, tag string) float32 {
	v, err := strconv.ParseFloat(onvif.FindXMLAttr(body, tag), 32)
	if err != nil {
		return 0
	}
	return float32(v)
}

func parsePTZTimeout(value string) time.Duration {
	if value == "" {
		return time.Second
	}
	if strings.HasPrefix(value, "PT") && strings.HasSuffix(value, "S") {
		seconds, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimPrefix(value, "PT"), "S"), 64)
		if err == nil {
			return time.Duration(seconds * float64(time.Second))
		}
	}
	if d, err := time.ParseDuration(value); err == nil {
		return d
	}
	return time.Second
}

func getActiveGWellProducer(token string) (*wyze.GWellProducer, error) {
	prod, err := getActiveGWellProducerRecursive(token, map[string]struct{}{})
	if err != nil {
		return nil, err
	}
	if prod != nil {
		return prod, nil
	}
	return nil, fmt.Errorf("onvif/ptz: active gwell producer not found for stream: %s", token)
}

func getActiveGWellProducerRecursive(token string, visited map[string]struct{}) (*wyze.GWellProducer, error) {
	if _, ok := visited[token]; ok {
		return nil, nil
	}
	visited[token] = struct{}{}

	stream := streams.Get(token)
	if stream == nil {
		return nil, fmt.Errorf("onvif/ptz: stream not found: %s", token)
	}

	for _, producer := range stream.Producers() {
		if producer == nil {
			continue
		}
		conn := producer.Conn()
		if conn == nil {
			continue
		}
		if gwellProd, ok := conn.(*wyze.GWellProducer); ok {
			return gwellProd, nil
		}
	}

	for _, source := range stream.Sources() {
		upstream := getLocalRTSPStreamName(source)
		if upstream == "" {
			continue
		}
		prod, err := getActiveGWellProducerRecursive(upstream, visited)
		if err != nil {
			if strings.Contains(err.Error(), "stream not found") {
				continue
			}
			return nil, err
		}
		if prod != nil {
			return prod, nil
		}
	}

	return nil, nil
}

func getLocalRTSPStreamName(source string) string {
	u, err := url.Parse(source)
	if err != nil {
		return ""
	}
	if u.Scheme != "rtsp" && u.Scheme != "rtsps" && u.Scheme != "rtspx" {
		return ""
	}
	host := u.Hostname()
	if host != "127.0.0.1" && host != "localhost" {
		return ""
	}
	if u.Path == "" || u.Path == "/" {
		return ""
	}
	return strings.TrimPrefix(u.Path, "/")
}

func apiOnvif(w http.ResponseWriter, r *http.Request) {
	src := r.URL.Query().Get("src")

	var items []*api.Source

	if src == "" {
		devices, err := onvif.DiscoveryStreamingDevices()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		for _, device := range devices {
			u, err := url.Parse(device.URL)
			if err != nil {
				log.Warn().Str("url", device.URL).Msg("[onvif] broken")
				continue
			}

			if u.Scheme != "http" {
				log.Warn().Str("url", device.URL).Msg("[onvif] unsupported")
				continue
			}

			u.Scheme = "onvif"
			u.User = url.UserPassword("user", "pass")

			if u.Path == onvif.PathDevice {
				u.Path = ""
			}

			items = append(items, &api.Source{
				Name: u.Host,
				URL:  u.String(),
				Info: device.Name + " " + device.Hardware,
			})
		}
	} else {
		client, err := onvif.NewClient(src)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if l := log.Trace(); l.Enabled() {
			b, _ := client.MediaRequest(onvif.MediaGetProfiles)
			l.Msgf("[onvif] src=%s profiles:\n%s", src, b)
		}

		name, err := client.GetName()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		tokens, err := client.GetProfilesTokens()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		for i, token := range tokens {
			items = append(items, &api.Source{
				Name: name + " stream" + strconv.Itoa(i),
				URL:  src + "?subtype=" + token,
			})
		}

		if len(tokens) > 0 && client.HasSnapshots() {
			items = append(items, &api.Source{
				Name: name + " snapshot",
				URL:  src + "?subtype=" + tokens[0] + "&snapshot",
			})
		}
	}

	api.ResponseSources(w, items)
}
