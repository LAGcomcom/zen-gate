package remote

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The authorize URL is the vendor's protocol: if a parameter is missing or
// misspelled the login page loads and then refuses the grant, which looks like
// "the button does nothing" from the UI. So the URL is asserted field by field.
func TestDeviceAuthURLMatchesVendorProtocol(t *testing.T) {
	d := NewDeviceLogins()
	start, err := d.Start("cn")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if start.Realm != "cn" || start.State == "" {
		t.Fatalf("start payload = %+v", start)
	}
	parsed, err := url.Parse(start.AuthURL)
	if err != nil {
		t.Fatalf("auth url does not parse: %v", err)
	}
	if parsed.Host != "qoder.com.cn" || parsed.Path != "/device/selectAccounts" {
		t.Fatalf("auth url = %s, want https://qoder.com.cn/device/selectAccounts", start.AuthURL)
	}
	q := parsed.Query()
	if got := q.Get("challenge_method"); got != "S256" {
		t.Fatalf("challenge_method = %q", got)
	}
	if got := q.Get("client_id"); got != "1c5e33e1-364d-4ce6-b02c-acaa81274a5c" {
		t.Fatalf("client_id = %q", got)
	}
	if got := q.Get("redirect_uri"); got != "qoder-work-cn://" {
		t.Fatalf("redirect_uri = %q", got)
	}
	if got := q.Get("machine_id"); strings.TrimSpace(got) == "" {
		t.Fatal("machine_id must be sent or the page shows no accounts")
	}
	if got := q.Get("nonce"); !strings.Contains(got, "-") {
		t.Fatalf("cn nonce must be a dashed uuid, got %q", got)
	}
	// The challenge must be the S256 digest of the verifier we kept, never the
	// verifier itself: that is the whole point of PKCE.
	d.mu.Lock()
	pending := d.pending[start.State]
	d.mu.Unlock()
	if pending == nil {
		t.Fatal("start did not record a pending window")
	}
	sum := sha256.Sum256([]byte(pending.verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); q.Get("challenge") != want {
		t.Fatalf("challenge = %q, want %q", q.Get("challenge"), want)
	}
	if len(pending.verifier) < 43 || len(pending.verifier) > 128 {
		t.Fatalf("verifier length %d outside RFC 7636 range", len(pending.verifier))
	}
}

// The international region deliberately omits redirect_uri and uses an
// undashed nonce; getting this wrong is invisible until a user in that region
// tries to log in.
func TestDeviceAuthURLIntlOmitsRedirect(t *testing.T) {
	d := NewDeviceLogins()
	start, err := d.Start("intl")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	parsed, _ := url.Parse(start.AuthURL)
	if parsed.Host != "qoder.com" {
		t.Fatalf("host = %q", parsed.Host)
	}
	if parsed.Query().Get("redirect_uri") != "" {
		t.Fatal("intl authorize URL must not carry redirect_uri")
	}
	if strings.Contains(parsed.Query().Get("nonce"), "-") {
		t.Fatal("intl nonce must be 32 hex characters")
	}
}

func TestDeviceLoginRejectsUnknownRealm(t *testing.T) {
	d := NewDeviceLogins()
	if _, err := d.Start("mars"); err == nil {
		t.Fatal("unknown realm must be rejected, not silently defaulted")
	}
}

// The credential is synthesised, so the only way to trust it is to undo the
// synthesis: decrypt the cosy-key with the private half, then decrypt info with
// the recovered key, and compare the plaintext to the exact bytes the vendor's
// client sends.
func TestSynthCosyIdentityRoundTrips(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	cosyKey, info, err := synthCosyIdentityWithKey(&priv.PublicKey,
		"uid-42", "测试账号", "org-1", "示例组织", "personal_professional_trial", "dt-token", "drt-token")
	if err != nil {
		t.Fatalf("synth: %v", err)
	}
	sealed, err := base64.StdEncoding.DecodeString(cosyKey)
	if err != nil {
		t.Fatalf("cosy-key is not base64: %v", err)
	}
	if len(sealed) != priv.Size() {
		t.Fatalf("cosy-key length %d, want one RSA block (%d)", len(sealed), priv.Size())
	}
	tempKey, err := rsa.DecryptPKCS1v15(rand.Reader, priv, sealed)
	if err != nil {
		t.Fatalf("decrypt cosy-key: %v", err)
	}
	if len(tempKey) != 16 {
		t.Fatalf("temp key length %d, want 16 (AES-128)", len(tempKey))
	}
	ciphertext, err := base64.StdEncoding.DecodeString(info)
	if err != nil {
		t.Fatalf("info is not base64: %v", err)
	}
	plain, err := aesCBCDecryptPKCS7(ciphertext, tempKey)
	if err != nil {
		t.Fatalf("decrypt info: %v", err)
	}
	want := `{"aid":"uid-42","name":"测试账号","organization_id":"org-1",` +
		`"organization_name":"示例组织","refresh_token":"drt-token",` +
		`"security_oauth_token":"dt-token","uid":"uid-42",` +
		`"user_type":"personal_professional_trial","yx_uid":""}`
	if string(plain) != want {
		t.Fatalf("identity payload =\n%s\nwant\n%s", plain, want)
	}
}

// aesCBCDecryptPKCS7 is the inverse of aesCBCEncryptPKCS7, used by the round-trip
// test: the producer (server side) is not ours, so a test that only encrypts and
// compares ciphertext would prove nothing.
func aesCBCDecryptPKCS7(ciphertext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext)%aes.BlockSize != 0 || len(ciphertext) == 0 {
		return nil, errBadBlock
	}
	out := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, key).CryptBlocks(out, ciphertext)
	pad := int(out[len(out)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(out) {
		return nil, errBadBlock
	}
	return out[:len(out)-pad], nil
}

var errBadBlock = &cipherBlockError{}

type cipherBlockError struct{}

func (*cipherBlockError) Error() string { return "bad cipher block" }

// deviceStub serves the two endpoints a poll touches. The grant endpoint
// answers "not yet" until notBefore polls have happened, which is exactly how
// the real one behaves (404 → 200).
func deviceStub(t *testing.T, notBefore int32) (*httptest.Server, *int32) {
	t.Helper()
	var polls int32
	mux := http.NewServeMux()
	mux.HandleFunc(devicePollPath, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&polls, 1)
		if r.URL.Query().Get("verifier") == "" || r.URL.Query().Get("nonce") == "" {
			t.Errorf("poll without nonce/verifier: %s", r.URL.RawQuery)
		}
		if n < notBefore {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"token":"dt-stub","refresh_token":"drt-stub"}`))
	})
	mux.HandleFunc(deviceUserInfoPath, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer dt-stub" {
			t.Errorf("userinfo Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"id":"uid-9","name":"备用账号","user_type":"personal_professional_trial"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &polls
}

func TestDevicePollPendingThenAuthorized(t *testing.T) {
	srv, _ := deviceStub(t, 2)
	d := NewDeviceLogins()
	d.OpenAPI = srv.URL
	start, err := d.Start("cn")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	first, err := d.Poll(start.State)
	if err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if first.Status != DevicePending {
		t.Fatalf("first poll status = %q, want pending (404 means not yet)", first.Status)
	}
	if first.Cred != nil {
		t.Fatal("a pending poll must not hand back a credential")
	}
	second, err := d.Poll(start.State)
	if err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if second.Status != DeviceOK {
		t.Fatalf("second poll status = %q (%s)", second.Status, second.Message)
	}
	if second.Cred == nil {
		t.Fatal("an authorized poll must carry the credential")
	}
	cred := *second.Cred
	if cred.UserID != "uid-9" || cred.AccessToken != "dt-stub" || cred.RefreshToken != "drt-stub" {
		t.Fatalf("credential = %+v", cred)
	}
	if cred.Source != "device:cn" {
		t.Fatalf("source = %q", cred.Source)
	}
	if err := validateCredential(cred); err != nil {
		t.Fatalf("synthesised credential is not usable: %v", err)
	}
	if base64Raw, err := base64.StdEncoding.DecodeString(cred.CosyKey); err != nil || len(base64Raw) != 128 {
		t.Fatalf("cosy-key should be one 1024-bit RSA block, got %d bytes (err %v)", len(base64Raw), err)
	}
	if second.Label != "备用账号" {
		t.Fatalf("label = %q, want the account name", second.Label)
	}
	days := time.Until(credExpiry(cred)).Hours() / 24
	if days < 29 || days > 31 {
		t.Fatalf("default lifetime = %.1f days, want ~30", days)
	}
	if _, ok := d.pending[start.State]; ok {
		t.Fatal("a finished window must be closed so a stale tab cannot re-import")
	}
}

func credExpiry(cred Credential) time.Time { return time.UnixMilli(cred.TokenExpireTime) }

func TestDevicePollForgetsWindowAfterGrant(t *testing.T) {
	srv, _ := deviceStub(t, 1)
	d := NewDeviceLogins()
	d.OpenAPI = srv.URL
	start, _ := d.Start("cn")
	if res, _ := d.Poll(start.State); res.Status != DeviceOK {
		t.Fatalf("poll = %+v", res)
	}
	if res, _ := d.Poll(start.State); res.Status != DeviceUnknown {
		t.Fatalf("second poll after success = %q, want unknown", res.Status)
	}
}

func TestDevicePollExpiredWindow(t *testing.T) {
	d := NewDeviceLogins()
	d.TTL = time.Nanosecond
	start, _ := d.Start("cn")
	time.Sleep(2 * time.Millisecond)
	res, err := d.Poll(start.State)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if res.Status != DeviceExpired {
		t.Fatalf("status = %q, want expired", res.Status)
	}
}

func TestDevicePollUnknownState(t *testing.T) {
	d := NewDeviceLogins()
	res, err := d.Poll("qd-nope")
	if err != nil {
		t.Fatalf("unknown state must not error: %v", err)
	}
	if res.Status != DeviceUnknown || res.Message == "" {
		t.Fatalf("result = %+v", res)
	}
}

// A non-JSON 200 is the endpoint's "user is mid-login" shape: treating it as a
// failure would close a window the browser is still using.
func TestDevicePollNonJSONStaysPending(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>please wait</html>`))
	}))
	t.Cleanup(srv.Close)
	d := NewDeviceLogins()
	d.OpenAPI = srv.URL
	start, _ := d.Start("cn")
	res, err := d.Poll(start.State)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if res.Status != DevicePending {
		t.Fatalf("status = %q, want pending", res.Status)
	}
}

