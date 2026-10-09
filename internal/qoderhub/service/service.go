package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"zen-gate/internal/qoderhub/remote"
	"zen-gate/internal/qoderhub/tooltypes"
)

type SessionMode string

const (
	SessionModeAuto  SessionMode = "auto"
	SessionModeFresh SessionMode = "fresh"
	SessionModeReuse SessionMode = "reuse"
)

type Config struct {
	Host           string
	Port           int
	RemoteBaseURL  string
	RemoteAuthFile string
	RemoteProxyURL string
	RemoteVersion  string
	Model          string
	SessionMode    SessionMode
	Timeout        time.Duration
	WarmupTimeout  time.Duration
	// AuthKeysFile points at an inbound API-key allowlist (one key per line).
	// Empty disables inbound authentication (open access, the default).
	AuthKeysFile string
}

type Image struct {
	MediaType string // e.g. "image/jpeg", "image/png"
	Data      string // base64 encoded data without prefix
	URL       string // optional original URL
}

type ChatMessage struct {
	Role       string
	Text       string
	Images     []Image
	ToolCallID string
	ToolCalls  []tooltypes.ToolCall
	// ReasoningText carries an assistant turn's prior thinking so extended
	// thinking survives multi-turn round-trips (forwarded as reasoning_content).
	ReasoningText string
}

type ChatRequest struct {
	Model             string
	System            string
	Messages          []ChatMessage
	Tools             []tooltypes.ToolDef
	ToolChoice        tooltypes.ToolChoice
	ParallelToolCalls *bool

	// Generation parameters (passed through for API compatibility; the gateway
	// honors only some — max_tokens/stop are enforced proxy-side, see limiter.go).
	Temperature      *float64
	TopP             *float64
	TopK             int
	Stop             []string
	PresencePenalty  float64
	FrequencyPenalty float64
	MaxTokens        int
	Seed             int
	User             string
	ReasoningEffort  string
	ResponseFormat   string // "json" or "json_schema"
}

type ChatResult struct {
	Text              string
	ThoughtText       string
	Model             string
	InputTokens       int
	OutputTokens      int
	SessionID         string
	RequestID         string
	FinishReason      string
	StopSequence      string
	UsedTokens        int
	LimitTokens       int
	CachedInputTokens int
	// CacheCreationInputTokens is the Anthropic cache-write count. QoderCN never
	// reports it (stays 0); kept for protocol completeness and future backends.
	CacheCreationInputTokens int
	ReasoningTokens          int
	// Credits is the QoderCN gateway's real per-request charge; OriginalCredits is
	// the pre-discount charge and Billable whether the call was metered.
	Credits          float64
	OriginalCredits  float64
	Billable         bool
	Endpoint         string
	Transport        string
	EffectiveSession SessionMode
	ToolCalls        []tooltypes.ToolCall
}

type StreamEvent struct {
	Type     string
	Delta    string
	ToolCall *StreamToolCall
}

// StreamToolCall carries one incremental native tool-call fragment.
type StreamToolCall struct {
	Index        int
	ID           string
	Name         string
	ArgsFragment string
}

type StreamResult struct {
	Result *ChatResult
	Err    error
}

type Model struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Scene      string `json:"scene,omitempty"`
	InternalID string `json:"-"`
	// Raw is the full upstream model object, forwarded verbatim to downstream so
	// no gateway-provided field is dropped.
	Raw map[string]any `json:"-"`
}

type State struct {
	Endpoint    string      `json:"endpoint,omitempty"`
	Transport   string      `json:"transport,omitempty"`
	Connected   bool        `json:"connected"`
	SessionMode SessionMode `json:"session_mode"`
}

type Service struct {
	cfg            Config
	mu             sync.Mutex
	remoteClient   *remote.Client
	remoteModelsMu sync.Mutex
	remoteModels   []remote.Model
	remoteModelsAt time.Time
}

