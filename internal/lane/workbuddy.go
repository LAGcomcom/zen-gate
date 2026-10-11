package lane

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The WorkBuddy 账号渠道 (intl): a credential-bearing lane source modeled on
// the Kilo template. The upstream speaks an OpenAI Chat variant on
// /v2/chat/completions — SSE, delta.reasoning_content for thinking — so the
// shared chat decoder works as is; what this source adds is the account
// lifecycle: a state-polling OAuth login, a refresh flow whose refresh token
// ROTATES upstream (two concurrent refreshes void the second one — every
// refresh is serialized), and the desktop-client identity headers the WAF
// expects (X-IDE-*/X-Product plus uid-derived X-Machine-ID/X-Session-ID).
//
// The free models ride a 24h rolling window per account: upstream answers 429
// with code 6004 and a reset wall clock, 14003 (or its text) for momentary
// crowding, 402 when paid balance is gone, and 403 with codes 11140/11128 when
// the WAF pattern-matches client fingerprints inside the system prompt — that
// last one is retried once with the known marker strings rewritten. The three
// free ids (hy4-preview-f, hy3, deepseek-v4.1-flash) are shipped as a static
// catalog: the dynamic listing endpoint sits behind the WAF and flakes, and a
// roster this small is cheaper to curate than to fetch.
//
// Prompts may be logged by the upstream; the blurb says so in front of the
// user, same discipline as the Kilo pool.

// WorkBuddyBase is the intl gateway root; override for tests.
var WorkBuddyBase = "https://www.workbuddy.ai"

const (
	wbTurnTimeout = 5 * time.Minute
	wbChatPath    = "/v2/chat/completions"

	wbAuthStatePath    = "/v2/plugin/auth/state"
	wbAuthTokenPath    = "/v2/plugin/auth/token"
	wbAuthRefreshPath  = "/v2/plugin/auth/token/refresh"
	wbLoginAccountPath = "/v2/plugin/login/account"
	wbUserResourcePath = "/v2/billing/meter/get-user-resource"

	// The desktop identity the WAF fingerprints: version constants move with
	// the client, so they live here as one edit-point.
	wbIDEVersion = "5.5.2"
	wbDesktopUA  = "WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2"
	wbOrigin     = "https://www.workbuddy.ai"
	wbDomain     = "www.workbuddy.ai"

	wbLoginPending  = 11217 // upstream code: the browser login is not finished yet
	wbLoginTTL      = 10 * time.Minute
	wbRefreshEarly  = 2 * time.Hour // refresh proactively this close to expiry
	wbSoftCooldown  = 60            // seconds; the soft-limit single-model window
	wbCapDefaultWait = 600          // seconds when 6004 carries no parsable reset clock
)

// wbCodeSanitize is the internal sentinel for "the WAF matched a client
// fingerprint": PostWorkBuddyStreamed rewrites the marker strings and retries
// once, converting a second hit into a plain CodeClient. The sentinel never
// escapes this file.
const wbCodeSanitize = "WB_SANITIZE"

// WBAccount is one WorkBuddy login. store.Config persists it verbatim; the
// lane keeps a snapshot and writes rotated token pairs back through the
// persist callback. UID is the JWT sub — the fingerprint seeds derive from it,
// so the same account always presents the same machine/session ids.
type WBAccount struct {
	AccessToken      string `json:"accessToken,omitempty"`
	RefreshToken     string `json:"refreshToken,omitempty"`
	UID              string `json:"uid,omitempty"`
	Nickname         string `json:"nickname,omitempty"`
	ExpiresAtMS      int64  `json:"expiresAtMs,omitempty"`
	Enabled          bool   `json:"enabled"`
	LastActivityDate string `json:"lastActivityDate,omitempty"`
}

// wbDeriveID derives the machine/session/request ids the upstream expects:
// md5("<salt>:<uid>"), stable per account — never re-randomized, so one
// account always looks like one machine and different accounts never correlate.
func wbDeriveID(uid, salt string) string {
	sum := md5.Sum([]byte(salt + ":" + uid))
	return hex.EncodeToString(sum[:])
}