// deviceRealmTable keeps the shipped constants honest: a missing client id or
// redirect makes the authorize page reject the grant server-side.
func TestDeviceRealmTable(t *testing.T) {
	for name, cfg := range deviceRealms {
		if !strings.HasPrefix(cfg.OpenAPI, "https://") || !strings.HasPrefix(cfg.Website, "https://") {
			t.Fatalf("%s: hosts must be https, got %q/%q", name, cfg.OpenAPI, cfg.Website)
		}
		if cfg.WithClient && cfg.ClientID == "" {
			t.Fatalf("%s: send_client_id without a client id", name)
		}
		if cfg.WithRedir && cfg.Redirect == "" {
			t.Fatalf("%s: send_redirect_uri without a redirect", name)
		}
	}
	if _, ok := deviceRealms["cn"]; !ok {
		t.Fatal("cn realm missing; every default path assumes it")
	}
}

// jsonString must not escape "<"/">"/"&": the vendor's client does not, and the
// server decrypts info and reads it as JSON without re-escaping.
func TestCosyIdentityJSONDoesNotEscapeHTML(t *testing.T) {
	var payload map[string]any
	if err := json.Unmarshal(cosyIdentityJSON("u", "a<b&c", "", "", "t", "tok", ""), &payload); err != nil {
		t.Fatalf("identity json does not parse: %v", err)
	}
	if payload["name"] != "a<b&c" {
		t.Fatalf("name round-trip = %q", payload["name"])
	}
	if strings.Contains(string(cosyIdentityJSON("u", "a<b", "", "", "t", "tok", "")), `\u003c`) {
		t.Fatal("identity json must not HTML-escape: the server compares raw bytes")
	}
}
