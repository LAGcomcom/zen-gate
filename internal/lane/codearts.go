package lane

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"strconv"
	"time"

	"zen-gate/internal/codearts/auth"
	"zen-gate/internal/codearts/upstream"
)

// The CodeArts 账号渠道: a credential-bearing lane source for Huawei Cloud's
// CodeArts Agent (盘古助手) free tier, ported from codearts2api (MIT). The
// wire protocol lives in internal/codearts/upstream; this file is the lane's
// adapter — account snapshot, catalog rows, one-turn post — following the same
// shape as the WorkBuddy channel.
//
// Unlike Kilo (keyless) and WorkBuddy (a bearer token), CodeArts authenticates
// each request twice: an STS security token in x-auth-token AND a full AK/SK
// HMAC signature over the canonical request. The login is a Huawei Cloud
// OAuth2 PKCE round-trip whose refresh token is bound to a DPoP keypair and the
// original code_verifier, so all three must persist together or refresh fails
// "InvalidDPoPHeader".
//
// The roster is not static: CodeArts rotates a 限时福利 (free-plan) set per
// account alongside the builtin models, and a benefit model refused without the
// maas_type: benefit header answers 404 "not registered". So discovery is a
// live fetch gated on the account, and the upstream package keeps the per-
// account benefit flags that the post path reads back by model id.
//
// Prompts may be logged by the upstream; the blurb says so in front of the
// user, same discipline as the Kilo pool and WorkBuddy.

var (
	// CodeArtsSnapBase / CodeArtsBenefitBase are the upstream roots, overridden
	// together for tests so one httptest server can stub both paths. The
	// login chain's three hosts (portal authorize page, snap-manager ticket
	// poll, STS token exchange) are overridable for the same reason: an
	// httptest URL serves all of them.
	CodeArtsSnapBase    = upstream.SnapEngineApiHost
	CodeArtsBenefitBase = upstream.BenefitHost
	CodeArtsPortalBase  = upstream.PortalHost
	CodeArtsManagerBase = upstream.SnapManagerHost
	CodeArtsSTSBase     = upstream.STSHost
)

const (
	caTurnTimeout  = streamIdleTimeout
	caRefreshEarly = 20 * time.Minute // STS tokens are short-lived; renew early
)

// CAAccount is one CodeArts login. store.Config persists it verbatim; the lane
// keeps a snapshot and writes rotated credentials back through the persist
// callback. SecurityToken/AccessKeyID/SecretAccessKey are STS temporary
// credentials; RefreshToken/CodeVerifier/DPoPKey are what renew them and must
// survive a restart.
type CAAccount struct {
	UserID          string            `json:"userId,omitempty"`
	UserName        string            `json:"userName,omitempty"`
	DomainID        string            `json:"domainId,omitempty"`
	SecurityToken   string            `json:"securityToken,omitempty"`
	AccessKeyID     string            `json:"accessKeyId,omitempty"`
	SecretAccessKey string            `json:"secretAccessKey,omitempty"`
	ExpirationMS    int64             `json:"expirationMs,omitempty"`
	RefreshToken    string            `json:"refreshToken,omitempty"`
	CodeVerifier    string            `json:"codeVerifier,omitempty"`
	ClientID        string            `json:"clientId,omitempty"`
	DPoPKey         map[string]string `json:"dpopKey,omitempty"`
	Enabled         bool              `json:"enabled"`
	// LastCheckinDate is the local calendar day the daily credit check-in
	// claimed, so the loop runs it at most once per day.
	LastCheckinDate string `json:"lastCheckinDate,omitempty"`
}

// auth rebuilds the protocol-package credential. Empty expiry writes no
// expiration so the package's Expired() defers to an upstream 401 instead of
// reading a zero time as "always expired".
func (a CAAccount) auth() *auth.Auth {
	exp := ""
	if a.ExpirationMS > 0 {
		exp = time.UnixMilli(a.ExpirationMS).UTC().Format(time.RFC3339)
	}
	au := auth.New(a.UserID, a.UserName, a.DomainID, a.SecurityToken,
		a.AccessKeyID, a.SecretAccessKey, exp, a.RefreshToken, a.CodeVerifier, a.DPoPKey)
	if a.ClientID != "" {
		au.SetClientID(a.ClientID)
	}
	return au
}

// caAccountFromToken rebuilds the persisted snapshot from a login/refresh
// TokenResponse, keeping the login-time bindings (verifier, DPoP key, client
// id) that the response does not carry — those never change across a refresh.
func caAccountFromToken(prev CAAccount, tok *upstream.TokenResponse) CAAccount {
	cred := tok.Credentials
	// The legacy ticket channel nests the pair under `credential` with other
	// field names; NormalizeCredential collapses both shapes.
	if cred.SecurityToken == "" && tok.Credential.SecurityToken != "" {
		cred = upstream.Credentials{
			AccessKeyID:     tok.Credential.Access,
			SecretAccessKey: tok.Credential.Secret,
			SecurityToken:   tok.Credential.SecurityToken,
			Expiration:      tok.Credential.ExpiresAt,
		}
	}
	out := prev
	out.UserID, out.UserName, out.DomainID = tok.UserID, tok.UserName, tok.DomainID
	out.SecurityToken = cred.SecurityToken
	out.AccessKeyID = cred.AccessKeyID
	out.SecretAccessKey = cred.SecretAccessKey
	if tok.RefreshToken != "" {
		out.RefreshToken = tok.RefreshToken
	}
	if exp := parseCodeArtsExpiry(cred.Expiration); exp > 0 {
		out.ExpirationMS = exp
	}
	return out
}

