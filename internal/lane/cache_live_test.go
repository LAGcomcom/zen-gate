//go:build live

// Live A/B for the prompt-cache hints: does the upstream accept the field, and
// does the hit count actually move?
//
// The second half is the one that matters. A gateway that answers 200 while
// reporting `prompt_cache_hit_tokens: 0` has accepted a field and cached
// nothing, and shipping it as "we now have caching" would be a lie. This test
// therefore sends the SAME long prefix twice on the SAME conversation id, once
// without the hint and once with it, and prints both usage blocks side by side.
//
// Run it against a COPY of the live config, never the running gateway's own
// file, so nothing can race it:
//
//	ZEN_GATE_HOME=D:\tmp\cache-ab ZEN_WB_PROBE=1 \
//	  go test ./internal/lane/ -tags live -run TestLivePromptCache -v
//
// Nothing here writes credentials, and it prints counts only — no token, no
// uid, no message text beyond a length.
package lane

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// liveCachePrefix is a long, deterministic prefix: long enough that a cache hit
// is unmistakable (an 8k-token prefix was the reference measurement for this
// backend), and boring enough that no content gate would object to it.
func liveCachePrefix() string {
	var b strings.Builder
	b.WriteString("You are a careful assistant. Follow these rules exactly.\n")
	for i := 0; i < 240; i++ {
		fmt.Fprintf(&b, "Rule %d: keep replies short, cite the file you read, and never invent a path.\n", i)
	}
	return b.String()
}

func liveCacheMessages(prefix, question string) []Message {
	return []Message{
		{Role: RoleSystem, Parts: []Part{TextPart{Text: prefix}}},
		{Role: RoleUser, Parts: []Part{TextPart{Text: question}}},
	}
}

