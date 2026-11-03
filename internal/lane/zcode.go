package lane

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ZCode (智谱) 账号渠道 — native login.
//
// The login is a server-mediated device authorization flow, so this gateway can
// own it end to end without any local callback port and without reading another
// app's files:
//
//	① POST {origin}/api/v1/oauth/cli/init     Authorization: Bearer <our own 32-byte hex>
//	   → { flow_id, poll_token, authorize_url, expires_at, poll_interval_sec }
//	② the user opens authorize_url in a browser and approves
//	   (approval completes server-side; nothing calls back to us)
//	③ GET  {origin}/api/v1/oauth/cli/poll/{flow_id}   same Bearer
//	   → { status: "pending" }
//	   → { status: "ready", token, user, zai|bigmodel: { access_token, refresh_token } }
//
// The Bearer on those two calls is a secret WE mint, not a user credential —
// that is exactly what makes this flow work from a plain process.
//
// Two wire facts decide the rest of this file:
//
//   - The zcode JWT (`data.token`) is what authorizes inference; the third-party
//     `access_token` in `data.zai` / `data.bigmodel` is a separate identity that
//     is NOT sent on chat turns. Both are stored, and either block satisfies the
//     login: requiring one specific block is how a working flow gets reported as
//     "missing required field".
//   - Inference speaks Anthropic's `messages` shape with a private handshake
//     (identity headers, an official system block, effort in
//     `output_config.effort`). A body without the identity block draws
//     405 + code 3012 — a verdict about the envelope, not the content, and one
//     that puts the ACCOUNT on a cooling penalty, so it must never be retried.

// ZCodeOrigin is the platform root; override for tests.
var ZCodeOrigin = "https://zcode.z.ai"

const (
	zcodeAppVersion    = "3.14.3"
	zcodeLoginProvider = "bigmodel" // 智谱开放平台（国内）; "zai" is the intl one
	zcodeLoginTTL      = 5 * time.Minute
	zcodeClientType    = "5"
	zcodeReleaseChan   = "stable"
	zcodePlatform      = "unknown" // configs endpoint refuses win32/windows/electron
)

// ZCodeAccount is one ZCode login, as this gateway stores it.
type ZCodeAccount struct {
	ID       string `json:"id,omitempty"`
	JWT      string `json:"jwt,omitempty"`
	DeviceMID string `json:"deviceMid,omitempty"`
	UserID   string `json:"userId,omitempty"`
	Label    string `json:"label,omitempty"`
	// ZaiToken / BigmodelToken are the third-party identities the flow hands
	// back. They authorize nothing on the chat wire; they are kept because the
	// coding-plan key is exchanged from them.
	ZaiToken      string `json:"zaiToken,omitempty"`
	BigmodelToken string `json:"bigmodelToken,omitempty"`
	Enabled       bool   `json:"enabled"`
}

// zcodeHeaders is the inference handshake. `Authorization: Bearer <jwt>` is the
// ONLY credential header — there is no x-api-key on this wire — and every
// X-* identity field travels together or the request is refused on shape alone.
func zcodeHeaders(a ZCodeAccount, stream bool) map[string]string {
	accept := "application/json"
	if stream {
		accept = "text/event-stream"
	}
	return map[string]string{
		"Authorization":        "Bearer " + a.JWT,
		"Content-Type":         "application/json",
		"Accept":               accept,
		"anthropic-version":    "2023-06-01",
		"User-Agent":           "ZCode/" + zcodeAppVersion,
		"HTTP-Referer":         ZCodeOrigin,
		"X-ZCode-App-Version":  zcodeAppVersion,
		"X-Release-Channel":    zcodeReleaseChan,
		"X-Client-Language":    "zh-CN",
		"X-Client-Timezone":    "Asia/Shanghai",
		"X-Device-Mid":         a.DeviceMID,
		"X-Platform":           "win32",
		"X-Os-Category":        "windows",
	}
}

// zcodeLoginFlow is one in-flight login. The poll secret is what the poll call
// authenticates with, so it must live exactly as long as the flow.
type zcodeLoginFlow struct {
	FlowID     string
	Secret     string
	AuthorizeURL string
	Interval   time.Duration
	StartedAt  time.Time
	Provider   string
}

var (
	zcodeLoginMu   sync.Mutex
	zcodeLoginCur  *zcodeLoginFlow
)

