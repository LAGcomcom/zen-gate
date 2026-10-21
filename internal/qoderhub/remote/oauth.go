package remote

// Qoder device-authorization login.
//
// Why this exists: the pool used to grow one way only — snapshot whatever the
// IDE holds. That makes "a second account" mean "log out of the IDE, log in as
// somebody else, come back and import", which is slow and throws away the first
// account's session on the way.
//
// The vendor's desktop client logs in through a PKCE device grant that a plain
// browser can complete, so we build the same authorize URL and let the user do
// the login: the dashboard opens a tab, the user authenticates on qoder.com.cn,
// and the grant endpoint hands us the token. Nothing here types a password.
//
// The one non-obvious part is that a Qoder credential is not a bearer token.
// Chat requests sign with Cosy-Key + info, where the client mints a random
// 16-byte temp key, sends RSA(temp key) as cosy-key, and sends the account
// identity AES-128-CBC encrypted under that same temp key. The server recovers
// the temp key with its private key and decrypts info. So a fresh grant is
// enough to synthesise a complete credential: no vendor wasm, no IDE, no
// machine-bound secret.

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	devicePollPath     = "/api/v1/deviceToken/poll"
	deviceUserInfoPath = "/api/v1/userinfo"

	// defaultDeviceLoginTTL bounds one browser round-trip. Ten minutes is the
	// window the vendor grants: a stale window must not be able to absorb a
	// login that starts later.
	defaultDeviceLoginTTL = 10 * time.Minute
	// deviceTokenLifetime is what a grant buys when the response does not say.
	// The vendor issues dt- tokens for a month.
	deviceTokenLifetime = 30 * 24 * time.Hour

	// defaultDeviceUserType is what the vendor reports for a trial account. The
	// identity payload has to carry something: an empty user_type changes the
	// bytes the server decrypts and compares.
	defaultDeviceUserType = "personal_professional_trial"

	// devicePollUserAgent is what the official client sends to the grant
	// endpoint. It is not the chat user agent: the openapi host distinguishes
	// the two surfaces.
	devicePollUserAgent = "QoderWork"
)

// Device status vocabulary. The card renders these strings directly, so they
// are stable names, not prose.
const (
	DevicePending = "pending"
	DeviceOK      = "ok"
	DeviceExpired = "expired"
	DeviceUnknown = "unknown"
	DeviceFailed  = "failed"
)

// deviceRealm is one vendor region. The two regions differ in more than hosts:
// the authorize URL carries a different parameter set, which is why this table
// is data and not a switch.
type deviceRealm struct {
	Name       string
	OpenAPI    string
	Website    string
	ClientID   string
	Redirect   string
	UserAgent  string
	HomeDir    string
	WithClient bool
	WithRedir  bool
	DashedNonc bool
}

var deviceRealms = map[string]deviceRealm{
	"cn": {
		Name:       "国内版",
		OpenAPI:    "https://openapi.qoder.com.cn",
		Website:    "https://qoder.com.cn",
		ClientID:   "1c5e33e1-364d-4ce6-b02c-acaa81274a5c",
		Redirect:   "qoder-work-cn://",
		UserAgent:  "QoderWork/1.1.64",
		HomeDir:    ".qoder-cn",
		WithClient: true,
		WithRedir:  true,
		DashedNonc: true,
	},
	"intl": {
		Name:       "国际版",
		OpenAPI:    "https://openapi.qoder.sh",
		Website:    "https://qoder.com",
		ClientID:   "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb",
		Redirect:   "qoder://aicoding.aicoding-agent/login-success",
		UserAgent:  "Qoder/1.1.64",
		HomeDir:    ".qoder",
		WithClient: true,
		WithRedir:  false,
		DashedNonc: false,
	},
}

// DeviceLogin is one open authorization window: the state to poll with and the
// URL to open.
type DeviceLogin struct {
	State   string `json:"state"`
	AuthURL string `json:"authUrl"`
	Realm   string `json:"realm"`
}

