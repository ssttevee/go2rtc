package wyze

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gwelllib "github.com/wlatic/wyze-gwell-bridge/wyze-p2p/pkg/gwell"
)

const gwellCredentialRefreshAge = 6 * 24 * time.Hour

type AccountCredentials struct {
	APIKey   string
	APIID    string
	Password string
}

type GWellCredentialCache struct {
	AccessID    string    `json:"access_id"`
	AccessToken string    `json:"access_token"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type GWellSessionCache struct {
	ServerAddr string                `json:"server_addr"`
	Devices    []gwelllib.DeviceInfo `json:"devices"`
	UpdatedAt  time.Time             `json:"updated_at"`
}

var gwellAccountProvider struct {
	mu sync.RWMutex
	fn func(account string) (*AccountCredentials, error)
}

func SetGWellAccountProvider(fn func(account string) (*AccountCredentials, error)) {
	gwellAccountProvider.mu.Lock()
	gwellAccountProvider.fn = fn
	gwellAccountProvider.mu.Unlock()
}

func resolveGWellAccessCredentials(rawURL string, forceRefresh bool) (accessID string, accessToken string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", fmt.Errorf("wyze/gwell: invalid URL: %w", err)
	}
	return resolveGWellAccessCredentialsFromURL(u, forceRefresh)
}

func resolveGWellAccessCredentialsFromURL(u *url.URL, forceRefresh bool) (accessID string, accessToken string, err error) {
	query := u.Query()
	account := query.Get("account")
	mac := query.Get("mac")

	var cached *GWellCredentialCache
	if account != "" && mac != "" {
		cached, _ = loadGWellCredentialCache(account, mac)
		if !forceRefresh && cached != nil && cached.AccessID != "" && cached.AccessToken != "" && !gwellCredentialNeedsRefresh(cached) {
			return cached.AccessID, cached.AccessToken, nil
		}

		fresh, refreshErr := refreshGWellAccessCredentials(account, mac)
		if refreshErr == nil {
			if err = saveGWellCredentialCache(account, mac, fresh); err != nil {
				return "", "", err
			}
			return fresh.AccessID, fresh.AccessToken, nil
		}

		if !forceRefresh && cached != nil && cached.AccessID != "" && cached.AccessToken != "" {
			return cached.AccessID, cached.AccessToken, nil
		}

		return "", "", refreshErr
	}

	return "", "", fmt.Errorf("wyze/gwell: account and mac are required for credential refresh")
}

func refreshGWellAccessCredentials(account, mac string) (*GWellCredentialCache, error) {
	provider := getGWellAccountProvider()
	if provider == nil {
		return nil, fmt.Errorf("wyze/gwell: account provider not configured")
	}

	creds, err := provider(account)
	if err != nil {
		return nil, err
	}
	if creds == nil || creds.APIKey == "" || creds.APIID == "" || creds.Password == "" {
		return nil, fmt.Errorf("wyze/gwell: incomplete account credentials for %s", account)
	}

	cloud := NewCloud(creds.APIKey, creds.APIID)
	if err = cloud.Login(account, creds.Password); err != nil {
		return nil, err
	}

	access, err := cloud.GetGWellAccessCredential(mac)
	if err != nil {
		return nil, err
	}

	return &GWellCredentialCache{
		AccessID:    access.AccessID,
		AccessToken: access.AccessToken,
		UpdatedAt:   time.Now().UTC(),
	}, nil
}

func getGWellAccountProvider() func(account string) (*AccountCredentials, error) {
	gwellAccountProvider.mu.RLock()
	fn := gwellAccountProvider.fn
	gwellAccountProvider.mu.RUnlock()
	return fn
}

func gwellCredentialNeedsRefresh(cache *GWellCredentialCache) bool {
	if cache == nil || cache.AccessID == "" || cache.AccessToken == "" {
		return true
	}
	if cache.UpdatedAt.IsZero() {
		return true
	}
	return time.Since(cache.UpdatedAt) >= gwellCredentialRefreshAge
}

func loadGWellCredentialCache(account, mac string) (*GWellCredentialCache, error) {
	path, err := gwellCachePath(account, mac, "credentials.json")
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cache GWellCredentialCache
	if err = json.Unmarshal(b, &cache); err != nil {
		return nil, fmt.Errorf("wyze/gwell: decode cache %s: %w", path, err)
	}
	return &cache, nil
}

func saveGWellCredentialCache(account, mac string, cache *GWellCredentialCache) error {
	path, err := gwellCachePath(account, mac, "credentials.json")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("wyze/gwell: create cache dir: %w", err)
	}
	b, err := json.Marshal(cache)
	if err != nil {
		return fmt.Errorf("wyze/gwell: encode cache: %w", err)
	}
	if err = os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("wyze/gwell: write cache %s: %w", path, err)
	}
	return nil
}

func loadGWellSessionCache(account, mac string) (*GWellSessionCache, error) {
	path, err := gwellCachePath(account, mac, "session.json")
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cache GWellSessionCache
	if err = json.Unmarshal(b, &cache); err != nil {
		return nil, fmt.Errorf("wyze/gwell: decode session cache %s: %w", path, err)
	}
	if cache.ServerAddr == "" || len(cache.Devices) == 0 {
		return nil, fmt.Errorf("wyze/gwell: incomplete session cache")
	}
	return &cache, nil
}

func saveGWellSessionCache(account, mac string, cache *GWellSessionCache) error {
	path, err := gwellCachePath(account, mac, "session.json")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("wyze/gwell: create cache dir: %w", err)
	}
	b, err := json.Marshal(cache)
	if err != nil {
		return fmt.Errorf("wyze/gwell: encode session cache: %w", err)
	}
	if err = os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("wyze/gwell: write session cache %s: %w", path, err)
	}
	return nil
}

func deleteGWellSessionCache(account, mac string) error {
	path, err := gwellCachePath(account, mac, "session.json")
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("wyze/gwell: delete session cache %s: %w", path, err)
	}
	return nil
}

func gwellCachePath(account, mac, file string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("wyze/gwell: locate home dir: %w", err)
	}
	return filepath.Join(home, ".cache", "go2rtc", "wyze", "gwell", sanitizeCachePathSegment(account), sanitizeCachePathSegment(mac), file), nil
}

func sanitizeCachePathSegment(s string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_")
	return replacer.Replace(s)
}
