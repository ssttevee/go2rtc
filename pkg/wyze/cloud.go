package wyze

import (
	"crypto/hmac"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
)

const (
	baseURLAuth = "https://auth-prod.api.wyze.com"
	baseURLAPI  = "https://api.wyzecam.com"
	baseURLMars = "https://wyze-mars-service.wyzecam.com"
	appName     = "com.hualai.WyzeCam"
	appVersion  = "2.50.0"
	gwellPrefix = "GW_"
	wpkAppID    = "9319141212m2ik"
	wpkAppVer   = "2.19.14"
	wpkSalt     = "wyze_app_secret_key_132"
)

type Cloud struct {
	client      *http.Client
	apiKey      string
	keyID       string
	accessToken string
	phoneID     string
	cameras     []*Camera
}

type Camera struct {
	MAC          string `json:"mac"`
	P2PID        string `json:"p2p_id"`
	ENR          string `json:"enr"`
	IP           string `json:"ip"`
	Nickname     string `json:"nickname"`
	ProductModel string `json:"product_model"`
	ProductType  string `json:"product_type"`
	DTLS         int    `json:"dtls"`
	FirmwareVer  string `json:"firmware_ver"`
	IsOnline     bool   `json:"is_online"`
}

type deviceListResponse struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		DeviceList []deviceInfo `json:"device_list"`
	} `json:"data"`
}

type deviceInfo struct {
	MAC          string       `json:"mac"`
	ENR          string       `json:"enr"`
	Nickname     string       `json:"nickname"`
	ProductModel string       `json:"product_model"`
	ProductType  string       `json:"product_type"`
	FirmwareVer  string       `json:"firmware_ver"`
	ConnState    int          `json:"conn_state"`
	DeviceParams deviceParams `json:"device_params"`
}

type deviceParams struct {
	P2PID   string `json:"p2p_id"`
	P2PType int    `json:"p2p_type"`
	IP      string `json:"ip"`
	DTLS    int    `json:"dtls"`
}

type p2pInfoResponse struct {
	Code string         `json:"code"`
	Msg  string         `json:"msg"`
	Data map[string]any `json:"data"`
}

type deviceInfoResponse struct {
	Code string         `json:"code"`
	Msg  string         `json:"msg"`
	Data map[string]any `json:"data"`
}

type loginResponse struct {
	AccessToken    string   `json:"access_token"`
	RefreshToken   string   `json:"refresh_token"`
	UserID         string   `json:"user_id"`
	MFAOptions     []string `json:"mfa_options"`
	SMSSessionID   string   `json:"sms_session_id"`
	EmailSessionID string   `json:"email_session_id"`
}

type marsTokenResponse struct {
	Code    any            `json:"code"`
	Msg     string         `json:"msg"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data"`
}

type GWellAccessCredential struct {
	AccessID    string
	AccessToken string
}

func NewCloud(apiKey, keyID string) *Cloud {
	return &Cloud{
		client:  &http.Client{Timeout: 30 * time.Second},
		phoneID: generatePhoneID(),
		apiKey:  apiKey,
		keyID:   keyID,
	}
}

func (c *Cloud) Login(email, password string) error {
	payload := map[string]string{
		"email":    strings.TrimSpace(email),
		"password": hashPassword(password),
	}

	jsonData, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", baseURLAuth+"/api/user/login", strings.NewReader(string(jsonData)))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Apikey", c.apiKey)
	req.Header.Set("Keyid", c.keyID)
	req.Header.Set("User-Agent", "go2rtc")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var errResp apiError
	_ = json.Unmarshal(body, &errResp)
	if errResp.hasError() {
		return fmt.Errorf("wyze: login failed (code %s): %s", errResp.code(), errResp.message())
	}

	var result loginResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("wyze: failed to parse login response: %w", err)
	}

	if len(result.MFAOptions) > 0 {
		return &AuthError{
			Message:  "MFA required",
			NeedsMFA: true,
			MFAType:  strings.Join(result.MFAOptions, ","),
		}
	}

	if result.AccessToken == "" {
		return errors.New("wyze: no access token in response")
	}

	c.accessToken = result.AccessToken

	return nil
}