// parseCodeArtsExpiry reads the STS expiration stamp; RFC3339 with and without
// a fractional seconds component both appear upstream.
func parseCodeArtsExpiry(s string) int64 {
	if s == "" {
		return 0
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}

func caLoginConfig(a CAAccount) upstream.LoginConfig {
	cfg := upstream.DefaultLoginConfig()
	cfg.PortalHost = CodeArtsPortalBase
	cfg.SnapManager = CodeArtsManagerBase
	cfg.STSHost = CodeArtsSTSBase
	if a.ClientID != "" {
		cfg.ClientID = a.ClientID
	}
	return cfg
}

// caTokenExpiringSoon is the proactive-refresh trigger on the persisted
// snapshot (no need to build an auth value just to read the clock).
func caTokenExpiringSoon(a CAAccount) bool {
	if a.ExpirationMS <= 0 {
		return false
	}
	return time.Now().UnixMilli()+caRefreshEarly.Milliseconds() >= a.ExpirationMS
}

// caClient builds a protocol client on the lane's proxy-aware transport.
func caClient() *upstream.Client {
	return upstream.NewWithBases(laneHTTP.Transport, CodeArtsSnapBase, CodeArtsBenefitBase)
}

// caFetchModels discovers the account's roster (builtin + benefit + agent
// center). A side effect records the per-account benefit flags in the upstream
// package, so caIsBenefit answers without the lane keeping its own copy.
func caFetchModels(a CAAccount) ([]ModelInfo, error) {
	infos, err := caClient().FetchModels(a.auth())
	if err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(infos))
	for _, m := range infos {
		out = append(out, ModelInfo{
			ID:                 m.ID,
			Name:               m.Name,
			Blurb:              caBlurb(m.Benefit),
			Wire:               "chat",
			Reasoning:          true,
			CanDisableThinking: true,
			ContextWindow:      int(m.ContextWindow),
			MaxOutput:          int(m.MaxTokens),
			Channel:            ChannelCodeArts,
		})
	}
	return out, nil
}

func caBlurb(benefit bool) string {
	if benefit {
		return "华为云 CodeArts 限时福利模型（免费套餐），提示词可能被上游记录。"
	}
	return "华为云 CodeArts 免费模型，提示词可能被上游记录。"
}

// caIsBenefit reports whether one catalog id needs the maas_type: benefit
// header on its turn.
func caIsBenefit(a CAAccount, model string) bool {
	return upstream.IsBenefitModel(a.UserID, model)
}

// caChatID maps one downstream conversation onto one 32-hex chat id: the
// upstream demands that shape and the id rides as prompt-cache affinity, so
// one conversation must keep one id across its turns.
func caChatID(seed string) string {
	sum := md5.Sum([]byte("codearts-chat:" + seed))
	return hex.EncodeToString(sum[:])
}

// caPostOnce performs one physical CodeArts chat turn and feeds the SSE stream
// through the shared reader, so the lane's decoder sees the same frame shape as
// any OpenAI-compatible source.
func caPostOnce(ctx context.Context, body map[string]any, a CAAccount, chatID string, benefit bool, onData func([]byte) error) (*Usage, error) {
	stream, err := caClient().SendChatV2Chat(ctx, body, "", caCred(a), a.SecurityToken, chatID, benefit)
	if err != nil {
		return nil, classifyCodeArts(err)
	}
	defer stream.Close()
	pump := newStreamReader(ctx, stream, caTurnTimeout)
	defer pump.Close()
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

// caCred is the AK/SK signing credential; the security token doubles as the
// x-auth-token and the STS signature's session header.
func caCred(a CAAccount) upstream.SignCredential {
	return upstream.SignCredential{
		AccessKeyID:     a.AccessKeyID,
		SecretAccessKey: a.SecretAccessKey,
		SecurityToken:   a.SecurityToken,
	}
}

// classifyCodeArts maps a protocol-package error onto the lane's shared
// vocabulary. The benefit/param refusals are request defects (not retryable);
// 401/403 is the security token dying mid-conversation, which the account path
// answers by refreshing once.
func classifyCodeArts(err error) *UpstreamError {
	pe, ok := err.(*upstream.ApiError)
	if !ok {
		return &UpstreamError{Code: CodeTransport, Message: truncate(err.Error(), 300)}
	}
	switch {
	case pe.Status == 401 || pe.Status == 403:
		return &UpstreamError{Code: CodeCredential, Message: "CodeArts 登录态失效（上游 " + strconv.Itoa(pe.Status) + "）"}
	case pe.Status == 429:
		return &UpstreamError{Code: CodeQuota, Message: truncate(pe.Message, 300), RetryAfter: 60}
	case pe.Status == 400 || pe.Status == 404:
		return &UpstreamError{Code: CodeClient, Message: truncate(pe.Message, 300)}
	default:
		return &UpstreamError{Code: CodeServer, Message: truncate(pe.Message, 300)}
	}
}

// caCanonicalModel resolves a catalog id to the exact registered name the
// upstream matches (case-sensitive), falling back to the id itself.
func caCanonicalModel(id string) string {
	return upstream.CanonicalModel(id)
}