const (
	StreamEventText     = "text"
	StreamEventThinking = "thinking"
	StreamEventToolCall = "tool_call"
)

func New(cfg Config) *Service {
	cfg.Model = normalizeRemoteModel(strings.TrimSpace(cfg.Model))
	if cfg.SessionMode == "" {
		cfg.SessionMode = SessionModeAuto
	}
	return &Service{cfg: cfg}
}

func (s *Service) SetDefaultModel(model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Model = normalizeRemoteModel(model)
}

func (s *Service) DefaultModel() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(s.cfg.Model)
}

func (s *Service) Warmup(ctx context.Context) error {
	return s.remoteClientLocked().Warmup(ctx)
}

func (s *Service) Close() error { return nil }

func contextWithOptionalTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, timeout)
}

func (s *Service) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return State{
		Endpoint:    remote.ResolveBaseURL(s.cfg.RemoteBaseURL),
		Transport:   "remote",
		Connected:   s.remoteClient != nil,
		SessionMode: s.cfg.SessionMode,
	}
}

func (s *Service) ListModels(ctx context.Context) ([]Model, error) {
	models, err := s.remoteClientLocked().ListModels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(models))
	seen := map[string]bool{}
	for _, model := range models {
		key := strings.TrimSpace(model.Key)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		name := strings.TrimSpace(model.DisplayName)
		if name == "" {
			name = key
		}
		// Expose the display name as the id (so downstream sees "GLM-5.2" not the
		// opaque key "gm51model"); the key is kept in InternalID for resolution.
		out = append(out, Model{ID: name, Name: name, InternalID: key, Raw: model.Raw})
	}
	// Trust the gateway's list as authoritative; no hardcoded fallback probing.
	return out, nil
}

// Quota returns the account credit/usage snapshot.
func (s *Service) Quota(ctx context.Context) (*remote.Quota, error) {
	return s.remoteClientLocked().FetchQuota(ctx)
}

// WebSearch runs a web search via the gateway's oneSearch endpoint, servicing
// Claude Code's hosted web_search tool.
func (s *Service) WebSearch(ctx context.Context, query string, opts remote.WebSearchOptions) ([]remote.SearchResult, error) {
	return s.remoteClientLocked().WebSearch(ctx, query, opts)
}

// ImageSearch runs an image search via the gateway.
func (s *Service) ImageSearch(ctx context.Context, query string, count int) ([]remote.ImageResult, error) {
	return s.remoteClientLocked().ImageSearch(ctx, query, count)
}

// GenerateImage generates an image via the gateway, returning a data URL.
func (s *Service) GenerateImage(ctx context.Context, prompt, size, model string) (string, error) {
	return s.remoteClientLocked().GenerateImage(ctx, prompt, size, model)
}

// PolishText cleans up raw text via the gateway: adds punctuation and fixes
// casing/spacing without changing the meaning.
func (s *Service) PolishText(ctx context.Context, text string) (string, error) {
	return s.remoteClientLocked().PolishText(ctx, text)
}

// EmulatesTextTools reports whether tool calls surface as text action blocks
// rather than native structured tool_calls. The gateway supports native
// function-calling, so this is always false; the method stays for the stream
// filter call sites, which become pass-throughs.
func (s *Service) EmulatesTextTools(req ChatRequest) bool { return false }

func (s *Service) Generate(ctx context.Context, req ChatRequest) (*ChatResult, error) {
	result, err := s.generateRemote(ctx, req, nil)
	if err == nil && result != nil {
		// The gateway ignores max_tokens/stop, so enforce them on the output here.
		if lim := newOutputLimiter(req.MaxTokens, req.Stop); lim.enabled() {
			truncated := lim.apply(result.Text)
			if lim.triggered() {
				result.Text = truncated
				applyLimiterFinish(result, lim)
			}
		}
	}
	return result, err
}