func (c *Cloud) GetCameraList() ([]*Camera, error) {
	payload := map[string]any{
		"access_token":      c.accessToken,
		"phone_id":          c.phoneID,
		"app_name":          appName,
		"app_ver":           appName + "___" + appVersion,
		"app_version":       appVersion,
		"phone_system_type": 1,
		"sc":                "9f275790cab94a72bd206c8876429f3c",
		"sv":                "9d74946e652647e9b6c9d59326aef104",
		"ts":                time.Now().UnixMilli(),
	}

	jsonData, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", baseURLAPI+"/app/v2/home_page/get_object_list", strings.NewReader(string(jsonData)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result deviceListResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("wyze: failed to parse device list: %w", err)
	}

	if result.Code != "1" {
		return nil, fmt.Errorf("wyze: API error: %s - %s", result.Code, result.Msg)
	}

	c.cameras = nil
	for _, dev := range result.Data.DeviceList {
		if dev.ProductType != "Camera" {
			continue
		}
		c.cameras = append(c.cameras, &Camera{
			MAC:          dev.MAC,
			P2PID:        dev.DeviceParams.P2PID,
			ENR:          dev.ENR,
			IP:           dev.DeviceParams.IP,
			Nickname:     dev.Nickname,
			ProductModel: dev.ProductModel,
			ProductType:  dev.ProductType,
			DTLS:         dev.DeviceParams.DTLS,
			FirmwareVer:  dev.FirmwareVer,
			IsOnline:     dev.ConnState == 1,
		})
	}

	return c.cameras, nil
}

func (c *Cloud) GetCamera(id string) (*Camera, error) {
	if c.cameras == nil {
		if _, err := c.GetCameraList(); err != nil {
			return nil, err
		}
	}

	id = strings.ToUpper(id)
	for _, cam := range c.cameras {
		if strings.ToUpper(cam.MAC) == id || strings.EqualFold(cam.Nickname, id) {
			return cam, nil
		}
	}

	return nil, fmt.Errorf("wyze: camera not found: %s", id)
}

func (c *Cloud) GetP2PInfo(mac string) (map[string]any, error) {
	payload := map[string]any{
		"access_token":      c.accessToken,
		"phone_id":          c.phoneID,
		"device_mac":        mac,
		"app_name":          appName,
		"app_ver":           appName + "___" + appVersion,
		"app_version":       appVersion,
		"phone_system_type": 1,
		"sc":                "9f275790cab94a72bd206c8876429f3c",
		"sv":                "9d74946e652647e9b6c9d59326aef104",
		"ts":                time.Now().UnixMilli(),
	}

	jsonData, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", baseURLAPI+"/app/v2/device/get_iotc_info", strings.NewReader(string(jsonData)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result p2pInfoResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	if result.Code != "1" {
		return nil, fmt.Errorf("wyze: API error: %s - %s", result.Code, result.Msg)
	}

	return result.Data, nil
}

func (c *Cloud) GetDeviceInfo(mac, model string) (map[string]any, error) {
	payload := map[string]any{
		"access_token":      c.accessToken,
		"phone_id":          c.phoneID,
		"device_mac":        mac,
		"device_model":      model,
		"app_name":          appName,
		"app_ver":           appName + "___" + appVersion,
		"app_version":       appVersion,
		"phone_system_type": 1,
		"sc":                "a626948714654991afd3c0dbd7cdb901",
		"sv":                "c7c9ed9d307c4cbbaf7d6f95b2b4f2e9",
		"ts":                time.Now().UnixMilli(),
	}

	jsonData, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", baseURLAPI+"/app/v2/device/get_device_Info", strings.NewReader(string(jsonData)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result deviceInfoResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("wyze: failed to parse device info: %w", err)
	}
	if result.Code != "1" {
		return nil, fmt.Errorf("wyze: API error: %s - %s", result.Code, result.Msg)
	}

	return result.Data, nil
}

