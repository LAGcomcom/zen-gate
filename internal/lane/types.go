// Package lane speaks to the sources the gateway serves: the keyless Kilo free
// pool, the WorkBuddy 账号渠道 (intl) and the Huawei Cloud CodeArts 账号渠道.
// The OpenCode Zen free lane that this package was first written for is retired
// — it answers FreeTierError to every unauthenticated call.
//
// The wire behaviour here was settled against a live client: the three wire
// protocols (chat / responses / messages), the tool-name gate one channel still
// uses, and cut-stream recovery all reproduce what a genuine desktop client
// sends, so models answer without a per-user key.
package lane

import (
	"context"
	"fmt"
)

// Role of a unified message.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Part is one typed block of a unified message content.
type Part interface{ partMarker() }

// TextPart is plain text.
type TextPart struct{ Text string }

// ReasoningPart is the thinking an assistant turn produced, as the client
// replayed it. DeepSeek-family thinking mode rejects a replayed assistant turn
// whose reasoning_content was dropped ("The `reasoning_content` in the thinking
// mode must be passed back to the API"), so the text is carried through the
// unified model and put back on the chat wire (issue #99).
type ReasoningPart struct{ Text string }

// ImagePart is an image carried as a data URL ("data:image/png;base64,…").
type ImagePart struct{ DataURL string }

// AudioPart is an audio clip carried as base64 with a short format tag
// ("wav", "mp3") — the OpenAI input_audio shape.
type AudioPart struct {
	Data   string // base64, no data: prefix
	Format string // wav | mp3
}

// FilePart is a document carried as base64 (a PDF for the Claude wire, or any
// file_data payload on the OpenAI wires).
type FilePart struct {
	Name      string // original filename, may be empty
	MediaType string // e.g. application/pdf
	Data      string // base64, no data: prefix
}

// ToolCallPart is a tool invocation requested by the assistant.
type ToolCallPart struct {
	ID        string
	Name      string
	Arguments string // raw JSON object text
}

// ToolResultPart is the outcome of a tool call, fed back as input.
type ToolResultPart struct {
	ToolCallID string
	Text       string
	IsError    bool
}

func (TextPart) partMarker()       {}
func (ReasoningPart) partMarker()  {}
func (ImagePart) partMarker()      {}
func (AudioPart) partMarker()      {}
func (FilePart) partMarker()       {}
func (ToolCallPart) partMarker()   {}
func (ToolResultPart) partMarker() {}

// Message is a unified conversation message.
type Message struct {
	Role  string
	Parts []Part
}

// TextOf returns the concatenated text of a message's text parts.
func (m Message) TextOf() string {
	out := ""
	for _, p := range m.Parts {
		if t, ok := p.(TextPart); ok {
			out += t.Text
		}
	}
	return out
}

// ToolDef describes one callable tool advertised to the model.
type ToolDef struct {
	Name        string
	Description string
	Parameters  string // JSON schema, raw
}

// Usage is one call's token accounting.
type Usage struct {
	Input       int `json:"input"`
	Output      int `json:"output"`
	Reasoning   int `json:"reasoning,omitempty"`
	CacheRead   int `json:"cacheRead,omitempty"`
	CacheWrite  int `json:"cacheWrite,omitempty"`
	TotalTokens int `json:"totalTokens,omitempty"`
}

// Finish reasons.
const (
	FinishStop      = "stop"
	FinishToolCalls = "tool-calls"
	FinishMaxTokens = "max-tokens"
	FinishAborted   = "aborted"
)

// Chunk kinds emitted by Stream.
const (
	ChunkBlockStart    = "block-start"
	ChunkTextDelta     = "text-delta"
	ChunkReasonDelta   = "reasoning-delta"
	ChunkToolCallDelta = "tool-call-delta"
	ChunkBlockEnd      = "block-end"
	ChunkUsage         = "usage"
	ChunkFinish        = "finish"
)

// Chunk is a normalized stream piece.
type Chunk struct {
	Kind      string
	Index     int
	BlockType string // text | reasoning | tool-call
	Text      string
	Delta     string
	ID        string
	Name      string
	Usage     *Usage
	Finish    string
}

// Needs summarises what one request actually carries — the modality footprint
// the router matches against model capabilities before a failover switch.
type Needs struct {
	Image bool `json:"image,omitempty"`
	Audio bool `json:"audio,omitempty"`
	File  bool `json:"file,omitempty"`
	Tools bool `json:"tools,omitempty"`
	// PromptTokens is a rough whole-request estimate (text/4 + images + files);
	// only used to skip candidates whose context window clearly cannot fit.
	PromptTokens int `json:"promptTokens,omitempty"`
}

// AnyModality reports whether the request carries anything beyond plain text.
func (n Needs) AnyModality() bool { return n.Image || n.Audio || n.File }