// applyLimiterFinish records the proxy-enforced finish reason (canonical
// OpenAI-style "length"/"stop"; Anthropic stop_reason is derived downstream).
func applyLimiterFinish(result *ChatResult, lim *outputLimiter) {
	result.FinishReason = lim.openAIFinish()
	result.StopSequence = lim.stopSeq
}

func (s *Service) GenerateStream(ctx context.Context, req ChatRequest) (<-chan StreamEvent, <-chan StreamResult, error) {
	events := make(chan StreamEvent, 256)
	done := make(chan StreamResult, 1)

	go func() {
		lim := newOutputLimiter(req.MaxTokens, req.Stop)
		send := func(ev StreamEvent) {
			select {
			case events <- ev:
			case <-ctx.Done():
			}
		}
		result, err := s.generateRemote(ctx, req, func(event StreamEvent) {
			if event.Type == StreamEventText {
				if !lim.enabled() {
					if event.Delta != "" {
						send(event)
					}
					return
				}
				if lim.triggered() {
					return // output limit already reached; drop further text
				}
				if emit := lim.Push(event.Delta); emit != "" {
					send(StreamEvent{Type: StreamEventText, Delta: emit})
				}
				return
			}
			if event.Delta == "" && event.ToolCall == nil {
				return
			}
			send(event)
		})
		// Emit any held-back tail if the stream ended without hitting a limit.
		if lim.enabled() && !lim.triggered() {
			if tail := lim.Flush(); tail != "" {
				send(StreamEvent{Type: StreamEventText, Delta: tail})
			}
		}
		// Reflect proxy-side truncation on the final result.
		if err == nil && result != nil && lim.triggered() {
			result.Text = lim.text()
			applyLimiterFinish(result, lim)
		}

		close(events)
		done <- StreamResult{Result: result, Err: err}
		close(done)
	}()

	return events, done, nil
}

func (s *Service) generateRemote(ctx context.Context, req ChatRequest, onDelta func(StreamEvent)) (*ChatResult, error) {
	if strings.TrimSpace(req.Model) == "" {
		req.Model = s.DefaultModel()
	}
	req.Model = s.resolveRemoteModel(ctx, req.Model)
	prompt, err := buildLingmaPrompt(req)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, errors.New("empty user message")
	}

	attemptCtx, cancel := contextWithOptionalTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	return s.chatOnce(attemptCtx, s.remoteClientLocked(), req, prompt, req.Model, onDelta)
}

