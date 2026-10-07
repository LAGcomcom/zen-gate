package gateway

import (
	"context"
	"net/http"
	"strings"
	"time"

	"zen-gate/internal/lane"
	"zen-gate/internal/logx"
	"zen-gate/internal/relay"
	"zen-gate/internal/store"
)

// Custom-provider ("自定义 API") relay paths. Model ids are namespaced as
// "<providerID>/<upstream model id>"; when a request names such an id the turn
// is forwarded to that provider instead of the free lane, and the response is
// normalized back into whatever wire the caller speaks.

// providerRoute resolves a namespaced model id onto its provider. Matching
// never collides with the free lane: zen model ids contain no "/", so a first
// segment that names a provider is unambiguous.
func (s *Server) providerRoute(model string) (*store.Provider, string, bool) {
	base := lane.BaseModelId(model)
	i := strings.Index(base, "/")
	if i <= 0 || i == len(base)-1 {
		return nil, "", false
	}
	id, upstream := base[:i], base[i+1:]
	cfg := s.Store.Config()
	for k := range cfg.Providers {
		if cfg.Providers[k].ID == id {
			return &cfg.Providers[k], upstream, true
		}
	}
	return nil, "", false
}

// relayError writes a provider failure in the caller's error shape.
func (s *Server) relayError(w http.ResponseWriter, anthropic bool, uerr *lane.UpstreamError) {
	setRetryAfter(w, uerr)
	if anthropic {
		writeJSON(w, errorStatus(uerr), map[string]any{"type": "error",
			"error": map[string]any{"type": errorType(uerr), "message": uerr.Message}})
		return
	}
	writeJSON(w, errorStatus(uerr), openaiError(uerr.Message, errorType(uerr)))
}

// recordRelay folds one relayed turn into the usage stats under its
// namespaced model id, so the heatmap and per-model tables cover providers too,
// and reports it as one upstream attempt of the request it served.
func (s *Server) recordRelay(ctx context.Context, p *store.Provider, upstream, agent string, ok bool, usage lane.Usage, ttftMs int64, finish string, err *lane.UpstreamError) {
	rec := lane.CallRecord{
		Model: p.ID + "/" + upstream, Agent: agent, Ok: ok && err == nil,
		Input: usage.Input, Output: usage.Output, Reasoning: usage.Reasoning,
		CacheRead: usage.CacheRead, TTFTMs: ttftMs, At: time.Now().UnixMilli(),
		Trace: lane.TraceFrom(ctx),
	}
	if err != nil {
		rec.ErrCode, rec.UpstreamRID = err.Code, err.UpstreamRID
	}
	if err == nil && usage.TotalTokens == 0 && ok {
		rec.NoUsage = true
	}
	if err == nil && !ok {
		rec.Truncated = true
	}
	s.Store.Record(rec)
	s.NoteCall(rec)
}