// zcodeRandomHex mints the flow secret: 32 random bytes, hex, as the client does.
func zcodeRandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// zcodeNewDeviceMID mints the device identity. It only has to be STABLE per
// account — the upstream treats it as an opaque install marker, not a secret —
// so a fresh value per login is correct and reusing it as an account key is not.
func zcodeNewDeviceMID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	// Format as a UUID so the value looks like what the desktop client sends.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ZCodeLoginStart begins one device-authorization flow and returns the URL the
// user must open. Nothing else is needed from the caller until the poll answers
// "ready".
func (l *Lane) ZCodeLoginStart(provider string) (string, error) {
	if provider == "" {
		provider = zcodeLoginProvider
	}
	secret, err := zcodeRandomHex(32)
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]any{"provider": provider})
	req, err := http.NewRequest(http.MethodPost, ZCodeOrigin+"/api/v1/oauth/cli/init", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ZCode/"+zcodeAppVersion)
	req.Header.Set("HTTP-Referer", ZCodeOrigin)
	req.Header.Set("X-ZCode-App-Version", zcodeAppVersion)
	req.Header.Set("X-Platform", "win32")

	client := &http.Client{Timeout: 30 * time.Second, Transport: laneHTTP.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("无法连接 ZCode 授权服务：%v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("授权初始化失败（HTTP %d）：%s", resp.StatusCode, truncate(string(raw), 200))
	}
	var parsed struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			FlowID          string  `json:"flow_id"`
			AuthorizeURL    string  `json:"authorize_url"`
			PollIntervalSec float64 `json:"poll_interval_sec"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("授权初始化响应不是 JSON：%s", truncate(string(raw), 200))
	}
	if parsed.Data.FlowID == "" {
		return "", fmt.Errorf("授权初始化响应缺 flow_id：%s", truncate(parsed.Msg, 160))
	}
	if !strings.HasPrefix(parsed.Data.AuthorizeURL, "https://") {
		return "", fmt.Errorf("授权初始化响应缺合法的 authorize_url")
	}
	interval := parsed.Data.PollIntervalSec
	if interval < 1 || interval > 60 {
		interval = 2
	}

	zcodeLoginMu.Lock()
	zcodeLoginCur = &zcodeLoginFlow{
		FlowID:       parsed.Data.FlowID,
		Secret:       secret,
		AuthorizeURL: parsed.Data.AuthorizeURL,
		Interval:     time.Duration(interval * float64(time.Second)),
		StartedAt:    time.Now(),
		Provider:     provider,
	}
	zcodeLoginMu.Unlock()
	return parsed.Data.AuthorizeURL, nil
}

// ZCodeLoginPoll checks one round. "pending" while the browser side is
// unfinished, "ok" once the account landed and was folded into the lane.
//
// Network trouble is reported as pending, not as failure: a transient dial error
// must not throw away a flow the user is still approving in a browser.
func (l *Lane) ZCodeLoginPoll() (string, error) {
	zcodeLoginMu.Lock()
	flow := zcodeLoginCur
	zcodeLoginMu.Unlock()
	if flow == nil {
		return "", fmt.Errorf("没有进行中的 ZCode 登录")
	}
	if time.Since(flow.StartedAt) > zcodeLoginTTL {
		zcodeLoginMu.Lock()
		zcodeLoginCur = nil
		zcodeLoginMu.Unlock()
		return "", fmt.Errorf("登录已超时，请重新开始")
	}

	req, err := http.NewRequest(http.MethodGet,
		ZCodeOrigin+"/api/v1/oauth/cli/poll/"+flow.FlowID, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+flow.Secret)
	req.Header.Set("User-Agent", "ZCode/"+zcodeAppVersion)
	req.Header.Set("HTTP-Referer", ZCodeOrigin)
	req.Header.Set("X-ZCode-App-Version", zcodeAppVersion)

	client := &http.Client{Timeout: 20 * time.Second, Transport: laneHTTP.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return "pending", nil // a blip is not a verdict
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	// Only a client error is terminal (408 and 429 excepted): the flow is gone.
	if resp.StatusCode >= 400 && resp.StatusCode < 500 &&
		resp.StatusCode != 408 && resp.StatusCode != 429 {
		return "", fmt.Errorf("轮询被拒（HTTP %d）：%s", resp.StatusCode, truncate(string(raw), 200))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "pending", nil
	}

	var parsed struct {
		Code int `json:"code"`
		Data *struct {
			Status string `json:"status"`
			Token  string `json:"token"`
			User   struct {
				UserID string `json:"user_id"`
				ID     string `json:"id"`
				Name   string `json:"name"`
				Email  string `json:"email"`
			} `json:"user"`
			Zai      *zcodeOAuthBlock `json:"zai"`
			Bigmodel *zcodeOAuthBlock `json:"bigmodel"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &parsed) != nil || parsed.Data == nil {
		return "pending", nil
	}
	switch parsed.Data.Status {
	case "pending":
		return "pending", nil
	case "failed":
		return "", fmt.Errorf("用户拒绝了授权或授权失败")
	case "ready":
	default:
		return "", fmt.Errorf("轮询响应状态无法识别：%s", parsed.Data.Status)
	}

	jwt := parsed.Data.Token
	userID := parsed.Data.User.UserID
	if userID == "" {
		userID = parsed.Data.User.ID
	}
	if jwt == "" || userID == "" {
		return "", fmt.Errorf("轮询响应缺关键字段（token / user_id）")
	}
	acct := ZCodeAccount{
		JWT:      jwt,
		DeviceMID: zcodeNewDeviceMID(),
		UserID:   userID,
		Enabled:  true,
	}
	if b := parsed.Data.Zai; b != nil {
		acct.ZaiToken = b.token()
	}
	if b := parsed.Data.Bigmodel; b != nil {
		acct.BigmodelToken = b.token()
	}
	if parsed.Data.Zai == nil && parsed.Data.Bigmodel == nil {
		// Not fatal for inference (the jwt is what authorizes it), but the
		// coding-plan key cannot be exchanged without one of them.
		acct.ZaiToken = ""
	}
	acct.Label = firstNonEmptyStr(parsed.Data.User.Name, parsed.Data.User.Email, userID)
	acct.ID = "zcode-" + userID

	l.zcodeUpsert(acct)
	zcodeLoginMu.Lock()
	zcodeLoginCur = nil
	zcodeLoginMu.Unlock()

	go l.RefreshCatalog(context.Background())
	l.fireChange()
	return "ok", nil
}

// zcodeOAuthBlock is the third-party token block; the two spellings appear in
// different publishments.
type zcodeOAuthBlock struct {
	AccessToken  string `json:"access_token"`
	AccessToken2 string `json:"accessToken"`
	RefreshToken string `json:"refresh_token"`
	RefreshTok2  string `json:"refreshToken"`
}

func (b zcodeOAuthBlock) token() string {
	return firstNonEmptyStr(b.AccessToken, b.AccessToken2)
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
