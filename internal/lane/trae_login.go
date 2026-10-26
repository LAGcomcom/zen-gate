package lane

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// TRAE native login.
//
// The desktop client's login is a browser handoff to the console, which lands on
// a LOCAL callback the app is listening on:
//
//	open  {consoleHost}/authorization?<18 params, mouth of them auth_callback_url>
//	→ the console redirects to  http://127.0.0.1:PORT/authorize?refreshToken=…&userInfo={…}&userJwt={…}
//
// So this gateway can own the whole flow by binding that port itself. Four
// details decide whether it works at all, and each one is a silent failure when
// wrong:
//
//   - the callback parameter is `auth_callback_url`, not `callback_url`. The
//     console ignores an unknown parameter and the browser simply never comes
//     back — no error, just a login that hangs;
//   - `auth_from=solo`, `login_channel=native_ide`, `auth_type=local` and
//     `redirect=0` together select the handoff that returns tokens directly.
//     Changing one lands on a different login surface entirely;
//   - `machine_id` and `device_id` must be STABLE and distinct per account: a
//     shared device id makes the daily checkin answer "this device already
//     claimed today", and a regenerated machine id makes the account look like a
//     new device on every restart;
//   - the listener must register its error handler BEFORE Serve, or an
//     EADDRINUSE surfaces as an unhandled process-level error.
//
// The tokens arrive in the callback, so there is no exchange step: the callback
// IS the credential.

const (
	traeCallbackPort  = 18080
	traeLoginTTL      = 10 * time.Minute
	traeConsoleHost   = "https://www.trae.cn"
	traeLoginPath     = "/authorization"
	traeCallbackPath  = "/authorize"
	traePluginVersion = "2.3.62834"
	traeClientIDLogin = "en1oxy7wnw8j9n"
)

// traePendingLogin is one in-flight browser handoff.
type traePendingLogin struct {
	mu      sync.Mutex
	port    int
	server  *http.Server
	done    chan struct{}
	acct    TraeAccount
	result  string // "ok" once a token landed
	err     string
	startAt time.Time
}

var (
	traeLoginMu  sync.Mutex
	traeLoginCur *traePendingLogin
)

// traeNewDeviceID mints a per-account device fingerprint: 32 hex chars, as the
// client generates (openssl rand -hex 16).
func traeNewDeviceID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// TraeLoginStart binds the callback port, opens the browser handoff, and returns
// the URL the user must open. The listener is live before the URL is returned —
// the browser can arrive at any moment after that.
func (l *Lane) TraeLoginStart() (string, error) {
	l.traeLoginClose() // one flow at a time

	machineID := traeNewDeviceID()
	deviceID := traeNewDeviceID()

	// Port 18080 is the console's expected one; when it is taken, fall back to
	// an ephemeral port and tell the console about it in auth_callback_url.
	port := traeCallbackPort
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", fmt.Errorf("无法监听本地回调端口：%v", err)
		}
		port = ln.Addr().(*net.TCPAddr).Port
	}

	pend := &traePendingLogin{
		port:    port,
		done:    make(chan struct{}),
		startAt: time.Now(),
	}
	pend.server = &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			pend.handleCallback(w, r)
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// The error handler goes on BEFORE Serve: an EADDRINUSE at this point must
	// surface as a returned error, not as a process-level panic.
	errc := make(chan error, 1)
	pend.server.ErrorLog = nil
	go func() {
		if serveErr := pend.server.Serve(ln); serveErr != nil && serveErr != http.ErrServerClosed {
			errc <- serveErr
		}
	}()
	select {
	case serveErr := <-errc:
		_ = ln.Close()
		return "", fmt.Errorf("回调服务启动失败：%v", serveErr)
	case <-time.After(300 * time.Millisecond):
		// Serving; the handoff URL can go out.
	}

	traeLoginMu.Lock()
	traeLoginCur = pend
	traeLoginMu.Unlock()

	ts := time.Now().UnixMilli()
	q := url.Values{}
	q.Set("login_version", "1")
	q.Set("auth_from", "solo")
	q.Set("login_channel", "native_ide")
	q.Set("plugin_version", traePluginVersion)
	q.Set("auth_type", "local")
	q.Set("client_id", traeClientIDLogin)
	q.Set("redirect", "0")
	q.Set("login_trace_id", traeNewDeviceID())
	q.Set("auth_callback_url", fmt.Sprintf("http://127.0.0.1:%d%s", port, traeCallbackPath))
	q.Set("machine_id", machineID)
	q.Set("device_id", deviceID)
	// The console's own login path sends the device identity twice: once as the
	// snake_case pair above and once with the x_ prefix the desktop client uses.
	// Omitting the prefixed pair is not fatal today (the console falls back to
	// the unprefixed one), but it is what the vendor's client actually sends and
	// the checkin endpoint has already been observed keying on device identity —
	// so send the same shape rather than a subset of it.
	q.Set("x_device_id", deviceID)
	q.Set("x_machine_id", machineID)
	q.Set("ts", fmt.Sprintf("%d", ts))
	return traeConsoleHost + traeLoginPath + "?" + q.Encode(), nil
}