// relayChatCompletions serves an OpenAI chat turn from a provider.
func (s *Server) relayChatCompletions(w http.ResponseWriter, r *http.Request, req openaiChatRequest, p *store.Provider, upstream, agent string, unified []lane.Message) {
	ctx := r.Context()
	maxTok := req.MaxTokens
	if req.MaxCompletionTokens > 0 {
		maxTok = req.MaxCompletionTokens
	}
	emit := &chatEmitter{declared: declaredNames(req.Tools)}
	id := randomID("chatcmpl-")
	created := time.Now().Unix()
	served := p.ID + "/" + upstream

	doRelay := func(consume func(lane.Chunk)) (lane.Usage, string, *lane.UpstreamError, int64) {
		start := time.Now()
		var firstAt int64
		usage, finish, uerr := relay.Complete(ctx, relay.Turn{
			Provider: p, Model: upstream, Messages: unified,
			Tools: convertOpenAITools(req.Tools), MaxTokens: maxTok, Agent: agent,
		}, func(c lane.Chunk) {
			if firstAt == 0 {
				firstAt = time.Since(start).Milliseconds()
			}
			consume(c)
		})
		return usage, finish, uerr, firstAt
	}

	if !req.Stream {
		var text, reasoning strings.Builder
		var toolCalls []map[string]any
		usage, finish, uerr, ttft := doRelay(func(c lane.Chunk) { emit.consume(c, &text, &reasoning, &toolCalls) })
		s.recordRelay(ctx, p, upstream, agent, finish != "", usage, ttft, finish, uerr)
		if uerr != nil && text.Len() == 0 && len(toolCalls) == 0 {
			s.relayError(w, false, uerr)
			return
		}
		w.Header().Set("x-zen-gate-served-by", served)
		msg := map[string]any{"role": "assistant", "content": text.String()}
		if reasoning.Len() > 0 {
			msg["reasoning_content"] = reasoning.String()
		}
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
		}
		outFinish := "stop"
		if finish == "" {
			outFinish = "length"
		} else {
			outFinish = mapFinish(finish)
		}
		writeJSON(w, 200, map[string]any{
			"id": id, "object": "chat.completion", "created": created, "model": served,
			"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": outFinish}},
			"usage":   usagePayload(usage),
		})
		return
	}

	sse, err := newSSE(w)
	if err != nil {
		writeJSON(w, 500, openaiError(err.Error(), "internal_error"))
		return
	}
	sendChunk := func(delta map[string]any, finish any) {
		sse.event(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": served,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		})
	}
	sendChunk(map[string]any{"role": "assistant", "content": ""}, nil)
	usage, finish, uerr, ttft := doRelay(func(c lane.Chunk) {
		for _, d := range emit.stream(c) {
			sendChunk(d.delta, nil)
		}
	})
	s.recordRelay(ctx, p, upstream, agent, finish != "", usage, ttft, finish, uerr)
	if uerr != nil && !emit.hasAny() {
		s.relayError(w, false, uerr)
		return
	}
	if uerr != nil {
		sse.event(map[string]any{"error": map[string]any{
			"message": uerr.Message, "type": errorType(uerr), "code": uerr.Code}})
		sse.done()
		return
	}
	outFinish := "stop"
	if finish == "" {
		outFinish = "length"
	} else {
		outFinish = mapFinish(finish)
	}
	sendChunk(map[string]any{}, outFinish)
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		sse.event(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": served,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": nil}},
			"usage":   usagePayload(usage),
		})
	}
	sse.done()
}

// relayResponses serves an OpenAI Responses turn from a provider by converting
// it to the provider's chat wire; the collector reassembles Responses events.
func (s *Server) relayResponses(w http.ResponseWriter, r *http.Request, req responsesRequest, p *store.Provider, upstream, agent string, unified []lane.Message) {
	ctx := r.Context()
	served := p.ID + "/" + upstream
	collector := newResponsesCollector()
	start := time.Now()
	var firstAt int64
	usage, finish, uerr := relay.Complete(ctx, relay.Turn{
		Provider: p, Model: upstream, Messages: unified,
		Tools: convertOpenAITools(req.Tools), MaxTokens: req.MaxOutputTokens, Agent: agent,
	}, func(c lane.Chunk) {
		if firstAt == 0 {
			firstAt = time.Since(start).Milliseconds()
		}
		collector.consume(c)
	})
	s.recordRelay(ctx, p, upstream, agent, finish != "", usage, firstAt, finish, uerr)
	outcome := lane.Outcome{Usage: usage, Finish: finish, ServedModel: served}
	if uerr != nil && !collector.hasAnything() {
		s.relayError(w, false, uerr)
		return
	}
	w.Header().Set("x-zen-gate-served-by", served)
	if req.Stream {
		s.writeResponsesEvents(w, served, collector, outcome)
		return
	}
	writeJSON(w, 200, collector.finalResponse(served, outcome, false))
}