// DeviceResult is one poll's answer. Cred is only set when Status is DeviceOK;
// it is not serialised because it carries the account's secrets.
type DeviceResult struct {
	Status    string `json:"status"`
	Message   string `json:"message,omitempty"`
	UID       string `json:"uid,omitempty"`
	Label     string `json:"label,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`
	Cred      *Credential
}

type pendingDeviceLogin struct {
	created  time.Time
	verifier string
	nonce    string
	realm    string
}

// DeviceLogins owns the pending authorization windows. One process-wide
// instance is enough: a login is a short-lived handshake, and a restart should
// forget it rather than resume a tab the user already closed.
type DeviceLogins struct {
	mu      sync.Mutex
	pending map[string]*pendingDeviceLogin

	// OpenAPI/Website override the realm table. Production leaves them empty;
	// a test points them at a stub so the flow can be exercised end to end.
	OpenAPI string
	Website string
	// Client is the outbound client for poll + userinfo. nil means a client
	// with a short timeout, since both calls are cheap and user-facing.
	Client *http.Client
	// TTL overrides defaultDeviceLoginTTL (tests use a negative window).
	TTL time.Duration
}

// NewDeviceLogins builds an empty login registry.
func NewDeviceLogins() *DeviceLogins {
	return &DeviceLogins{pending: map[string]*pendingDeviceLogin{}}
}

func (d *DeviceLogins) ttl() time.Duration {
	if d.TTL > 0 {
		return d.TTL
	}
	return defaultDeviceLoginTTL
}

func (d *DeviceLogins) client() *http.Client {
	if d.Client != nil {
		return d.Client
	}
	return &http.Client{Timeout: 25 * time.Second}
}

// realmConfig resolves one realm name, defaulting to the domestic region: the
// whole gateway is built for qodercn, so "unspecified" must mean cn.
func (d *DeviceLogins) realmConfig(name string) (deviceRealm, string, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		key = "cn"
	}
	cfg, ok := deviceRealms[key]
	if !ok {
		return deviceRealm{}, "", fmt.Errorf("未知的 Qoder 区域：%s（可用 cn / intl）", name)
	}
	if v := strings.TrimRight(strings.TrimSpace(d.OpenAPI), "/"); v != "" {
		cfg.OpenAPI = v
	}
	if v := strings.TrimRight(strings.TrimSpace(d.Website), "/"); v != "" {
		cfg.Website = v
	}
	return cfg, key, nil
}

// Start opens one authorization window and returns the URL the dashboard must
// open. The verifier never leaves this process: only its SHA-256 goes to the
// browser, which is the entire point of PKCE.
func (d *DeviceLogins) Start(realm string) (DeviceLogin, error) {
	cfg, key, err := d.realmConfig(realm)
	if err != nil {
		return DeviceLogin{}, err
	}
	verifier, err := pkceVerifier()
	if err != nil {
		return DeviceLogin{}, err
	}
	challenge := pkceChallenge(verifier)
	nonce := newHexID()
	if cfg.DashedNonc {
		nonce = newUUID()
	}
	q := url.Values{}
	q.Set("challenge", challenge)
	q.Set("challenge_method", "S256")
	q.Set("nonce", nonce)
	if cfg.WithRedir {
		q.Set("redirect_uri", cfg.Redirect)
	}
	if cfg.WithClient {
		q.Set("client_id", cfg.ClientID)
		q.Set("machine_id", localDeviceMachineID(cfg.HomeDir))
	}
	state := "qd-" + newHexID()
	d.mu.Lock()
	if d.pending == nil {
		d.pending = map[string]*pendingDeviceLogin{}
	}
	d.pending[state] = &pendingDeviceLogin{
		created:  time.Now(),
		verifier: verifier,
		nonce:    nonce,
		realm:    key,
	}
	d.mu.Unlock()
	return DeviceLogin{
		State:   state,
		AuthURL: cfg.Website + "/device/selectAccounts?" + q.Encode(),
		Realm:   key,
	}, nil
}