func (s *Service) chatOnce(
	ctx context.Context,
	client *remote.Client,
	req ChatRequest,
	prompt string,
	model string,
	onDelta func(StreamEvent),
) (*ChatResult, error) {
	delta := func(ev remote.StreamEvent) {
		if ev.Kind == remote.StreamKindToolCall {
			if ev.ToolCall == nil {
				return
			}
			if onDelta != nil {
				onDelta(StreamEvent{Type: StreamEventToolCall, ToolCall: &StreamToolCall{
					Index:        ev.ToolCall.Index,
					ID:           ev.ToolCall.ID,
					Name:         ev.ToolCall.Name,
					ArgsFragment: ev.ToolCall.ArgsFragment,
				}})
			}
			return
		}
		if ev.Delta == "" {
			return
		}
		eventType := StreamEventText
		if ev.Kind == remote.StreamKindReasoning {
			eventType = StreamEventThinking
		}
		if onDelta != nil {
			onDelta(StreamEvent{Type: eventType, Delta: ev.Delta})
		}
	}
	remoteResult, err := client.Chat(ctx, remote.ChatRequest{
		Model:           model,
		Prompt:          prompt,
		Messages:        remoteMessagesFromRequest(req),
		Images:          remoteImagesFromRequest(req),
		Stream:          onDelta != nil,
		Temperature:     req.Temperature,
		TopP:            req.TopP,
		TopK:            req.TopK,
		Stop:            req.Stop,
		MaxTokens:       req.MaxTokens,
		ReasoningEffort: req.ReasoningEffort,
		Tools:           req.Tools,
		ToolChoice:      req.ToolChoice,
	}, delta)
	if err != nil {
		return nil, err
	}
	if len(remoteResult.ToolCalls) == 0 && shouldRetryRemoteNativeTool(req, remoteResult.Text) {
		retryResult, retryErr := client.Chat(ctx, remote.ChatRequest{
			Model:           model,
			Prompt:          prompt,
			Messages:        remoteMessagesFromRequest(req),
			Images:          remoteImagesFromRequest(req),
			Stream:          false,
			Temperature:     req.Temperature,
			TopP:            req.TopP,
			TopK:            req.TopK,
			Stop:            req.Stop,
			MaxTokens:       req.MaxTokens,
			ReasoningEffort: req.ReasoningEffort,
			Tools:           req.Tools,
			ToolChoice:      tooltypes.ToolChoice{Mode: "any"},
		}, nil)
		if retryErr == nil && len(retryResult.ToolCalls) > 0 {
			remoteResult = retryResult
		}
	}

	if remoteResult.TotalTokens > 0 || remoteResult.Credits > 0 {
		log.Printf("remote usage model=%s in=%d out=%d cached=%d reasoning=%d total=%d credits=%.4f",
			model, remoteResult.InputTokens, remoteResult.OutputTokens,
			remoteResult.CachedInputTokens, remoteResult.ReasoningTokens,
			remoteResult.TotalTokens, remoteResult.Credits)
	}
	finishReason := valueOr(strings.TrimSpace(remoteResult.FinishReason), "stop")
	return &ChatResult{
		Text:              remoteResult.Text,
		ThoughtText:       remoteResult.ReasoningText,
		Model:             valueOr(strings.TrimSpace(model), "lingma"),
		InputTokens:       remoteResult.InputTokens,
		OutputTokens:      remoteResult.OutputTokens,
		CachedInputTokens: remoteResult.CachedInputTokens,
		ReasoningTokens:   remoteResult.ReasoningTokens,
		UsedTokens:        remoteResult.TotalTokens,
		Credits:           remoteResult.Credits,
		OriginalCredits:   remoteResult.OriginalCredits,
		Billable:          remoteResult.Billable,
		RequestID:         remoteResult.RequestID,
		FinishReason:      finishReason,
		Endpoint:          remote.ResolveBaseURL(s.cfg.RemoteBaseURL),
		Transport:         "remote",
		EffectiveSession:  SessionModeFresh,
		ToolCalls:         remoteResult.ToolCalls,
	}, nil
}

// cachedRemoteModels returns the gateway's model list, refetched at most every
// few minutes; on error it returns the last good snapshot (or nil).
func (s *Service) cachedRemoteModels(ctx context.Context) []remote.Model {
	s.remoteModelsMu.Lock()
	cached := s.remoteModels
	fresh := cached != nil && time.Since(s.remoteModelsAt) < 5*time.Minute
	s.remoteModelsMu.Unlock()
	if fresh {
		return cached
	}
	models, err := s.remoteClientLocked().ListModels(ctx)
	if err != nil || len(models) == 0 {
		return cached
	}
	s.remoteModelsMu.Lock()
	s.remoteModels = models
	s.remoteModelsAt = time.Now()
	s.remoteModelsMu.Unlock()
	return models
}

// resolveRemoteModel maps the requested model to a gateway key via the cached
// model list (key or display-name match), falling back to the static alias map.
func (s *Service) resolveRemoteModel(ctx context.Context, model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	models := s.cachedRemoteModels(ctx)
	for _, m := range models {
		if strings.EqualFold(model, strings.TrimSpace(m.Key)) {
			return strings.TrimSpace(m.Key)
		}
	}
	for _, m := range models {
		if strings.EqualFold(model, strings.TrimSpace(m.DisplayName)) {
			return strings.TrimSpace(m.Key)
		}
	}
	return normalizeRemoteModel(model)
}