func (c *Cloud) GetGWellAccessCredential(mac string) (*GWellAccessCredential, error) {
	nonce := time.Now().UnixMilli()
	payload := map[string]any{
		"ttl_minutes": 10080,
		"nonce":       fmt.Sprintf("%d", nonce),
		"unique_id":   c.phoneID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", baseURLMars+"/plugin/mars/v2/regist_gw_user/"+url.PathEscape(mac), strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Content-Type", "application/json;charset=utf-8")
	req.Header.Set("User-Agent", "wyze_android_"+wpkAppVer)
	req.Header.Set("access_token", c.accessToken)
	req.Header.Set("requestid", requestID(nonce))
	req.Header.Set("appid", wpkAppID)
	req.Header.Set("appinfo", "wyze_android_"+wpkAppVer)
	req.Header.Set("phoneid", c.phoneID)
	req.Header.Set("signature2", dynamicSignature(c.accessToken, wpkSalt, body))

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result marsTokenResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("wyze: failed to parse Mars response: %w", err)
	}

	accessID, _ := result.Data["accessId"].(string)
	accessToken, _ := result.Data["accessToken"].(string)
	if accessID == "" || accessToken == "" {
		msg := result.Msg
		if msg == "" {
			msg = result.Message
		}
		return nil, fmt.Errorf("wyze: missing GWell access credentials (code=%v msg=%s keys=%v)", result.Code, msg, sortedKeys(result.Data))
	}

	return &GWellAccessCredential{AccessID: accessID, AccessToken: accessToken}, nil
}

func IsGWellCamera(cam *Camera) bool {
	if cam == nil {
		return false
	}
	if strings.HasPrefix(strings.ToUpper(cam.MAC), gwellPrefix) {
		return true
	}
	return cam.IP == "" && cam.DTLS == 0
}

func ResolveLANIP(deviceID string) string {
	mac := macFromDeviceID(deviceID)
	if mac == "" {
		return ""
	}

	out, err := exec.Command("arp", "-a").Output()
	if err != nil {
		return ""
	}

	needle := normalizeMAC(mac)
	for _, line := range strings.Split(string(out), "\n") {
		ip, seenMAC := parseARPLine(line)
		if ip == "" || seenMAC == "" {
			continue
		}
		if normalizeMAC(seenMAC) != needle {
			continue
		}
		if parsed := net.ParseIP(ip); parsed != nil {
			return parsed.String()
		}
	}

	return ""
}

type apiError struct {
	Code        string `json:"code"`
	ErrorCode   int    `json:"errorCode"`
	Msg         string `json:"msg"`
	Description string `json:"description"`
}

func (e *apiError) hasError() bool {
	if e.Code == "1" || e.Code == "0" {
		return false
	}
	if e.Code == "" && e.ErrorCode == 0 {
		return false
	}
	return e.Code != "" || e.ErrorCode != 0
}

func (e *apiError) message() string {
	if e.Msg != "" {
		return e.Msg
	}
	return e.Description
}

func (e *apiError) code() string {
	if e.Code != "" {
		return e.Code
	}
	return fmt.Sprintf("%d", e.ErrorCode)
}

type AuthError struct {
	Message  string `json:"message"`
	NeedsMFA bool   `json:"needs_mfa,omitempty"`
	MFAType  string `json:"mfa_type,omitempty"`
}

func (e *AuthError) Error() string {
	return e.Message
}

func generatePhoneID() string {
	return core.RandString(16, 16) // 16 hex chars
}

func hashPassword(password string) string {
	encoded := strings.TrimSpace(password)
	if strings.HasPrefix(strings.ToLower(encoded), "md5:") {
		return encoded[4:]
	}
	for range 3 {
		hash := md5.Sum([]byte(encoded))
		encoded = hex.EncodeToString(hash[:])
	}
	return encoded
}

func requestID(nonce int64) string {
	first := md5Hex(fmt.Sprintf("%d", nonce))
	return md5Hex(first)
}

func dynamicSignature(accessToken, salt string, body []byte) string {
	secret := md5Hex(accessToken + salt)
	h := hmac.New(md5.New, []byte(secret))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func sortedKeys(m map[string]any) []string {
	if m == nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func macFromDeviceID(deviceID string) string {
	parts := strings.Split(deviceID, "_")
	if len(parts) == 0 {
		return ""
	}
	raw := parts[len(parts)-1]
	if len(raw) != 12 {
		return ""
	}
	for _, ch := range raw {
		if !strings.ContainsRune("0123456789abcdefABCDEF", ch) {
			return ""
		}
	}
	return raw
}

var arpLine = regexp.MustCompile(`\(([^)]+)\) at ([0-9a-fA-F:]+) `)

func parseARPLine(line string) (ip string, mac string) {
	m := arpLine.FindStringSubmatch(line)
	if len(m) != 3 {
		return "", ""
	}
	return m[1], m[2]
}

func normalizeMAC(s string) string {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(s)), ":")
	for i, part := range parts {
		if len(part) == 1 {
			parts[i] = "0" + part
		}
	}
	return strings.Join(parts, "")
}