// Pending reports how many windows are open (the card shows it as feedback).
func (d *DeviceLogins) Pending() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.pending)
}

// Cancel forgets one window.
func (d *DeviceLogins) Cancel(state string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.pending[strings.TrimSpace(state)]
	delete(d.pending, strings.TrimSpace(state))
	return ok
}

// Poll asks the grant endpoint whether the browser finished. A 404 or 202 is
// the documented "not yet" answer — not an error — and every other failure is
// reported as a message instead of a Go error so a transient hiccup leaves the
// window open for the next poll.
func (d *DeviceLogins) Poll(state string) (DeviceResult, error) {
	state = strings.TrimSpace(state)
	d.mu.Lock()
	info, ok := d.pending[state]
	d.mu.Unlock()
	if !ok {
		return DeviceResult{Status: DeviceUnknown, Message: "这次授权已失效，请重新点「授权登录」"}, nil
	}
	if time.Since(info.created) > d.ttl() {
		d.Cancel(state)
		return DeviceResult{Status: DeviceExpired, Message: "授权窗口已过期（10 分钟），请重新发起"}, nil
	}
	cfg, _, err := d.realmConfig(info.realm)
	if err != nil {
		return DeviceResult{}, err
	}
	q := url.Values{}
	q.Set("challenge_method", "S256")
	q.Set("nonce", info.nonce)
	q.Set("verifier", info.verifier)

	req, err := http.NewRequest(http.MethodGet, cfg.OpenAPI+devicePollPath+"?"+q.Encode(), nil)
	if err != nil {
		return DeviceResult{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", devicePollUserAgent)
	resp, err := d.client().Do(req)
	if err != nil {
		return DeviceResult{Status: DevicePending, Message: "等待浏览器完成授权（网络抖动，继续轮询）"}, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusAccepted {
		return DeviceResult{Status: DevicePending, Message: "等待浏览器完成 Qoder 授权"}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return DeviceResult{
			Status:  DeviceFailed,
			Message: fmt.Sprintf("授权轮询返回 HTTP %d：%s", resp.StatusCode, truncateNote(string(body))),
		}, nil
	}
	var payload struct {
		Token        string `json:"token"`
		DeviceToken  string `json:"device_token"`
		RefreshToken string `json:"refresh_token"`
		UserID       string `json:"user_id"`
		ExpiresInMS  int64  `json:"expires_in"`
		ExpiresAt    string `json:"expires_at"`
	}
	if json.Unmarshal(body, &payload) != nil {
		// A grant that is not JSON yet is still pending: the endpoint answers
		// 200 with an empty object while the user is mid-login.
		return DeviceResult{Status: DevicePending, Message: "等待浏览器完成 Qoder 授权"}, nil
	}
	token := strings.TrimSpace(payload.Token)
	if token == "" {
		token = strings.TrimSpace(payload.DeviceToken)
	}
	if token == "" {
		return DeviceResult{Status: DevicePending, Message: "等待浏览器完成 Qoder 授权"}, nil
	}

	uid := strings.TrimSpace(payload.UserID)
	name, userType, orgID, orgName := "", defaultDeviceUserType, "", ""
	if info, err := d.fetchIdentity(cfg, token); err == nil {
		if v := strings.TrimSpace(info.ID); v != "" {
			uid = v
		}
		name = strings.TrimSpace(info.Name)
		if v := strings.TrimSpace(info.UserType); v != "" {
			userType = v
		}
		orgID = strings.TrimSpace(info.OrganizationID)
		orgName = strings.TrimSpace(info.OrganizationName)
	}
	if uid == "" {
		uid = "q-" + strings.ReplaceAll(newUUID(), "-", "")[:24]
	}

	cosyKey, infoValue, err := synthCosyIdentity(uid, name, orgID, orgName, userType, token, payload.RefreshToken)
	if err != nil {
		return DeviceResult{}, err
	}
	expiry := deviceExpiry(payload.ExpiresInMS, payload.ExpiresAt)
	cred := Credential{
		CosyKey:         cosyKey,
		EncryptUserInfo: infoValue,
		UserID:          uid,
		MachineID:       localDeviceMachineID(cfg.HomeDir),
		AccessToken:     token,
		RefreshToken:    payload.RefreshToken,
		Source:          "device:" + info.realm,
		TokenExpireTime: expiry.UnixMilli(),
	}
	d.Cancel(state)

	label := name
	if label == "" {
		label = "Qoder 账号 " + shortUID(uid)
	}
	return DeviceResult{
		Status:    DeviceOK,
		UID:       uid,
		Label:     label,
		ExpiresAt: expiry.Format(time.RFC3339),
		Cred:      &cred,
	}, nil
}

// identity is the subset of /api/v1/userinfo the credential needs.
type identity struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	UserType         string `json:"user_type"`
	OrganizationID   string `json:"organization_id"`
	OrganizationName string `json:"organization_name"`
}

// fetchIdentity reads the display identity behind a fresh token. Every field is
// cosmetic except the id: a failure here must not lose the grant.
func (d *DeviceLogins) fetchIdentity(cfg deviceRealm, token string) (identity, error) {
	req, err := http.NewRequest(http.MethodGet, cfg.OpenAPI+deviceUserInfoPath, nil)
	if err != nil {
		return identity{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", cfg.UserAgent)
	resp, err := d.client().Do(req)
	if err != nil {
		return identity{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return identity{}, fmt.Errorf("userinfo status %d", resp.StatusCode)
	}
	var out identity
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return identity{}, err
	}
	return out, nil
}

// deviceExpiry reads the grant's expiry. expires_in is milliseconds (that is
// what this endpoint sends); expires_at is RFC3339 when present.
func deviceExpiry(expiresInMS int64, expiresAt string) time.Time {
	now := time.Now()
	if expiresInMS > 0 {
		return now.Add(time.Duration(expiresInMS) * time.Millisecond)
	}
	if raw := strings.TrimSpace(expiresAt); raw != "" {
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			return parsed
		}
	}
	return now.Add(deviceTokenLifetime)
}

// localDeviceMachineID keeps the device identity stable across logins: if the
// vendor client already stored one, reuse it, otherwise mint a UUID once per
// process. A machine id is not a secret, but a changing one makes the account
// look like a new device on every login.
var (
	deviceMachineMu   sync.Mutex
	deviceMachineSeen string
)

func localDeviceMachineID(homeDir string) string {
	deviceMachineMu.Lock()
	defer deviceMachineMu.Unlock()
	if deviceMachineSeen != "" {
		return deviceMachineSeen
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		for _, rel := range []string{
			filepath.Join(homeDir, ".auth", "machine_id"),
			filepath.Join(homeDir, "machine_id"),
		} {
			if raw, err := os.ReadFile(filepath.Join(home, rel)); err == nil {
				if value := strings.TrimSpace(string(raw)); value != "" {
					deviceMachineSeen = value
					return value
				}
			}
		}
	}
	deviceMachineSeen = newUUID()
	return deviceMachineSeen
}

// pkceVerifier mints an RFC 7636 verifier: 64 characters from the unreserved
// set, which is inside the 43..128 range the grant endpoint accepts.
func pkceVerifier() (string, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	raw := make([]byte, 64)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	out := make([]byte, len(raw))
	for i, b := range raw {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out), nil
}

// pkceChallenge is base64url(SHA-256(verifier)) without padding, per S256.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// qoderCosyPublicKey is the vendor's RSA key: the client encrypts its temp key
// with this, the server decrypts with the matching private key. It ships inside
// the official client, so it is a protocol constant, not a secret.
const qoderCosyPublicKey = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

// cosyPublicKey parses the embedded vendor key.
func cosyPublicKey() (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(qoderCosyPublicKey))
	if block == nil {
		return nil, errors.New("Qoder cosy public key: no PEM block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("Qoder cosy public key: %w", err)
	}
	pub, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("Qoder cosy public key is not RSA")
	}
	return pub, nil
}

// synthCosyIdentity builds the cosy-key + info pair for one account.
func synthCosyIdentity(uid, name, orgID, orgName, userType, accessToken, refreshToken string) (string, string, error) {
	pub, err := cosyPublicKey()
	if err != nil {
		return "", "", err
	}
	return synthCosyIdentityWithKey(pub, uid, name, orgID, orgName, userType, accessToken, refreshToken)
}

// synthCosyIdentityWithKey is synthCosyIdentity with the key injected so a test
// can use a throwaway pair and decrypt what it produced.
func synthCosyIdentityWithKey(pub *rsa.PublicKey, uid, name, orgID, orgName, userType, accessToken, refreshToken string) (string, string, error) {
	tempKey, err := tempCosyKey()
	if err != nil {
		return "", "", err
	}
	sealed, err := rsa.EncryptPKCS1v15(rand.Reader, pub, tempKey)
	if err != nil {
		return "", "", fmt.Errorf("encrypt cosy temp key: %w", err)
	}
	plain := cosyIdentityJSON(uid, name, orgID, orgName, userType, accessToken, refreshToken)
	ciphertext, err := aesCBCEncryptPKCS7(plain, tempKey, tempKey)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(sealed), base64.StdEncoding.EncodeToString(ciphertext), nil
}

// tempCosyKey mints the 16-byte AES-128 key. ASCII letters/digits only: the
// official client sends a printable key and the server's AES implementation is
// byte-oriented, so this keeps the ciphertext identical in shape.
func tempCosyKey() ([]byte, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	out := make([]byte, len(raw))
	for i, b := range raw {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return out, nil
}

// cosyIdentityKeys lists the identity fields in the order the vendor's client
// serialises them. JSON object order is irrelevant to a parser but not to a
// signature, so this slice is the single source of that order.
var cosyIdentityKeys = []string{
	"aid", "name", "organization_id", "organization_name",
	"refresh_token", "security_oauth_token", "uid", "user_type", "yx_uid",
}

// cosyIdentityJSON serialises the identity payload: compact JSON with keys in
// sorted order and no HTML escaping, which is what the client produces and what
// the server's signature covers.
func cosyIdentityJSON(uid, name, orgID, orgName, userType, accessToken, refreshToken string) []byte {
	values := map[string]string{
		"aid":                  uid,
		"name":                 name,
		"organization_id":      orgID,
		"organization_name":    orgName,
		"refresh_token":        refreshToken,
		"security_oauth_token": accessToken,
		"uid":                  uid,
		"user_type":            userType,
		"yx_uid":               "",
	}
	var b bytes.Buffer
	b.WriteByte('{')
	for i, key := range cosyIdentityKeys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(jsonString(key))
		b.WriteByte(':')
		b.WriteString(jsonString(values[key]))
	}
	b.WriteByte('}')
	return b.Bytes()
}

// jsonString quotes one JSON string without HTML escaping.
func jsonString(value string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return `""`
	}
	return strings.TrimRight(buf.String(), "\n")
}

// aesCBCEncryptPKCS7 encrypts with AES-CBC and PKCS#7 padding; the vendor uses
// key == iv for this field.
func aesCBCEncryptPKCS7(plain, key, iv []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	padded := make([]byte, len(plain)+pad)
	copy(padded, plain)
	for i := 0; i < pad; i++ {
		padded[len(plain)+i] = byte(pad)
	}
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return out, nil
}