func (s *Service) remoteClientLocked() *remote.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.remoteClient == nil {
		s.remoteClient = remote.New(remote.Config{
			BaseURL:     s.cfg.RemoteBaseURL,
			AuthFile:    s.cfg.RemoteAuthFile,
			ProxyURL:    s.cfg.RemoteProxyURL,
			CosyVersion: s.cfg.RemoteVersion,
			Timeout:     s.cfg.Timeout,
		})
	}
	return s.remoteClient
}

func remoteMessagesFromRequest(req ChatRequest) []remote.Message {
	out := make([]remote.Message, 0, len(req.Messages)+1)
	if system := strings.TrimSpace(req.System); system != "" {
		out = append(out, remote.Message{Role: "system", Content: system})
	}
	for _, message := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role == "" {
			continue
		}
		content := strings.TrimSpace(message.Text)
		reasoning := strings.TrimSpace(message.ReasoningText)
		if content == "" && reasoning == "" && len(message.Images) == 0 && len(message.ToolCalls) == 0 {
			continue
		}
		out = append(out, remote.Message{
			Role:          role,
			Content:       content,
			Images:        remoteImagesFromChatMessage(message),
			ToolCallID:    strings.TrimSpace(message.ToolCallID),
			ToolCalls:     message.ToolCalls,
			ReasoningText: reasoning,
		})
	}
	return out
}

func remoteImagesFromChatMessage(message ChatMessage) []remote.Image {
	if len(message.Images) == 0 {
		return nil
	}
	images := make([]remote.Image, 0, len(message.Images))
	for _, img := range message.Images {
		if strings.TrimSpace(img.Data) == "" && strings.TrimSpace(img.URL) == "" {
			continue
		}
		images = append(images, remote.Image{
			MediaType: strings.TrimSpace(img.MediaType),
			Data:      img.Data,
			URL:       strings.TrimSpace(img.URL),
		})
	}
	return images
}

func remoteImagesFromRequest(req ChatRequest) []remote.Image {
	var images []remote.Image
	for _, message := range req.Messages {
		for _, img := range message.Images {
			if strings.TrimSpace(img.Data) == "" && strings.TrimSpace(img.URL) == "" {
				continue
			}
			images = append(images, remote.Image{
				MediaType: strings.TrimSpace(img.MediaType),
				Data:      img.Data,
				URL:       strings.TrimSpace(img.URL),
			})
		}
	}
	return images
}

func imagePromptFallback(req ChatRequest, imageMessageIndex int) string {
	for i := imageMessageIndex - 1; i >= 0; i-- {
		message := req.Messages[i]
		if strings.EqualFold(strings.TrimSpace(message.Role), "user") {
			if text := strings.TrimSpace(message.Text); text != "" {
				return "请只根据图片内容回答用户这条问题，忽略更早的对话历史：" + text
			}
		}
	}
	system := strings.TrimSpace(req.System)
	if system != "" && len([]rune(system)) <= 1000 {
		return "请只根据图片内容回答这条要求：" + system
	}
	return "请描述这张图片的主要内容。"
}