// TestLivePromptCache sends one prefix twice per arm and reports the two usage
// blocks. The field under test is the only difference between the arms.
func TestLivePromptCache(t *testing.T) {
	if os.Getenv("ZEN_WB_PROBE") == "" {
		t.Skip("live A/B; set ZEN_WB_PROBE=1")
	}
	home := os.Getenv("ZEN_GATE_HOME")
	if home == "" {
		t.Fatal("ZEN_GATE_HOME must point at a copy of the live config")
	}
	raw, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	// internal/store imports lane, so the pool is read straight off the JSON.
	var cfg struct {
		WorkBuddyAccounts []WBAccount `json:"workBuddyAccounts"`
		CodeArts          CAAccount   `json:"codeArts"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	prefix := liveCachePrefix()
	t.Logf("prefix: %d bytes, ~%d tokens by the 4-bytes-per-token rule of thumb",
		len(prefix), len(prefix)/4)

	if len(cfg.WorkBuddyAccounts) > 0 {
		testLiveWorkBuddyCache(t, cfg.WorkBuddyAccounts, prefix)
	} else {
		t.Log("no WorkBuddy account in the copied config")
	}
	if cfg.CodeArts.SecurityToken != "" {
		testLiveCodeArtsCache(t, cfg.CodeArts, prefix)
	} else {
		t.Log("no CodeArts credential in the copied config")
	}
}

// testLiveWorkBuddyCache is the arm that decides the default of
// Config.WorkBuddyCacheKey. Its verdict is printed, not asserted: a machine
// with no account must skip, and a human reads the table.
//
// The pool has to be handed to the lane explicitly — NewLane() starts empty, and
// a lane with no account answers wbNoAccountError before ever reaching the
// upstream, which would make every arm look like "the field was refused".
func testLiveWorkBuddyCache(t *testing.T, accounts []WBAccount, prefix string) {
	enabled := 0
	for _, a := range accounts {
		if a.Enabled && a.AccessToken != "" {
			enabled++
		}
	}
	t.Logf("WorkBuddy pool: %d account(s), %d enabled with a token", len(accounts), enabled)
	if enabled == 0 {
		t.Skip("no enabled WorkBuddy account with a token in the copied config")
	}

	// The model the reference measurement used. Read it from the copied channel
	// roster when it is there, so a renamed id does not silently turn the probe
	// into a 404.
	model := liveWorkBuddyModel(t)
	t.Logf("WorkBuddy model under test: %q", model)
	conv := fmt.Sprintf("cache-ab-%d", time.Now().UnixNano())

	// One lane per arm, each carrying the real pool. A lane with no account
	// answers before reaching the upstream, so an empty one would make both arms
	// look identical and prove nothing.
	l := NewLane()
	l.SetWorkBuddyAccounts(accounts)
	l.SetWorkBuddyPersist(func(WBAccount) {})
	for _, hint := range []struct {
		name string
		on   bool
	}{{"without prompt_cache_key", false}, {"with prompt_cache_key", true}} {
		l.SetWorkBuddyCacheKey(hint.on)
		t.Logf("=== WorkBuddy %s (model %s)", hint.name, model)
		// Two turns on one conversation: the first warms whatever cache exists,
		// the second is the one that can hit it.
		for turn := 1; turn <= 2; turn++ {
			msgs := liveCacheMessages(prefix, fmt.Sprintf("Turn %d: reply with the word ok.", turn))
			out, uerr := l.Complete(context.Background(), Request{
				Model: model, Messages: msgs, SessionSeed: conv, TurnSeed: fmt.Sprintf("%s-turn%d", conv, turn),
				MaxTokens: 32, Agent: "cache-ab",
			}, func(Chunk) {})
			if uerr != nil {
				t.Logf("  turn %d: ERROR %s: %s", turn, uerr.Code, uerr.Message)
				break
			}
			u := out.Usage
			t.Logf("  turn %d: input=%d fresh=%d cacheRead=%d cacheWrite=%d out=%d total=%d",
				turn, u.InputTotal(), u.InputFresh(), u.CacheRead, u.CacheWrite, u.Output, u.TotalTokens)
		}
	}
	t.Log("verdict: if the second arm reports cacheRead > 0 while the first stays 0, " +
		"flip workBuddyCacheKey to true by default (store.Config.WorkBuddyCacheKey).")
}

// liveWorkBuddyModel reads the WorkBuddy roster the gateway last wrote, so the
// probe asks for a model id the upstream actually publishes. A hardcoded id is
// how a live probe silently becomes a 404: "hy4-preview" was renamed to
// "hy4-preview-f" upstream and nothing in the test noticed.
func liveWorkBuddyModel(t *testing.T) string {
	t.Helper()
	const fallback = "hy4-preview"
	home := os.Getenv("ZEN_GATE_HOME")
	raw, err := os.ReadFile(filepath.Join(home, "channels", "workbuddy.json"))
	if err != nil {
		t.Logf("no workbuddy roster (%v); falling back to %q", err, fallback)
		return fallback
	}
	var roster struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &roster); err != nil || len(roster.Models) == 0 {
		t.Logf("workbuddy roster unreadable or empty; falling back to %q", fallback)
		return fallback
	}
	// Prefer the hy4 family the reference measurement used; a roster that has
	// moved on still yields a servable id rather than a 404.
	for _, m := range roster.Models {
		if strings.Contains(strings.ToLower(m.ID), "hy4") {
			return m.ID
		}
	}
	t.Logf("no hy4 model in the roster; using its first entry %q", roster.Models[0].ID)
	return roster.Models[0].ID
}

// testLiveCodeArtsCache does the same for the CodeArts lane, whose reference
// body builder has always sent prompt_cache_key — so the expectation here is
// that it already works and the hint simply stops being a no-op.
//
// The credential has to be installed on the lane: NewLane() has none, and an
// empty CodeArts account is answered locally instead of reaching the upstream.
func testLiveCodeArtsCache(t *testing.T, acct CAAccount, prefix string) {
	conv := fmt.Sprintf("cache-ab-ca-%d", time.Now().UnixNano())
	l := NewLane()
	l.SetCodeArtsAccount(acct)
	l.SetCodeArtsPersist(func(CAAccount) {})
	for turn := 1; turn <= 3; turn++ {
		msgs := liveCacheMessages(prefix, fmt.Sprintf("Turn %d: reply with the word ok.", turn))
		out, uerr := l.Complete(context.Background(), Request{
			Model: "GLM-5.2", Messages: msgs, SessionSeed: conv, TurnSeed: fmt.Sprintf("%s-turn%d", conv, turn),
			MaxTokens: 32, Agent: "cache-ab",
		}, func(Chunk) {})
		if uerr != nil {
			t.Logf("  turn %d: ERROR %s: %s", turn, uerr.Code, uerr.Message)
			return
		}
		u := out.Usage
		t.Logf("  turn %d: input=%d fresh=%d cacheRead=%d cacheWrite=%d out=%d total=%d",
			turn, u.InputTotal(), u.InputFresh(), u.CacheRead, u.CacheWrite, u.Output, u.TotalTokens)
	}
}

// TestLiveCacheFieldAccepted is the cheap half: send ONE tiny turn per mode and
// report whether the request was accepted at all. Run this first when a hint is
// suspected of breaking a lane — it answers "rejected?" without spending a
// full prefix's worth of tokens.
func TestLiveCacheFieldAccepted(t *testing.T) {
	if os.Getenv("ZEN_WB_PROBE") == "" {
		t.Skip("live probe; set ZEN_WB_PROBE=1")
	}
	home := os.Getenv("ZEN_GATE_HOME")
	raw, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		WorkBuddyAccounts []WBAccount `json:"workBuddyAccounts"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.WorkBuddyAccounts) == 0 {
		t.Skip("no WorkBuddy account")
	}
	l := NewLane()
	for _, on := range []bool{false, true} {
		l.SetWorkBuddyCacheKey(on)
		_, uerr := l.Complete(context.Background(), Request{
			Model:     "hy4-preview",
			Messages:  []Message{{Role: RoleUser, Parts: []Part{TextPart{Text: "ping"}}}},
			MaxTokens: 16, Agent: "cache-ab",
		}, func(Chunk) {})
		verdict := "accepted"
		if uerr != nil {
			verdict = "REFUSED " + uerr.Code + ": " + uerr.Message
		}
		t.Logf("prompt_cache_key=%-5v → %s", on, verdict)
	}
}
