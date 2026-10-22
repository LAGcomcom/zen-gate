package lane

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
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
// last one is retried once with the known marker strings rewritten. The free
// ids (hy4-preview-f, hy3, deepseek-v4.1-flash, step-5-preview) are shipped
// as a static catalog: the dynamic listing endpoint sits behind the WAF and
// flakes, and a roster this small is cheaper to curate than to fetch.
//
// Prompts may be logged by the upstream; the blurb says so in front of the
// user, same discipline as the Kilo pool.

// WorkBuddyBase is the intl gateway root; override for tests.
var WorkBuddyBase = "https://www.workbuddy.ai"

// The CN publishment does not live on one host: it chats on
// copilot.tencent.com but bills and checks in on www.codebuddy.cn, so a
// single "base per realm" cannot express it. Both are vars for the same
// reason WorkBuddyBase is — tests point them at a stub.
var (
	WorkBuddyCNBase    = "https://copilot.tencent.com"
	WorkBuddyCNBilling = "https://www.codebuddy.cn"
)

// The two publishments this lane can log into. Everything that used to be a
// hard-coded intl constant now funnels through wbRealmConfig, so adding a
// region is one table entry rather than a hunt through every call site.
const (
	WBRealmIntl = "intl"
	WBRealmCN   = "cn"
)

// wbRealm is one region's endpoints and desktop identity. The WAF reads the
// Origin/X-Domain/UA family as a fingerprint, so a CN token presented with
// intl headers is refused on shape alone — the fields travel together or not
// at all.
type wbRealm struct {
	ChatBase    string
	BillingBase string
	Origin      string
	Domain      string
	IDEVersion  string
	DesktopUA   string
	// HasCheckin: only CN publishes /v2/billing/meter/daily-checkin. The
	// intl daily is an activity conversation instead (WorkBuddyDailyActivity).
	HasCheckin bool
}

func wbRealmConfig(realm string) wbRealm {
	if realm == WBRealmCN {
		return wbRealm{
			ChatBase:    WorkBuddyCNBase,
			BillingBase: WorkBuddyCNBilling,
			Origin:      "https://www.codebuddy.cn",
			Domain:      "copilot.tencent.com",
			IDEVersion:  wbIDEVersionCN,
			DesktopUA:   wbDesktopUACN,
			HasCheckin:  true,
		}
	}
	return wbRealm{
		ChatBase:    WorkBuddyBase,
		BillingBase: WorkBuddyBase,
		Origin:      wbOrigin,
		Domain:      wbDomain,
		IDEVersion:  wbIDEVersion,
		DesktopUA:   wbDesktopUA,
		HasCheckin:  false,
	}
}

// wbRealmOf resolves an account's region. Accounts minted before the CN lane
// existed carry no realm at all; they are intl, which is what the field would
// have said.
func wbRealmOf(acct WBAccount) string {
	if acct.Realm == WBRealmCN {
		return WBRealmCN
	}
	return WBRealmIntl
}