// handleCallback accepts the console's redirect. Two shapes arrive in practice:
// the current one carries a refreshToken directly, the older one nests the pair
// in a `userJwt` JSON blob. Both are accepted, and a callback that carries
// neither must still SETTLE the flow — otherwise the card sits on "authenticating"
// forever.
func (p *traePendingLogin) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	refreshToken := q.Get("refreshToken")
	userInfo := parseTraeJSONParam(q.Get("userInfo"))
	userJwt := parseTraeJSONParam(q.Get("userJwt"))

	accessToken := traeJSONField(userJwt, "Token")
	if accessToken == "" {
		accessToken = traeJSONField(userJwt, "AccessToken")
	}
	if refreshToken == "" {
		refreshToken = traeJSONField(userJwt, "RefreshToken")
	}

	finish := func(msg string) {
		w.Header().Set("content-type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><meta charset=utf-8><title>登录完成</title>" +
			"<body style=\"font:15px system-ui;padding:40px;text-align:center\">" + msg +
			"<p style=\"color:#888;font-size:13px\">可以关闭此页面回到 zen-gate。</p></body>"))
		p.mu.Lock()
		p.result = "ok"
		p.mu.Unlock()
		close(p.done)
	}

	if accessToken == "" && refreshToken == "" {
		// Distinguish "the user cancelled" from "the console answered through a
		// flow this build cannot finish". Both settle the popup so the card
		// stops spinning, but only the second one is actionable.
		if code := traeCallbackAuthCode(q, parseTraeJSONParam(q.Get("authCodeInfo"))); code != "" {
			p.mu.Lock()
			p.err = "上游回调了 PKCE 授权码（code/authCodeInfo），本版本只支持直传 token 的流程；" +
				"请确认 TRAE 授权页是否已切换到新流程"
			p.mu.Unlock()
			finish("授权未完成：TRAE 走了 PKCE 流程，本版本暂不支持。")
			return
		}
		// A callback with no token at all is a real outcome (the console sent
		// the user back without approving). Settle it as a failure rather than
		// leaving the flow hanging.
		p.mu.Lock()
		p.err = "回调未携带 token（可能取消了授权）"
		p.mu.Unlock()
		finish("授权未完成：回调里没有 token。")
		return
	}

	acct := TraeAccount{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		Enabled:      true,
		MachineID:    traeNewDeviceID(),
		DeviceID:     traeNewDeviceID(),
	}
	acct.UID = traeJSONField(userInfo, "UserID")
	if acct.UID == "" {
		acct.UID = traeJSONField(userInfo, "userID")
	}
	if acct.UID == "" {
		acct.UID = traeJSONField(userJwt, "UserID")
	}
	acct.Nickname = firstNonEmptyStr(
		traeFixMojibake(traeJSONField(userInfo, "ScreenName")),
		traeFixMojibake(traeJSONField(userInfo, "Nickname")),
		acct.UID,
	)
	acct.Phone = traeJSONField(userInfo, "NonPlainTextMobile")
	// The console spells the enterprise field `TenantID` (capital D), not
	// `TenantId`/`EnterpriseId`. Reading the wrong spelling yields an empty
	// enterprise and the account looks personal when it is a team seat.
	acct.EnterpriseID = firstNonEmptyStr(
		traeJSONField(userInfo, "TenantID"),
		traeJSONField(userInfo, "TenantId"),
	)
	if acct.UID == "" {
		// Without a uid the account cannot be keyed or fingerprinted; derive a
		// stable one from the token so the login still lands instead of dying
		// on a field the console did not send.
		acct.UID = "unknown-" + hex.EncodeToString([]byte(accessToken))[:12]
	}
	acct.ID = "trae-" + acct.UID
	acct.ExpiresAtMS = traeJWTExp(accessToken)

	p.mu.Lock()
	p.acct = acct
	p.mu.Unlock()
	finish("登录成功，账号已保存。")
}