// SummarizeNeeds walks unified messages and computes the request's Needs.
// Images count ~1k tokens each, files ~len/4, text ~len/4 — a deliberately
// coarse estimate that only has to catch obvious context overflows.
func SummarizeNeeds(messages []Message, toolCount int) Needs {
	var n Needs
	if toolCount > 0 {
		n.Tools = true
	}
	text := 0
	for _, m := range messages {
		for _, p := range m.Parts {
			switch t := p.(type) {
			case ImagePart:
				n.Image = true
				n.PromptTokens += 1024
			case AudioPart:
				n.Audio = true
				n.PromptTokens += len(t.Data) / 4
			case FilePart:
				n.File = true
				n.PromptTokens += len(t.Data) / 4
			case TextPart:
				text += len(t.Text)
			case ToolResultPart:
				text += len(t.Text)
			}
		}
	}
	n.PromptTokens += text / 4
	return n
}

// Request is one logical completion turn.
type Request struct {
	Model       string // base model id, no "(level)" suffix
	Effort      string // light | balanced | deep ("" → balanced for reasoning models)
	Messages    []Message
	Tools       []ToolDef
	MaxTokens   int    // 0 → default budget
	SessionSeed string // stable per-conversation identity; "" → shared
	TurnSeed    string // stable per-turn identity for request ids; "" → mint fresh
	Agent       string // caller label for stats (main key → "")
	// Needs describes the request's modality footprint; the failover
	// candidate filter matches it against model capabilities.
	Needs   Needs
	Context context.Context
	// OnAttempt is called before each upstream attempt with the model about to
	// be tried — including failover attempts. The gateway uses it to stamp the
	// served-by header at first byte.
	OnAttempt func(model string)
}

// Outcome summarises one Stream call.
type Outcome struct {
	Usage Usage
	// Finish is the terminal reason; "" means the upstream closed without one
	// (a cut stream).
	Finish string
	// Recovered reports that a cut pure-reasoning turn was continued with a
	// checkpoint request and produced an answer.
	Recovered bool
	// ServedModel is the model that actually produced this outcome — it
	// differs from Request.Model when a failover attempt took over.
	ServedModel string
	// Failovers counts extra models tried before this outcome.
	Failovers int
}

// UpstreamError carries a classified lane failure.
type UpstreamError struct {
	Code    string
	Message string
	// Unavailable marks a gateway refusal that names the model specifically.
	Unavailable bool
	RetryAfter  int // seconds, when the gateway supplied one
	// Exhausted marks that the lane had no further failover candidate left —
	// the gateway may offer this failure to user-added providers instead.
	Exhausted bool
	// UpstreamRID is the trace id the provider put on the response
	// (x-request-id / request-id). It is the only handle the provider's
	// support team can use to find a failed turn on their side.
	UpstreamRID string
	// Soft marks a credential refusal that is probably transient: the account
	// stays in the pool, only a short cooldown applies, and repeated hits
	// escalate to a real logout. WorkBuddy answers 12153 (and "Offline user
	// session not found") for network hiccups and refresh races too, so one
	// of those must not bench an account for good.
	Soft bool
	// Scope says how far a quota refusal reaches, which a pool must know: a
	// per-model window (6004 names one model's reset clock) leaves the
	// account's other models usable, while an account-level one (paid balance
	// gone) takes everything down. Empty means "the model named in the
	// request", which is the safe default — it never over-benches.
	Scope string
}

// Refusal scopes. See UpstreamError.Scope.
const (
	ScopeModel   = "model"
	ScopeAccount = "account"
)

func (e *UpstreamError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// Classification codes for lane failures. CodeClient marks a refusal that is
// the request's own fault (a WAF page, a 4xx the caller cannot fix by
// re-sending): replaying the identical body reproduces the identical refusal,
// so it stays outside the failover set.
const (
	CodeRegion     = "REGION_BLOCKED"
	CodeQuota      = "RATE_LIMIT"
	CodeCredential = "INVALID_CREDENTIAL"
	CodeTransport  = "TRANSPORT"
	CodeReset      = "TRANSPORT_RESET"
	CodeTimeout    = "TIMEOUT"
	CodeServer     = "SERVER"
	CodeClient     = "CLIENT_ERROR"
	CodeEmpty      = "EMPTY_RESPONSE"
	CodeAborted    = "ABORTED"
	// CodeFreeTier is the upstream free-tier gate refusing the whole lane
	// ("free tier can only be used from within OpenCode"). It is its own code
	// rather than a flavour of CodeRegion because the two need opposite
	// treatment on the egress axis: a region block is a verdict about the exit
	// IP's country, so another exit can answer it; this gate is decided before
	// the request body is even read, so it lands on every exit identically and
	// retrying them only burns the budget. Both stay switchable, which is what
	// lets the turn fall through to the Kilo pool instead of surfacing as 401.
	CodeFreeTier = "FREE_TIER_BLOCKED"
)

// traceKey carries the gateway's correlation id for one logical client request
// into the lane, so every physical attempt (including failovers the client
// never saw) can be reported back under the same id.
type traceKey struct{}

// WithTrace returns ctx carrying the request correlation id.
func WithTrace(ctx context.Context, rid string) context.Context {
	return context.WithValue(ctx, traceKey{}, rid)
}

// TraceFrom reads the correlation id, empty when the call was not made for a
// client request (probes, background work).
func TraceFrom(ctx context.Context) string {
	if v, ok := ctx.Value(traceKey{}).(string); ok {
		return v
	}
	return ""
}