func shouldRetryRemoteNativeTool(req ChatRequest, text string) bool {
	if len(req.Tools) == 0 || req.ToolChoice.Mode == "none" {
		return false
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || len([]rune(trimmed)) > 180 {
		return false
	}
	lower := strings.ToLower(trimmed)
	cues := []string{
		"让我", "我来", "我将", "接下来", "继续", "查看", "检查", "搜索", "读取", "运行", "执行",
		"let me", "i'll", "i will", "next", "continue", "check", "inspect", "search", "read", "run",
	}
	hasCue := false
	for _, cue := range cues {
		if strings.Contains(lower, cue) {
			hasCue = true
			break
		}
	}
	if !hasCue {
		return false
	}
	return strings.HasSuffix(trimmed, ":") ||
		strings.HasSuffix(trimmed, "：") ||
		strings.Contains(trimmed, "：\n") ||
		strings.Contains(lower, "use ") ||
		strings.Contains(lower, "call ") ||
		strings.Contains(trimmed, "工具")
}

// buildLingmaPrompt renders a flattened prompt used as the gateway's fallback
// text and for the input-token estimate; the primary path sends the structured
// messages from remoteMessagesFromRequest.
func buildLingmaPrompt(req ChatRequest) (string, error) {
	messages := filteredMessages(req.Messages)
	var lastUser string
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			lastUser = messages[i].Text
			break
		}
	}
	if strings.TrimSpace(lastUser) == "" {
		if idx := latestImageMessageIndex(req.Messages); idx >= 0 {
			lastUser = imagePromptFallback(req, idx)
			messages = append(messages, ChatMessage{Role: "user", Text: lastUser})
		} else {
			return "", errors.New("no user message found in request")
		}
	}

	system := strings.TrimSpace(req.System)
	if reasoningHint := reasoningSystemHint(req.ReasoningEffort); reasoningHint != "" {
		if system == "" {
			system = reasoningHint
		} else {
			system = reasoningHint + "\n\n" + system
		}
	}

	if system == "" && len(messages) == 1 {
		return lastUser, nil
	}

	parts := make([]string, 0, len(messages)+4)
	if system != "" {
		parts = append(parts, "System instructions:", system)
	}
	parts = append(parts, "Conversation transcript:")
	for _, message := range messages {
		role := "User"
		if message.Role == "assistant" {
			role = "Assistant"
		}
		parts = append(parts, fmt.Sprintf("%s: %s", role, message.Text))
	}
	parts = append(parts, "Reply as the assistant to the latest user message only. Follow the system instructions and prior transcript naturally.")
	return strings.Join(parts, "\n\n"), nil
}

func latestImageMessageIndex(messages []ChatMessage) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if !strings.EqualFold(strings.TrimSpace(messages[i].Role), "user") {
			continue
		}
		if len(remoteImagesFromChatMessage(messages[i])) > 0 {
			return i
		}
	}
	return -1
}

func filteredMessages(messages []ChatMessage) []ChatMessage {
	out := make([]ChatMessage, 0, len(messages))
	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		text := strings.TrimSpace(message.Text)
		if text == "" {
			continue
		}
		// The structured path carries tool results with their tool role; for the
		// flattened fallback we surface them as plain user text.
		if role == "tool" {
			role = "user"
		}
		if role != "user" && role != "assistant" {
			continue
		}
		out = append(out, ChatMessage{Role: role, Text: text})
	}
	return out
}

func reasoningSystemHint(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "":
		return ""
	case "low":
		return "Reasoning mode is enabled. Think briefly but deliberately before answering. Do not reveal private chain-of-thought; only provide the final answer."
	case "high":
		return "Reasoning mode is enabled. Take extra time to reason carefully before answering. Do not reveal private chain-of-thought; only provide the final answer and any concise user-facing rationale."
	default:
		return "Reasoning mode is enabled. Think carefully before answering. Do not reveal private chain-of-thought; only provide the final answer and any concise user-facing rationale."
	}
}

func valueOr(value string, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}

// normalizeRemoteModel maps a few friendly display names to their gateway keys;
// unknown values pass through so the gateway's own resolution still applies.
func normalizeRemoteModel(model string) string {
	model = strings.TrimSpace(model)
	switch strings.ToLower(model) {
	case "":
		return ""
	case "kimi-k2.6":
		return "kmodel"
	case "minimax-m2.7":
		return "mmodel"
	case "qwen3-coder":
		return "dashscope_qwen3_coder"
	case "qwen3-max":
		return "dashscope_qwen_max_latest"
	case "qwen3-thinking":
		return "dashscope_qwen_plus_20250428_thinking"
	case "qwen3.6-plus":
		return "dashscope_qmodel"
	case "auto":
		return "org_auto"
	default:
		return model
	}
}