// relayAnthropicMessages serves an Anthropic /v1/messages turn from a provider
// (either protocol: openai providers get the messages converted to chat wire).
func (s *Server) relayAnthropicMessages(w http.ResponseWriter, r *http.Request, req anthropicRequest, p *store.Provider, upstream, agent string, unified []lane.Message, tools []lane.ToolDef) {
	ctx := r.Context()
	served := p.ID + "/" + upstream
	maxTok := req.MaxTokens
	if maxTok <= 0 {
		maxTok = 8192
	}
	st := &anthropicStreamState{blockMap: map[int]int{}}

	doRelay := func(consume func(lane.Chunk)) (lane.Usage, string, *lane.UpstreamError, int64) {
		start := time.Now()
		var firstAt int64
		usage, finish, uerr := relay.Complete(ctx, relay.Turn{
			Provider: p, Model: upstream, Messages: unified,
			Tools: tools, MaxTokens: maxTok, Agent: agent,
		}, func(c lane.Chunk) {
			if firstAt == 0 {
				firstAt = time.Since(start).Milliseconds()
			}
			consume(c)
		})
		return usage, finish, uerr, firstAt
	}

	if !req.Stream {
		collector := newResponsesCollector()
		usage, finish, uerr, ttft := doRelay(collector.consume)
		s.recordRelay(ctx, p, upstream, agent, finish != "", usage, ttft, finish, uerr)
		outcome := lane.Outcome{Usage: usage, Finish: finish, ServedModel: served}
		if uerr != nil && !collector.hasAnything() {
			s.relayError(w, true, uerr)
			return
		}
		w.Header().Set("x-zen-gate-served-by", served)
		writeJSON(w, 200, anthropicResponse(randomID("msg_"), served, collector, outcome))
		return
	}

	sse, err := newSSE(w)
	if err != nil {
		writeJSON(w, 500, map[string]any{"type": "error",
			"error": map[string]any{"type": "api_error", "message": err.Error()}})
		return
	}
	st.sse = sse
	sse.event(map[string]any{"type": "message_start", "message": map[string]any{
		"id": randomID("msg_"), "type": "message", "role": "assistant", "model": served,
		"content": []any{}, "stop_sequence": nil, "stop_reason": nil,
		"usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}})
	usage, finish, uerr, ttft := doRelay(st.consume)
	s.recordRelay(ctx, p, upstream, agent, finish != "", usage, ttft, finish, uerr)
	stop := "end_turn"
	switch {
	case finish == lane.FinishToolCalls:
		stop = "tool_use"
	case finish == lane.FinishMaxTokens || finish == "":
		stop = "max_tokens"
	}
	if uerr != nil && finish == "" && !st.sawAny {
		sse.event(map[string]any{"type": "error",
			"error": map[string]any{"type": errorType(uerr), "message": uerr.Message}})
		return
	}
	sse.event(map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": usage.Output}})
	sse.event(map[string]any{"type": "message_stop"})
}

// mintProviderID derives a unique, model-id-safe provider id from a display
// name: lowercase alphanumerics collapsed onto dashes ("NVIDIA NIM" →
// "nvidia-nim"), with a numeric suffix on collisions.
func (s *Server) mintProviderID(name string) string {
	var b strings.Builder
	lastDash := true // swallow leading dashes
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	id := strings.Trim(b.String(), "-")
	if id == "" {
		id = "api"
	}
	base := id
	for n := 2; ; n++ {
		taken := false
		for _, p := range s.Store.Config().Providers {
			if p.ID == id {
				taken = true
				break
			}
		}
		if !taken {
			return id
		}
		id = base + "-" + itoa(n)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// maskKey renders a provider key for display: a local secret the dashboard may
// see only in outline.
func maskKey(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 8 {
		return "••••"
	}
	return k[:4] + "••••" + k[len(k)-4:]
}

// providerViews is the admin-facing provider snapshot.
func (s *Server) providerViews() []map[string]any {
	out := []map[string]any{}
	for _, p := range s.Store.Config().Providers {
		models := []string{}
		models = append(models, p.Models...)
		out = append(out, map[string]any{
			"id": p.ID, "name": p.Name, "baseUrl": p.BaseURL,
			"apiKeyMasked": maskKey(p.APIKey), "hasKey": p.APIKey != "",
			"protocol": p.Protocol, "enabled": p.Enabled,
			"models": models, "note": p.Note,
		})
	}
	return out
}

// decodeProviderIn is the shared save/fetch request body.
type providerIn struct {
	ID       string             `json:"id"`
	Name     string             `json:"name"`
	BaseURL  string             `json:"baseUrl"`
	APIKey   string             `json:"apiKey"`
	Protocol string             `json:"protocol"`
	Enabled  *bool              `json:"enabled"`
	Models   []string           `json:"models"`
	Catalog  []lane.ListingMeta `json:"catalog"` // listing rows, numbers included
	Save     *bool              `json:"save"`    // models fetch: false = probe only, don't persist
}

func catalogIDs(catalog []lane.ListingMeta) []string {
	ids := make([]string, 0, len(catalog))
	for _, row := range catalog {
		ids = append(ids, row.ID)
	}
	return ids
}

// mergeCatalog records the token capacities a listing declared for the
// provider's selected models and drops entries for deselected ones. A model
// the listing did not number keeps whatever was known: a provider that stops
// publishing capacities must not erase them.
func mergeCatalog(p *store.Provider, catalog []lane.ListingMeta) {
	selected := make(map[string]bool, len(p.Models))
	for _, m := range p.Models {
		selected[m] = true
	}
	for id := range p.ModelMeta {
		if !selected[id] {
			delete(p.ModelMeta, id)
		}
	}
	for _, row := range catalog {
		if !selected[row.ID] {
			continue
		}
		meta := store.ModelMeta{
			Name: row.Name, ContextWindow: row.ContextWindow, MaxOutput: row.MaxOutput,
			Vision: row.Vision, Audio: row.Audio, File: row.File, Reasoning: row.Reasoning,
			InputDeclared: row.InputDeclared, OutputDeclared: row.OutputDeclared,
		}
		if !meta.Declared() {
			continue
		}
		if p.ModelMeta == nil {
			p.ModelMeta = map[string]store.ModelMeta{}
		}
		p.ModelMeta[row.ID] = meta
	}
	if len(p.ModelMeta) == 0 {
		p.ModelMeta = nil
	}
}

func (in *providerIn) normalize() error {
	in.Name = strings.TrimSpace(in.Name)
	in.BaseURL = relay.NormalizeBaseURL(in.BaseURL)
	in.Protocol = strings.TrimSpace(in.Protocol)
	if in.Protocol == "" {
		in.Protocol = store.ProtocolOpenAI
	}
	if in.Name == "" {
		return errAdmin("名称不能为空")
	}
	if in.BaseURL == "" {
		return errAdmin("Base URL 不能为空")
	}
	if in.Protocol != store.ProtocolOpenAI && in.Protocol != store.ProtocolAnthropic {
		return errAdmin("协议只支持 openai / anthropic")
	}
	return nil
}

type adminErr string

func (e adminErr) Error() string { return string(e) }

func errAdmin(msg string) error { return adminErr(msg) }

// cleanModels dedupes and trims a model id list.
func cleanModels(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

// adminProviderSave creates or updates one provider. APIKey is write-only: an
// empty value on update keeps the stored key so the form never round-trips it.
func (s *Server) adminProviderSave(w http.ResponseWriter, r *http.Request) {
	var in providerIn
	if err := decodeBody(r, &in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if err := in.normalize(); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	cfg := s.Store.Config()
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	if in.ID != "" {
		for k := range cfg.Providers {
			if cfg.Providers[k].ID != in.ID {
				continue
			}
			p := &cfg.Providers[k]
			p.Name, p.BaseURL, p.Protocol = in.Name, in.BaseURL, in.Protocol
			if in.APIKey != "" {
				p.APIKey = strings.TrimSpace(in.APIKey)
			}
			p.Enabled = enabled
			if in.Models != nil {
				p.Models = cleanModels(in.Models)
			}
			mergeCatalog(p, in.Catalog)
			if err := s.Store.Save(); err != nil {
				writeJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
				return
			}
			if s.tagger != nil && len(p.Models) > 0 {
				s.tagger.Enqueue(p.Models...)
			}
			s.logCat(logx.CatAdmin, "info", "自定义供应商已更新: %s (%s)", p.Name, p.ID)
			writeJSON(w, 200, map[string]any{"ok": true, "id": p.ID})
			return
		}
		writeJSON(w, 404, map[string]any{"ok": false, "error": "供应商不存在: " + in.ID})
		return
	}
	p := store.Provider{
		ID: s.mintProviderID(in.Name), Name: in.Name, BaseURL: in.BaseURL,
		APIKey: strings.TrimSpace(in.APIKey), Protocol: in.Protocol,
		Enabled: enabled, Models: cleanModels(in.Models),
	}
	mergeCatalog(&p, in.Catalog)
	cfg.Providers = append(cfg.Providers, p)
	if err := s.Store.Save(); err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if s.tagger != nil && len(p.Models) > 0 {
		s.tagger.Enqueue(p.Models...)
	}
	s.logCat(logx.CatAdmin, "info", "自定义供应商已添加: %s (%s, %d 模型)", p.Name, p.ID, len(p.Models))
	writeJSON(w, 200, map[string]any{"ok": true, "id": p.ID})
}

// adminProviderDelete removes one provider by id.
func (s *Server) adminProviderDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	cfg := s.Store.Config()
	for k := range cfg.Providers {
		if cfg.Providers[k].ID == in.ID {
			name := cfg.Providers[k].Name
			cfg.Providers = append(cfg.Providers[:k], cfg.Providers[k+1:]...)
			if err := s.Store.Save(); err != nil {
				writeJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
				return
			}
			s.logCat(logx.CatAdmin, "info", "自定义供应商已删除: %s (%s)", name, in.ID)
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
	}
	writeJSON(w, 404, map[string]any{"ok": false, "error": "供应商不存在: " + in.ID})
}

// adminProviderModels fetches a provider's /models listing live. With an id it
// uses the stored credentials and also persists the fresh listing (the
// dashboard's 刷新模型 button); without one it probes unsaved form fields and
// saves nothing (the 拉取模型 button before first save).
func (s *Server) adminProviderModels(w http.ResponseWriter, r *http.Request) {
	var in providerIn
	if err := decodeBody(r, &in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	ctx := r.Context()
	shouldSave := in.Save == nil || *in.Save
	if in.ID != "" {
		cfg := s.Store.Config()
		for k := range cfg.Providers {
			if cfg.Providers[k].ID != in.ID {
				continue
			}
			p := &cfg.Providers[k]
			catalog, err := relay.FetchModelCatalog(ctx, p.BaseURL, p.APIKey, p.Protocol)
			if err != nil {
				writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
				return
			}
			models := catalogIDs(catalog)
			if shouldSave {
				// 刷新只修剪选择，不重新展开目录：已勾选的模型若仍在上游就保留，
				// 上游已下架的移除，新模型不自动加入（要用「挑选模型」勾）。
				keep := map[string]bool{}
				for _, m := range p.Models {
					keep[m] = true
				}
				var merged []string
				for _, m := range cleanModels(models) {
					if keep[m] {
						merged = append(merged, m)
					}
				}
				p.Models = merged
				mergeCatalog(p, catalog)
				if err := s.Store.Save(); err != nil {
					writeJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
					return
				}
				if s.tagger != nil && len(p.Models) > 0 {
					s.tagger.Enqueue(p.Models...)
				}
				s.logCat(logx.CatAdmin, "info", "自定义供应商模型已刷新: %s (保留 %d 个勾选)", p.Name, len(p.Models))
			}
			writeJSON(w, 200, map[string]any{"ok": true, "models": models, "catalog": catalog,
				"saved":       shouldSave,
				"recommended": relay.RecommendedModels("", p.BaseURL, models)})
			return
		}
		writeJSON(w, 404, map[string]any{"ok": false, "error": "供应商不存在: " + in.ID})
		return
	}
	if err := in.normalize(); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	catalog, err := relay.FetchModelCatalog(ctx, in.BaseURL, strings.TrimSpace(in.APIKey), in.Protocol)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	models := catalogIDs(catalog)
	writeJSON(w, 200, map[string]any{"ok": true, "models": models, "catalog": catalog, "saved": false,
		"recommended": relay.RecommendedModels("", in.BaseURL, models)})
}

// logCat writes one classified line through the injected logger when present.
func (s *Server) logCat(cat, level, format string, args ...any) {
	if s.logger != nil {
		s.logger.Logf(cat, level, format, args...)
	}
}

// MigrateProviderSelections re-applies the recommended subset to providers
// whose model lists were saved by pre-selection builds — those always stored
// the full catalog dump, which would flood the 模型 page with unchecked
// models. One-time gate at schema v5; runs at boot from main. Returns the
// number of providers whose list shrank.
func MigrateProviderSelections(st *store.Store) int {
	cfg := st.Config()
	if cfg.SchemaVersion >= 5 {
		return 0
	}
	cfg.SchemaVersion = 5
	shrunk := 0
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if len(p.Models) == 0 {
			continue
		}
		before := len(p.Models)
		p.Models = relay.RecommendedModels("", p.BaseURL, p.Models)
		if len(p.Models) != before {
			shrunk++
		}
	}
	_ = st.Save()
	return shrunk
}
