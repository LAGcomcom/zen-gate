// Package autotag classifies model capabilities (vision / audio / file
// inputs, reasoning, context) by asking the free lane's own models — the user
// already has them, so a classification costs a few hundred tokens of free
// quota instead of a curated-table update. Results land in tags.json and feed
// both the dashboard's capability badges and the modality-aware router.
package autotag

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// taggerModel is the classification workhorse. Failover (and the Exhausted
// signal) is the lane's job — if this model is rate-limited the request rides
// the normal candidate chain.
const taggerModel = "mimo-v2.6-flash-free"

// batchLimit keeps one classification turn well inside a comfortable output
// budget; a provider rarely ships more chosen models than this.
const batchLimit = 30

// taggerBudget caps one classification turn. Only public model ids reach the
// tagger (private lane ids are the curated table's job — a reasoning model
// spirals on ids it cannot know and burns the budget thinking).
const taggerBudget = 8192

// failBackoff waits out the free lane after a failed round (the lane's own
// prober backs off the same way when it hits the quota wall).
const failBackoff = 30 * time.Minute

// maxConsecutiveFails stops the retry treadmill after this many failed rounds
// in a row; new Enqueue calls reset it.
const maxConsecutiveFails = 5

// Tagger runs the background classification worker.
type Tagger struct {
	lane  *lane.Lane
	store *store.Store
	logf  func(format string, args ...any)

	mu         sync.Mutex
	enabled    bool
	pending    []string
	queued     map[string]bool
	lastFail   time.Time
	failStreak int
	tagging    bool
	kick       chan struct{}
}

// New wires a tagger; logf may be nil. The tagger starts disabled — the
// caller applies the stored AutoTagEnabled setting via SetEnabled.
func New(l *lane.Lane, st *store.Store, logf func(format string, args ...any)) *Tagger {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Tagger{
		lane:   l,
		store:  st,
		logf:   logf,
		queued: map[string]bool{},
		kick:   make(chan struct{}, 1),
	}
}

// SetEnabled pauses/resumes the worker; queued ids survive the toggle.
func (t *Tagger) SetEnabled(on bool) {
	t.mu.Lock()
	t.enabled = on
	t.mu.Unlock()
	if on {
		select {
		case t.kick <- struct{}{}:
		default:
		}
	}
}

// Enabled reports whether the worker classifies.
func (t *Tagger) Enabled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.enabled
}

// Enqueue queues model ids for classification and wakes the worker. Ids the
// user has probed live (实测) are never re-classified — a probe verdict
// outranks anything the tagger can produce.
func (t *Tagger) Enqueue(ids ...string) {
	t.mu.Lock()
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || t.queued[id] {
			continue
		}
		if prev, ok := t.store.ModelTagOf(id); ok && prev.Source == store.TagSourceProbe {
			continue
		}
		t.queued[id] = true
		t.pending = append(t.pending, id)
	}
	t.mu.Unlock()
	select {
	case t.kick <- struct{}{}:
	default:
	}
}

// Start runs the worker until ctx is cancelled: classify whatever is pending
// whenever the lane is past its failure backoff.
func (t *Tagger) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.kick:
			case <-ticker.C:
			}
			t.mu.Lock()
			ready := t.lastFail.IsZero() || time.Since(t.lastFail) >= failBackoff
			streak := t.failStreak
			t.mu.Unlock()
			if !ready || streak >= maxConsecutiveFails {
				continue
			}
			t.drain(ctx)
		}
	}()
}

// drain classifies pending ids in batches until the queue is empty or a
// round fails.
func (t *Tagger) drain(ctx context.Context) {
	t.mu.Lock()
	if t.tagging || !t.enabled {
		t.mu.Unlock()
		return
	}
	t.tagging = true
	batch := make([]string, 0, batchLimit)
	for len(batch) < batchLimit && len(t.pending) > 0 {
		id := t.pending[0]
		t.pending = t.pending[1:]
		delete(t.queued, id)
		batch = append(batch, id)
	}
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.tagging = false
		t.mu.Unlock()
	}()
	if len(batch) == 0 {
		return
	}

	tags, raw, err := t.classify(ctx, batch)
	if err != nil {
		t.mu.Lock()
		t.lastFail = time.Now()
		t.failStreak++
		t.tagging = false
		t.mu.Unlock()
		// re-queue for the next backoff window (outside the lock — Enqueue
		// takes it itself)
		t.Enqueue(batch...)
		t.logf("AI 标注失败（%d 个模型，%v 后重试）: %v", len(batch), failBackoff, err)
		return
	}
	now := time.Now().UnixMilli()
	applied := 0
	for _, id := range batch {
		tag, ok := tags[strings.TrimSpace(id)]
		if !ok {
			continue // the model did not recognize this id — leave unknown
		}
		tag.Source = store.TagSourceAI
		tag.At = now
		t.store.SetModelTag(id, tag)
		t.applyToLane(id, tag)
		applied++
	}
	t.mu.Lock()
	t.failStreak = 0
	t.tagging = false
	t.mu.Unlock()
	if applied == 0 {
		// Nothing matched: surface the raw answer (trimmed) so the dashboard
		// log shows whether the model skipped ids or answered unparseably.
		snippet := raw
		if len(snippet) > 200 {
			snippet = snippet[:200] + "…"
		}
		t.logf("AI 标注完成: 0/%d 个模型已分类，原始返回: %s", len(batch), strings.TrimSpace(snippet))
		return
	}
	t.logf("AI 标注完成: %d/%d 个模型已分类", applied, len(batch))
}