// wbDrain reads a bounded error/JSON body off the pump. Unlike a plain
// io.ReadAll it keeps a per-read deadline: the upstream answers refusals over
// HTTP/2 and does not always half-close the stream, so "the body ended" never
// arrives and an unbounded drain parks the caller for the full idle timeout.
// Data that keeps flowing keeps resetting the deadline — only true silence
// ends the drain.
func wbDrain(s *streamReader) []byte {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 2048)
	for len(buf) < 1<<20 {
		k, err := s.ReadTimeout(tmp, wbDrainReadTimeout)
		buf = append(buf, tmp[:k]...)
		if err != nil || k == 0 {
			return buf
		}
	}
	return buf
}

const wbDrainReadTimeout = 2 * time.Second

func wbRequestID() string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

// wbJWTClaims reads sub/iss/exp out of an access token. exp wins over any
// expiresAt field the endpoints carry — it is the JWT's own word.
func wbJWTClaims(token string) (sub, iss string, expMS int64, ok bool) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return "", "", 0, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if payload, err = base64.RawStdEncoding.DecodeString(parts[1]); err != nil {
			return "", "", 0, false
		}
	}
	var claims struct {
		Sub string `json:"sub"`
		Iss string `json:"iss"`
		Exp any    `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return "", "", 0, false
	}
	return claims.Sub, claims.Iss, wbExpToMS(claims.Exp), true
}

func wbExpToMS(v any) int64 {
	f, ok := toNum(v)
	if !ok || f <= 0 {
		return 0
	}
	if f < 1e12 {
		return int64(f) * 1000 // seconds
	}
	return int64(f)
}

func wbTokenNearExpiry(acct WBAccount) bool {
	if acct.ExpiresAtMS <= 0 {
		return false // unknown lifetime: let an upstream 401 drive the refresh
	}
	return time.Now().UnixMilli()+wbRefreshEarly.Milliseconds() >= acct.ExpiresAtMS
}

// buildWorkBuddyCatalog is the intl free roster, one entry per x0.00 model in
// the desktop app's own listing. hy4/hy3 only accept the high effort rung, and
// deepseek-v4.1-flash runs thinking forced — hence CanDisableThinking false on
// all three; the reasoning_effort field is still set per rung (see
// wbApplyModelQuirks) but the off rung is never offered.
func buildWorkBuddyCatalog() []ModelInfo {
	blurb := "WorkBuddy 账号免费模型，24h 窗口限量；提示词可能被上游记录，别交机密内容"
	return []ModelInfo{
		{ID: "hy4-preview-f", Name: "Hy4 Preview", Blurb: blurb,
			Wire: "chat", Channel: ChannelWorkBuddy,
			Reasoning: true, ContextWindow: 1000000, MaxOutput: 64000, CanDisableThinking: false},
		{ID: "hy3", Name: "Hy3", Blurb: blurb,
			Wire: "chat", Channel: ChannelWorkBuddy,
			Reasoning: true, ContextWindow: 192000, MaxOutput: 64000, CanDisableThinking: false},
		{ID: "deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash", Blurb: blurb,
			Wire: "chat", Channel: ChannelWorkBuddy,
			Vision: true, FileInput: true, Reasoning: true,
			ContextWindow: 1000000, MaxOutput: 128000, CanDisableThinking: false},
	}
}

// workBuddyHeaders assembles the full desktop-client header set for one chat
// request: the base web layer, the identity layer, and the uid-derived
// machine/session ids. No cookies, no signatures — the JWT is the credential.
func workBuddyHeaders(acct WBAccount, requestID string) map[string]string {
	return map[string]string{
		"content-type":        "application/json",
		"accept":              "application/json, text/plain, */*",
		"x-requested-with":    "XMLHttpRequest",
		"origin":              wbOrigin,
		"referer":             wbOrigin + "/",
		"x-codebuddy-request": "1",
		"accept-language":     "en-US",
		"x-machine-id":        wbDeriveID(acct.UID, "machine"),
		"x-session-id":        wbDeriveID(acct.UID, "session"),
		"x-agent-purpose":     "conversation",
		"x-ide-name":          "WorkBuddy",
		"x-ide-type":          "WorkBuddy",
		"x-ide-version":       wbIDEVersion,
		"x-product":           "WorkBuddy",
		"x-domain":            wbDomain,
		"user-agent":          wbDesktopUA,
		"x-user-id":           acct.UID,
		"authorization":       "Bearer " + acct.AccessToken,
		"x-request-id":        requestID,
	}
}

// wbApplyModelQuirks layers this source's body extensions onto a chat body:
// forced streaming with usage, and per-family thinking fields (deepseek wants
// the thinking switch plus its own effort spelling; the hy family only
// accepts "high").
func wbApplyModelQuirks(body map[string]any, model, effort string) {
	body["stream_options"] = map[string]any{"include_usage": true}
	switch {
	case strings.HasPrefix(model, "deepseek"):
		body["thinking"] = map[string]any{"type": "enabled"}
		body["reasoning_effort"] = wbDeepseekEffort(effort)
	case strings.HasPrefix(model, "hy"):
		body["reasoning_effort"] = "high"
	}
}

// wbDeepseekEffort maps the rung ladder onto the effort spelling deepseek
// accepts. Thinking stays on for every rung (forced reasoning), the rung only
// picks how hard it thinks.
func wbDeepseekEffort(effort string) string {
	switch effort {
	case "light":
		return "low"
	case "deep":
		return "high"
	default:
		return "medium"
	}
}

// wbBackfillReasoning satisfies an upstream quirk: with thinking enabled, every
// assistant message must carry reasoning_content (an empty string passes).
// ToChatMessages never emits the field, so the gateway adds it.
func wbBackfillReasoning(body map[string]any) {
	msgs, ok := body["messages"].([]map[string]any)
	if !ok {
		return
	}
	for _, m := range msgs {
		if m["role"] != RoleAssistant {
			continue
		}
		if _, has := m["reasoning_content"]; !has {
			m["reasoning_content"] = ""
		}
	}
}

// wbPostOnce performs one physical POST and decodes the stream, following the
// same sniff-and-replay discipline as the other post paths. Non-2xx answers go
// through the WorkBuddy-specific classifier.
func wbPostOnce(ctx context.Context, body map[string]any, acct WBAccount, onData func(payload []byte) error) (*Usage, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, &UpstreamError{Code: CodeTransport, Message: err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, WorkBuddyBase+wbChatPath, bytes.NewReader(payload))
	if err != nil {
		return nil, &UpstreamError{Code: CodeTransport, Message: err.Error()}
	}
	for k, v := range workBuddyHeaders(acct, wbRequestID()) {
		req.Header.Set(k, v)
	}
	resp, err := laneHTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &UpstreamError{Code: CodeAborted, Message: "cancelled"}
		}
		return nil, &UpstreamError{Code: CodeTransport, Message: err.Error()}
	}
	defer resp.Body.Close()

	retryAfter := 0
	if ra := resp.Header.Get("retry-after"); ra != "" {
		if n, perr := strconv.Atoi(ra); perr == nil && n > 0 && n < 3600 {
			retryAfter = n
		}
	}

	pump := newStreamReader(ctx, resp.Body, wbTurnTimeout)
	defer pump.Close()
	head := readUpTo(pump, 4096, 20*time.Second)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rest := wbDrain(pump)
		return nil, classifyWorkBuddyFailure(resp.StatusCode, string(head)+string(rest), retryAfter)
	}

	switch sniffBody(head) {
	case "empty":
		return nil, &UpstreamError{Code: CodeEmpty, Message: "empty response body"}
	case "json", "unknown":
		rest := wbDrain(pump)
		full := string(head) + string(rest)
		var parsed any
		if jerr := json.Unmarshal([]byte(full), &parsed); jerr != nil {
			return nil, &UpstreamError{Code: CodeServer, Message: truncate(full, 300)}
		}
		if m, ok := parsed.(map[string]any); ok {
			if _, has := m["error"]; has {
				return nil, classifyWorkBuddyFailure(resp.StatusCode, full, retryAfter)
			}
		}
		if uerr := onData([]byte(full)); uerr != nil {
			return nil, uerr
		}
		if m, ok := parsed.(map[string]any); ok {
			return usageFromBody(m), nil
		}
		return nil, nil
	}

	pump.Unread(head)
	usage, err := readSSE(pump, onData)
	if err != nil {
		if ctx.Err() != nil {
			return usage, &UpstreamError{Code: CodeAborted, Message: "cancelled"}
		}
		if ue, ok := err.(*UpstreamError); ok {
			return usage, ue
		}
		return usage, &UpstreamError{Code: CodeTransport, Message: err.Error()}
	}
	return usage, nil
}

// PostWorkBuddyStreamed posts one turn, retrying a WAF fingerprint hit once
// with the marker strings rewritten. A second hit surfaces as a plain client
// error — the request is the caller's own fingerprint, replaying it more
// cannot help.
func PostWorkBuddyStreamed(ctx context.Context, body map[string]any, acct WBAccount, onData func(payload []byte) error) (*Usage, error) {
	usage, err := wbPostOnce(ctx, body, acct, onData)
	if err == nil {
		return usage, nil
	}
	ue := asUpstream(err)
	if ue == nil || ue.Code != wbCodeSanitize {
		return usage, err
	}
	usage2, err2 := wbPostOnce(ctx, wbSanitizeFingerprints(body), acct, onData)
	if err2 == nil {
		return usage2, nil
	}
	ue2 := asUpstream(err2)
	if ue2 != nil && ue2.Code == wbCodeSanitize {
		return usage, &UpstreamError{Code: CodeClient,
			Message: "上游内容门拦截了请求里的客户端特征文本，改写后仍被拒绝——换一个说法或去掉相关系统提示"}
	}
	return usage, err2
}

// wbFingerprintRewrites are the marker strings the WAF pattern-matches inside
// the system prompt (observed on Codex / Claude Code traffic): each maps to a
// neutral stand-in. The list follows the upstream's evolving rules and is the
// one place to update when 11140/11128 starts hitting again.
var wbFingerprintRewrites = []struct {
	re *regexp.Regexp
	to string
}{
	{regexp.MustCompile(`(?i)x-anthropic-billing-header[^\n"]*`), ""},
	{regexp.MustCompile(`(?i)\bcc_[a-z_]+=[^\s"']*`), ""},
	{regexp.MustCompile(`You are Claude Code`), "You are a CLI tool for Claude"},
	{regexp.MustCompile(`Main branch \(you will usually use this for PRs\)`), "Default branch (you will usually use this for PRs)"},
	{regexp.MustCompile(`(?i)OhMyOpenCode`), "OpenCode"},
	{regexp.MustCompile(`11128`), "11-128"},
}

// wbSanitizeFingerprints rewrites the known client fingerprints out of a
// request body's message texts. Messages are deep-copied: the caller's body
// must stay intact for the recovery paths.
func wbSanitizeFingerprints(body map[string]any) map[string]any {
	next := cloneMap(body)
	if msgs, ok := body["messages"].([]map[string]any); ok {
		copied := make([]map[string]any, len(msgs))
		for i, m := range msgs {
			copied[i] = cloneMap(m)
			switch text := copied[i]["content"].(type) {
			case string:
				copied[i]["content"] = wbRewriteText(text)
			case []any:
				parts := make([]any, len(text))
				copy(parts, text)
				for j, p := range parts {
					if pm, ok := p.(map[string]any); ok {
						if t, ok := pm["text"].(string); ok {
							pm = cloneMap(pm)
							pm["text"] = wbRewriteText(t)
							parts[j] = pm
						}
					}
				}
				copied[i]["content"] = parts
			case []map[string]any:
				parts := make([]map[string]any, len(text))
				for j, p := range text {
					pm := cloneMap(p)
					if t, ok := pm["text"].(string); ok {
						pm["text"] = wbRewriteText(t)
					}
					parts[j] = pm
				}
				copied[i]["content"] = parts
			}
		}
		next["messages"] = copied
	}
	return next
}

func wbRewriteText(text string) string {
	for _, r := range wbFingerprintRewrites {
		text = r.re.ReplaceAllString(text, r.to)
	}
	return text
}

// Classify codes this source carries beyond the shared vocabulary.
var (
	wbCapLimitCode  = 6004 // the free window is spent; the only code with a reset clock
	wbSoftLimitCode = 14003
)

var (
	wbResetTimeRe = regexp.MustCompile(`(?:reset at|将在)\s*(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2})`)
	wbResetTZRe   = regexp.MustCompile(`UTC([+-])(\d{1,2})(?::?(\d{2}))?`)
	wbSoftLimitRe = regexp.MustCompile(`(?i)usage exceeds frequency limit|too many requests|请求过于频繁`)
)

// classifyWorkBuddyFailure reads one of this gateway's refusals onto the
// shared vocabulary. The status alone misleads here: 6004 arrives as a 429 but
// names a daily window (its reset wall clock is the honest Retry-After),
// 14003 is momentary crowding, 402 is paid balance, and 403 with 11140/11128
// is the WAF speaking — a request-text verdict, not a credential one.
func classifyWorkBuddyFailure(status int, payload string, retryAfterSec int) *UpstreamError {
	code := wbCodeFromPayload(payload)
	switch {
	case code == wbCapLimitCode:
		wait := wbResetWait(payload)
		if wait <= 0 {
			wait = wbCapDefaultWait
		}
		if retryAfterSec > 0 && retryAfterSec < wait {
			wait = retryAfterSec
		}
		return &UpstreamError{Code: CodeQuota, Message: truncate(payload, 300), RetryAfter: wait}
	case code == wbSoftLimitCode || wbSoftLimitRe.MatchString(payload):
		return &UpstreamError{Code: CodeQuota, Message: truncate(payload, 300), RetryAfter: wbSoftCooldown}
	case status == 402:
		return &UpstreamError{Code: CodeQuota,
			Message:    truncate(payload, 300) + "（WorkBuddy 余额耗尽，免费模型不受影响）",
			RetryAfter: wbSecondsUntil4AM()}
	case status == 401:
		// Unconditional: the front gateway answers a dead credential with an
		// openresty HTML page, and the HTML special case downstream would
		// misfile it as a request fault — exactly the one status that must
		// trigger the refresh-and-retry path.
		return &UpstreamError{Code: CodeCredential, Message: "WorkBuddy 登录态失效（上游 401）"}
	case code == 11140 || code == 11128:
		return &UpstreamError{Code: wbCodeSanitize,
			Message: "上游内容门拦截（" + strconv.Itoa(code) + "）"}
	}
	return ClassifyFailure(status, payload, retryAfterSec)
}

// wbCodeFromPayload digs the numeric error code out of either envelope shape
// the upstream uses: {"code":6004,...} and {"error":{"code":6004,...}}.
func wbCodeFromPayload(payload string) int {
	var shaped map[string]any
	if json.Unmarshal([]byte(payload), &shaped) != nil {
		return 0
	}
	if f, ok := toNum(shaped["code"]); ok {
		return int(f)
	}
	if errObj, ok := shaped["error"].(map[string]any); ok {
		if f, ok := toNum(errObj["code"]); ok {
			return int(f)
		}
	}
	return 0
}

// wbResetWait parses the reset wall clock out of a 6004 body — intl spells it
// "your usage will reset at 2026-09-19 18:29:03 UTC+8" — and returns seconds
// from now. 0 when nothing parsable is present.
func wbResetWait(payload string) int {
	m := wbResetTimeRe.FindStringSubmatch(payload)
	if m == nil {
		return 0
	}
	stamp := strings.Replace(m[1], "T", " ", 1)
	t, err := time.ParseInLocation("2006-01-02 15:04:05", stamp, wbResetZone(payload))
	if err != nil {
		return 0
	}
	wait := int(time.Until(t).Seconds())
	if wait < 0 {
		return 0
	}
	if wait > 48*3600 {
		wait = 48 * 3600
	}
	return wait
}

func wbResetZone(payload string) *time.Location {
	m := wbResetTZRe.FindStringSubmatch(payload)
	if m == nil {
		return time.Local
	}
	h, _ := strconv.Atoi(m[2])
	min := 0
	if m[3] != "" {
		min, _ = strconv.Atoi(m[3])
	}
	off := h*3600 + min*60
	if m[1] == "-" {
		off = -off
	}
	return time.FixedZone("wb", off)
}

func wbSecondsUntil4AM() int {
	now := time.Now()
	next := time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return int(next.Sub(now).Seconds())
}
