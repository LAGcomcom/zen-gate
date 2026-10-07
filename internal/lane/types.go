// Package lane speaks to the OpenCode Zen free lane (https://opencode.ai).
//
// The wire behaviour ported here was reverse-engineered and documented by the
// MIT-licensed dsh-our-free-model plugin (github.com/zouyuxuan122/dsh-our-free-model);
// every constant and format matches that implementation so the two stay
// interchangeable against the same gateway.
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

// Chunk is a normalized stream piece, mirroring the harness StreamChunk shape
// used by dsh-our-free-model.
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
}

func (e *UpstreamError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// Classification codes, matching dsh-our-free-model's CODE table.
const (
	CodeRegion     = "REGION_BLOCKED"
	CodeQuota      = "RATE_LIMIT"
	CodeCredential = "INVALID_CREDENTIAL"
	CodeTransport  = "TRANSPORT"
	CodeTimeout    = "TIMEOUT"
	CodeServer     = "SERVER"
	CodeEmpty      = "EMPTY_RESPONSE"
	CodeAborted    = "ABORTED"
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