// TraeLoginPoll waits for the callback. It blocks until the browser comes back,
// the TTL expires, or the flow is cancelled — the caller polls this once and the
// HTTP request stays open, which is what lets the card stop spinning the moment
// the account lands.
func (l *Lane) TraeLoginPoll() (string, error) {
	traeLoginMu.Lock()
	pend := traeLoginCur
	traeLoginMu.Unlock()
	if pend == nil {
		return "", fmt.Errorf("没有进行中的 TRAE 登录")
	}
	select {
	case <-pend.done:
	case <-time.After(3 * time.Second):
		if time.Since(pend.startAt) > traeLoginTTL {
			l.traeLoginClose()
			return "", fmt.Errorf("登录已超时，请重新开始")
		}
		return "pending", nil
	}
	pend.mu.Lock()
	acct, errText := pend.acct, pend.err
	pend.mu.Unlock()
	l.traeLoginClose()
	if errText != "" {
		return "", fmt.Errorf("%s", errText)
	}
	if acct.AccessToken == "" && acct.RefreshToken == "" {
		return "", fmt.Errorf("登录未完成")
	}
	l.traeUpsert(acct)
	go l.RefreshCatalog(context.Background())
	l.fireChange()
	return "ok", nil
}

// traeLoginClose stops the listener and forgets the flow.
func (l *Lane) traeLoginClose() {
	traeLoginMu.Lock()
	pend := traeLoginCur
	traeLoginCur = nil
	traeLoginMu.Unlock()
	if pend != nil && pend.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = pend.server.Shutdown(ctx)
	}
}

// parseTraeJSONParam decodes a URL-encoded JSON parameter from the callback.
// The console sends it URL-encoded and sometimes double-encoded.
func parseTraeJSONParam(raw string) map[string]any {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	if dec, err := url.QueryUnescape(s); err == nil {
		s = dec
	}
	// A base64 variant appears too; try it before giving up.
	if !strings.HasPrefix(s, "{") {
		if b, err := base64.StdEncoding.DecodeString(s); err == nil && strings.HasPrefix(string(b), "{") {
			s = string(b)
		}
	}
	var out map[string]any
	if json.Unmarshal([]byte(s), &out) == nil {
		return out
	}
	return nil
}

// traeJSONField reads one string field from a decoded parameter, tolerating the
// case the console happens to use.
func traeJSONField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	for k, v := range m {
		if strings.EqualFold(k, key) {
			if s, ok := v.(string); ok {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// traeFixMojibake repairs the nickname the console sends when it has already
// mangled UTF-8 through a single-byte codec: the bytes arrive as the mojibake
// that latin-1 decoding produces, and reinterpreting each rune as one byte
// recovers the original. Names that are already valid are returned unchanged,
// so this can be applied unconditionally.
func traeFixMojibake(s string) string {
	if s == "" {
		return ""
	}
	// A mojibake run is all code points below U+0100 (each stands for one
	// byte); real CJK names contain code points well above that.
	needsFix := false
	suspect := true
	for _, r := range s {
		if r >= 0x4E00 {
			suspect = false
			break
		}
		if r >= 0x80 {
			needsFix = true
		}
	}
	if !needsFix || !suspect {
		return s
	}
	buf := make([]byte, 0, len(s))
	for _, r := range s {
		if r > 0xFF {
			return s // not a byte-per-rune string; leave it alone
		}
		buf = append(buf, byte(r))
	}
	if fixed := string(buf); utf8.ValidString(fixed) {
		return fixed
	}
	return s
}

// traeCallbackAuthCode extracts a PKCE authorization code from the callback.
//
// The console runs two flows side by side: the local handoff returns tokens
// directly, while the PKCE flow returns `code` / `authCode` / `authCodeInfo`
// instead. This gateway does not exchange that code, so the value only exists
// to tell the two apart — a callback carrying a code is a legitimate
// authorization that this build cannot finish, not a cancelled one. Reporting
// it as "no token" sends the user looking at the wrong thing.
func traeCallbackAuthCode(q url.Values, authCodeInfo map[string]any) string {
	candidates := []string{
		q.Get("code"),
		q.Get("authCode"),
		traeJSONField(authCodeInfo, "code"),
		traeJSONField(authCodeInfo, "authCode"),
		// A bare-string authCodeInfo is not JSON, so the field reads above miss
		// it; the raw value is the last resort.
		q.Get("authCodeInfo"),
	}
	for _, c := range candidates {
		if s := strings.TrimSpace(c); s != "" {
			return s
		}
	}
	return ""
}