// applyToLane upgrades a free-lane model's catalog entry with the tagger's
// verdict (audio/file are accepted as-is; vision is upgrade-only).
func (t *Tagger) applyToLane(id string, tag store.ModelTag) {
	audio, file, vision := tag.Audio, tag.File, tag.Vision
	if t.lane.ApplyCapabilityTags(id, lane.CapabilityTags{
		Audio:         &audio,
		File:          &file,
		Vision:        &vision,
		ContextWindow: tag.ContextWindow,
		MaxOutput:     tag.MaxOutput,
	}) {
		return
	}
	// The id may carry an effort suffix — retry on the bare id.
	if base := lane.BaseModelId(id); base != id {
		t.lane.ApplyCapabilityTags(base, lane.CapabilityTags{
			Audio:         &audio,
			File:          &file,
			Vision:        &vision,
			ContextWindow: tag.ContextWindow,
			MaxOutput:     tag.MaxOutput,
		})
	}
}

// classify asks the tagger model once for a batch of model ids; it returns
// the parsed tags plus the raw answer for observability.
func (t *Tagger) classify(ctx context.Context, ids []string) (map[string]store.ModelTag, string, error) {
	prompt := tagPrompt(ids)
	var out strings.Builder
	_, uerr := t.lane.Complete(ctx, lane.Request{
		Model:     taggerModel,
		Agent:     "autotag",
		MaxTokens: taggerBudget,
		Messages: []lane.Message{{Role: lane.RoleUser,
			Parts: []lane.Part{lane.TextPart{Text: prompt}}}},
	}, func(c lane.Chunk) {
		if c.Kind == lane.ChunkTextDelta {
			out.WriteString(c.Delta)
		}
	})
	if uerr != nil {
		return nil, out.String(), uerr
	}
	return parseTagJSON(out.String(), ids), out.String(), nil
}

func tagPrompt(ids []string) string {
	var b strings.Builder
	b.WriteString("You are a model specification database. For each model id below, output one JSON object describing its INPUT capabilities.\n\n")
	b.WriteString("Respond with ONLY a JSON array — no prose, no markdown fences, no explanation. One object per model, exactly this shape:\n")
	b.WriteString(`[{"id":"<id from the list>","vision":true,"audio":true,"file":true,"reasoning":true,"contextWindow":0,"maxOutput":0}]` + "\n\n")
	b.WriteString("Example input `qwen2.5-vl-72b` → output `[{\"id\":\"qwen2.5-vl-72b\",\"vision\":true,\"audio\":false,\"file\":false,\"reasoning\":false,\"contextWindow\":131072,\"maxOutput\":8192}]`.\n\n")
	b.WriteString("Field meanings:\n")
	b.WriteString("- vision: accepts image inputs (image_url / base64 image parts)\n")
	b.WriteString("- audio: accepts audio inputs (input_audio parts)\n")
	b.WriteString("- file: accepts document or file inputs (PDF / file_data parts)\n")
	b.WriteString("- reasoning: a reasoning/thinking model by design\n")
	b.WriteString("- contextWindow: max input tokens (0 if unknown); maxOutput: max output tokens (0 if unknown)\n")
	b.WriteString("- If you are not sure what a model is, SKIP it immediately without thinking about it. Never invent ids.\n\nModels:\n")
	for _, id := range ids {
		b.WriteString("- " + id + "\n")
	}
	return b.String()
}

// parseTagJSON defensively extracts the JSON array from a model answer: it
// brackets to the outermost array (tolerating markdown fences and stray
// prose), validates types, and keeps only ids that were actually requested.
func parseTagJSON(text string, wanted []string) map[string]store.ModelTag {
	out := map[string]store.ModelTag{}
	want := map[string]bool{}
	for _, id := range wanted {
		want[strings.TrimSpace(id)] = true
	}
	start := strings.Index(text, "[")
	end := strings.LastIndex(text, "]")
	if start < 0 || end <= start {
		return out
	}
	var rows []struct {
		ID            string `json:"id"`
		Vision        *bool  `json:"vision"`
		Audio         *bool  `json:"audio"`
		File          *bool  `json:"file"`
		Reasoning     *bool  `json:"reasoning"`
		ContextWindow int    `json:"contextWindow"`
		MaxOutput     int    `json:"maxOutput"`
	}
	if err := json.Unmarshal([]byte(text[start:end+1]), &rows); err != nil {
		return out
	}
	for _, r := range rows {
		id := strings.TrimSpace(r.ID)
		if id == "" || !want[id] {
			continue
		}
		tag := store.ModelTag{ContextWindow: r.ContextWindow, MaxOutput: r.MaxOutput}
		if r.Vision != nil {
			tag.Vision = *r.Vision
		}
		if r.Audio != nil {
			tag.Audio = *r.Audio
		}
		if r.File != nil {
			tag.File = *r.File
		}
		if r.Reasoning != nil {
			tag.Reasoning = *r.Reasoning
		}
		out[id] = tag
	}
	return out
}