const (
	wbTurnTimeout = streamIdleTimeout // shares the lane-wide silence bound (PR #40)

	wbChatPath = "/v2/chat/completions"

	wbAuthStatePath    = "/v2/plugin/auth/state"
	wbAuthTokenPath    = "/v2/plugin/auth/token"
	wbAuthRefreshPath  = "/v2/plugin/auth/token/refresh"
	wbLoginAccountPath = "/v2/plugin/login/account"
	wbUserResourcePath = "/v2/billing/meter/get-user-resource"
	// wbCheckinPath is CN-only: the daily reward endpoint. Answering
	// code 0 means claimed; 10001 (or any 已签到 text) means it was already
	// claimed today, which is the same outcome for our purposes.
	wbCheckinPath = "/v2/billing/meter/daily-checkin"
	// wbCheckinAlready is the meter's "you already claimed today" code. It
	// arrives as an HTTP 400 with a 已签到 message, so it reads as a failure
	// to any caller that only checks the status line.
	wbCheckinAlready = 10001

	// The desktop identity the WAF fingerprints: version constants move with
	// the client, so they live here as one edit-point.
	wbIDEVersion = "5.5.2"
	wbDesktopUA  = "WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2"
	wbOrigin     = "https://www.workbuddy.ai"
	wbDomain     = "www.workbuddy.ai"

	// The CN client's own identity. No packet capture of the CN build was
	// available, so these mirror the intl shape with the CN product name —
	// wrong-but-plausible beats intl headers against a CN token.
	wbIDEVersionCN = "5.5.2"
	wbDesktopUACN  = "WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2"

	wbLoginPending   = 11217 // upstream code: the browser login is not finished yet
	wbLoginTTL       = 10 * time.Minute
	wbRefreshEarly   = 2 * time.Hour // refresh proactively this close to expiry
	wbSoftCooldown   = 60            // seconds; the soft-limit single-model window
	wbCapDefaultWait = 600           // seconds when 6004 carries no parsable reset clock
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
	// ID is the account pool's stable handle: admin routes address accounts by
	// it, and it survives a token rotation. It is the UID whenever the token
	// names one; a mint without a subject falls back to a random hex id so the
	// account can still be listed, toggled and removed.
	ID               string `json:"id,omitempty"`
	AccessToken      string `json:"accessToken,omitempty"`
	RefreshToken     string `json:"refreshToken,omitempty"`
	UID              string `json:"uid,omitempty"`
	Nickname         string `json:"nickname,omitempty"`
	ExpiresAtMS      int64  `json:"expiresAtMs,omitempty"`
	Enabled          bool   `json:"enabled"`
	LastActivityDate string `json:"lastActivityDate,omitempty"`
	// Realm is which publishment issued this token: "intl" or "cn". Empty
	// means intl — the field did not exist when intl was the only lane.
	Realm string `json:"realm,omitempty"`
	// LastCheckinDate is the local calendar day the CN daily-checkin last
	// succeeded; CN only, and independent of LastActivityDate (which is the
	// intl activity conversation's day).
	LastCheckinDate string `json:"lastCheckinDate,omitempty"`
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
//
// step-5-preview joins the roster for as long as the official partnership
// window makes it free (the app's own promotion badge: "Free now", 2026-10-12
// through 2026-10-25, unlimited daily credits). After the window the model
// bills the account's credits instead, so it leaves this roster rather than
// quietly spending a balance the user never agreed to spend: the roster's
// promise is that every row in it is free.
func buildWorkBuddyCatalog() []ModelInfo {
	blurb := "WorkBuddy 账号免费模型，24h 窗口限量；提示词可能被上游记录，别交机密内容"
	roster := []ModelInfo{
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
	if time.Now().Before(wbStep5FreeUntil) {
		roster = append(roster, ModelInfo{
			ID: "step-5-preview", Name: "Step-5 Preview",
			Blurb: "WorkBuddy 账号免费模型（官方合作期 10-12 至 10-25 不限额度），24h 窗口限量；提示词可能被上游记录，别交机密内容",
			Wire: "chat", Channel: ChannelWorkBuddy,
			Vision: true, Reasoning: true,
			ContextWindow: 1000000, MaxOutput: 64000, CanDisableThinking: false})
	}
	return roster
}

// wbStep5FreeUntil is the end of the official partnership window that made
// step-5-preview free: the promotion the app itself publishes runs
// 2026-10-12T00:00+08:00 through 2026-10-26T00:00+08:00, so the last free
// day is the 25th.
var wbStep5FreeUntil = time.Date(2026, 10, 26, 0, 0, 0, 0, time.FixedZone("CST", 8*3600))


// workBuddyHeaders assembles the full desktop-client header set for one chat
// request: the base web layer, the identity layer, and the uid-derived
// machine/session ids. No cookies, no signatures — the JWT is the credential.
func workBuddyHeaders(acct WBAccount, requestID string) map[string]string {
	rc := wbRealmConfig(wbRealmOf(acct))
	return map[string]string{
		"content-type":        "application/json",
		"accept":              "application/json, text/plain, */*",
		"x-requested-with":    "XMLHttpRequest",
		"origin":              rc.Origin,
		"referer":             rc.Origin + "/",
		"x-codebuddy-request": "1",
		// CN answers zh-CN; the intl build sends en-US. Wrong locale is not
		// fatal but is one more mismatch in a fingerprint the WAF scores.
		"accept-language": func() string {
			if wbRealmOf(acct) == WBRealmCN {
				return "zh-CN"
			}
			return "en-US"
		}(),
		"x-machine-id":    wbDeriveID(acct.UID, "machine"),
		"x-session-id":    wbDeriveID(acct.UID, "session"),
		"x-agent-purpose": "conversation",
		"x-ide-name":      "WorkBuddy",
		"x-ide-type":      "WorkBuddy",
		"x-ide-version":   rc.IDEVersion,
		"x-product":       "WorkBuddy",
		"x-domain":        rc.Domain,
		"user-agent":      rc.DesktopUA,
		"x-user-id":       acct.UID,
		"authorization":   "Bearer " + acct.AccessToken,
		"x-request-id":    requestID,
	}
}

// wbConversationProtocol is the header family the v2 gate fingerprints. Found
// by reading the community's CodeBuddy/WorkBuddy clients (pi-codebuddy-oauth,
// opencode-codebuddy-oauth): the same /v2/chat/completions surface, but with
// X-Conversation-ID and the B3 trace quartet attached. Without it the upstream
// answers 400 code 11128 "conversation id missing" — a verdict about the
// envelope, not the body, so no amount of prompt rewriting can satisfy it.
func wbConversationProtocol(requestID, conversationID, model string) map[string]string {
	trace := wbRequestID() // 32 hex: the shape the client's dashless UUID has
	span, parent := trace[:16], trace[16:32]
	h := map[string]string{
		"X-Request-ID":              requestID,
		"X-Conversation-ID":         conversationID,
		"X-Conversation-Request-ID": requestID,
		"X-Conversation-Message-ID": requestID,
		"X-Request-Trace-Id":        trace,
		"b3":                        trace + "-" + span + "-1-" + parent,
		"X-B3-TraceId":              trace,
		"X-B3-SpanId":               span,
		"X-B3-ParentSpanId":         parent,
		"X-B3-Sampled":              "1",
	}
	if model != "" {
		h["X-Model-ID"] = model
	}
	return h
}

// wbConversationID maps one downstream conversation onto one upstream
// conversation id. The gate wants a 32-hex id, and one conversation must keep
// the same one for the upstream prompt cache to hit, so it is derived from the
// conversation seed instead of cached: no table to bound, no eviction policy.
// An empty seed — a probe, or a client that sends no session hint — gets a
// fresh id per turn.
func wbConversationID(seed string) string {
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return wbRequestID()
	}
	sum := sha256.Sum256([]byte("wb-conversation\x00" + seed))
	return hex.EncodeToString(sum[:16])
}

// wbChatHeaders is the header set for one chat turn: the desktop identity the
// WAF knows, plus the conversation protocol above.
func wbChatHeaders(acct WBAccount, requestID, conversationID, model string) map[string]string {
	h := workBuddyHeaders(acct, requestID)
	for k, v := range wbConversationProtocol(requestID, conversationID, model) {
		h[k] = v
	}
	return h
}

// wbSystemPreamble opens every WorkBuddy turn. The gate's second check is
// "first message is not system prompt" (400, code 11128) — the upstream
// refuses a conversation that starts with the user, which is exactly what a
// plain OpenAI-shaped request looks like. The wording is deliberately neutral:
// a caller's own system prompt is preserved when it already leads the turn.
const wbSystemPreamble = "You are WorkBuddy, an AI assistant for everyday office work."

// wbEnsureSystemFirst makes the turn open with a system message, leaving an
// existing leading system prompt untouched.
func wbEnsureSystemFirst(body map[string]any) {
	msgs, _ := body["messages"].([]map[string]any)
	if len(msgs) > 0 {
		if role, _ := msgs[0]["role"].(string); role == RoleSystem {
			return
		}
	}
	lead := map[string]any{"role": RoleSystem, "content": wbSystemPreamble}
	body["messages"] = append([]map[string]any{lead}, msgs...)
}

// wbApplyModelQuirks layers this source's body extensions onto a chat body:
// forced streaming with usage, and per-family thinking fields (deepseek wants
// the thinking switch plus its own effort spelling; the hy family only
// accepts "high"; the step family is a plain OpenAI-compatible vendor whose
// rung rides on reasoning_effort alone).
func wbApplyModelQuirks(body map[string]any, model, effort string) {
	body["stream_options"] = map[string]any{"include_usage": true}
	switch {
	case strings.HasPrefix(model, "deepseek"):
		body["thinking"] = map[string]any{"type": "enabled"}
		body["reasoning_effort"] = wbOpenAIRungEffort(effort)
	case strings.HasPrefix(model, "hy"):
		body["reasoning_effort"] = "high"
	case strings.HasPrefix(model, "step"):
		body["reasoning_effort"] = wbOpenAIRungEffort(effort)
	}
}

// wbOpenAIRungEffort maps the rung ladder onto the effort spelling the
// OpenAI-compatible families accept: low / medium / high, the same three
// levels the upstream's own listing publishes for them. Thinking stays on for
// every rung (these models reason always), the rung only picks how hard it
// thinks.
func wbOpenAIRungEffort(effort string) string {
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
func wbPostOnce(ctx context.Context, body map[string]any, acct WBAccount, conversationID string, onData func(payload []byte) error) (*Usage, error) {
	// The gate refuses a turn whose first message is not a system prompt, and
	// the rule is enforced here — the one place every WorkBuddy POST goes
	// through — so the hand-built probe and daily-activity bodies obey it
	// exactly like a chat turn does.
	wbEnsureSystemFirst(body)
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, &UpstreamError{Code: CodeTransport, Message: err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, wbRealmConfig(wbRealmOf(acct)).ChatBase+wbChatPath, bytes.NewReader(payload))
	if err != nil {
		return nil, &UpstreamError{Code: CodeTransport, Message: err.Error()}
	}
	model, _ := body["model"].(string)
	for k, v := range wbChatHeaders(acct, wbRequestID(), conversationID, model) {
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
			// readSSE files a mid-stream {"error":...} through the shared
			// classifier, which reads the status line — and this upstream
			// reports its own refusals *inside* a 200 stream. A 6004 delivered
			// that way came out as CodeServer, so the turn neither cooled the
			// model nor switched accounts. Re-file anything unclassified or
			// server-shaped through this source's own vocabulary.
			if ue.Code == CodeServer || ue.Code == "" {
				if refiled := classifyWorkBuddyFailure(0, ue.Message, ue.RetryAfter); refiled != nil {
					return usage, refiled
				}
			}
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
func PostWorkBuddyStreamed(ctx context.Context, body map[string]any, acct WBAccount, conversationID string, onData func(payload []byte) error) (*Usage, error) {
	usage, err := wbPostOnce(ctx, body, acct, conversationID, onData)
	if err == nil {
		return usage, nil
	}
	ue := asUpstream(err)
	if ue == nil || ue.Code != wbCodeSanitize {
		return usage, err
	}
	usage2, err2 := wbPostOnce(ctx, wbSanitizeFingerprints(body), acct, conversationID, onData)
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
	// DeepSeek Harness opens every turn with this fixed self-introduction.
	// The upstream's WAF has learned it and refuses the request outright
	// (11140/11128 on the first try and on the sanitized retry alike), so the
	// client never gets a turn at all. Keep the sentence, drop the product.
	{regexp.MustCompile(`(?i)You are an AI agent powered by DeepSeek Harness`), "You are an AI agent"},
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
	// wbSessionDeadCode is "your session is gone": the same family the CN
	// publishment also spells out as text. It is usually a network hiccup or a
	// refresh race rather than a revoked login, so it cools briefly and only
	// escalates after repeated hits (wbAuthFailsMax).
	wbSessionDeadCode = 12153
	// wbBadBodyCode is the upstream refusing a malformed request body. The
	// request is the caller's fault, not the account's: rotating accounts
	// replays the identical malformed body, so it must never be charged
	// against the account.
	wbBadBodyCode = 11101
)

var (
	wbResetTimeRe = regexp.MustCompile(`(?:reset at|将在)\s*(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2})`)
	wbResetTZRe   = regexp.MustCompile(`UTC([+-])(\d{1,2})(?::?(\d{2}))?`)
	wbSoftLimitRe = regexp.MustCompile(`(?i)usage exceeds frequency limit|too many requests|请求过于频繁`)
	wbSessionRe   = regexp.MustCompile(`(?i)offline user session not found|session not found|session expired`)
	wbBadBodyRe   = regexp.MustCompile(`(?i)unmarshal chat params failed|invalid request body|malformed`)
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
		// 6004 names ONE model's daily window (its message carries that
		// model's reset clock), so the refusal is scoped to the model: the
		// account's other models keep working, which is the difference between
		// a pool that helps and one that benches an account for a whole day.
		wait := wbResetWait(payload)
		if wait <= 0 {
			wait = wbCapDefaultWait
		}
		if retryAfterSec > 0 && retryAfterSec < wait {
			wait = retryAfterSec
		}
		msg := wbMessageFromPayload(payload)
		if !strings.Contains(msg, "窗口") {
			// The upstream only says it in English; name the condition in the
			// card's own language so the line reads as a status, not a relay.
			msg = "免费窗口已用满：" + msg
		}
		return &UpstreamError{Code: CodeQuota, Message: msg, RetryAfter: wait, Scope: ScopeModel}
	case code == wbSoftLimitCode || wbSoftLimitRe.MatchString(payload):
		return &UpstreamError{Code: CodeQuota, Message: wbMessageFromPayload(payload), RetryAfter: wbSoftCooldown, Scope: ScopeModel}
	case status == 402:
		return &UpstreamError{Code: CodeQuota,
			Message:    wbMessageFromPayload(payload) + "（WorkBuddy 余额耗尽，免费模型不受影响）",
			RetryAfter: wbSecondsUntil4AM(), Scope: ScopeAccount}
	case status == 401:
		// Unconditional: the front gateway answers a dead credential with an
		// openresty HTML page, and the HTML special case downstream would
		// misfile it as a request fault — exactly the one status that must
		// trigger the refresh-and-retry path.
		return &UpstreamError{Code: CodeCredential, Message: "WorkBuddy 登录态失效（上游 401）"}
	case code == 11140 || code == 11128:
		return &UpstreamError{Code: wbCodeSanitize,
			Message: "上游内容门拦截（" + strconv.Itoa(code) + "）"}
	case code == wbBadBodyCode || wbBadBodyRe.MatchString(payload):
		// The body itself is malformed: every account answers the same 400, so
		// this is filed as the request's fault and carries no account penalty.
		return &UpstreamError{Code: CodeClient,
			Message: "上游拒绝请求体（" + strconv.Itoa(code) + "）: " + wbMessageFromPayload(payload)}
	case code == wbSessionDeadCode || wbSessionRe.MatchString(payload):
		// Soft: one 12153 is routinely a refresh race or a dropped connection.
		// The caller counts hits and escalates after wbAuthFailsMax.
		return &UpstreamError{Code: CodeCredential, Soft: true,
			Message: "WorkBuddy 会话失效（" + strconv.Itoa(code) + "）"}
	}
	return ClassifyFailure(status, payload, retryAfterSec)
}

// wbMessageFromPayload turns one refusal body into the line a person reads.
//
// The chat gate ships {"code":14003,"msg":"too many requests","requestId":"…",
// "displayMsg":{"en":"Model busy. Please switch models or retry later",
// "zh":"模型繁忙，请换模型或稍后再试"}}. Handing that JSON to the card was wrong
// twice over: it is a wire envelope, not a sentence, and it made the card read
// English while the Chinese the upstream itself wrote sat in the same body.
// The zh line wins, then en, then msg; the requestId is dropped because a
// per-request id is a support handle, not a status the user can act on.
func wbMessageFromPayload(payload string) string {
	var env struct {
		Code       int    `json:"code"`
		Msg        string `json:"msg"`
		Message    string `json:"message"`
		DisplayMsg struct {
			En string `json:"en"`
			Zh string `json:"zh"`
		} `json:"displayMsg"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	trimmed := strings.TrimSpace(payload)
	if json.Unmarshal([]byte(trimmed), &env) != nil {
		return truncate(trimmed, 200)
	}
	say := env.DisplayMsg.Zh
	if say == "" {
		say = env.DisplayMsg.En
	}
	if say == "" {
		say = env.Msg
	}
	if say == "" {
		// 6004 ships its reset clock in a top-level "message", and that half
		// sentence is the part the user can act on.
		say = env.Message
	}
	if say == "" {
		say = env.Error.Message
	}
	code := env.Code
	if code == 0 {
		code = env.Error.Code
	}
	if say == "" {
		if code != 0 {
			return "上游返回 code " + strconv.Itoa(code)
		}
		return truncate(trimmed, 200)
	}
	if code != 0 && !strings.Contains(say, strconv.Itoa(code)) {
		say += "（上游 code " + strconv.Itoa(code) + "）"
	}
	return truncate(say, 200)
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
