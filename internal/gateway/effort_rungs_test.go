package gateway

import (
	"testing"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

func rungCfg() *store.Config {
	return &store.Config{DefaultMaxTokens: 32768, DefaultEffort: "balanced"}
}

func rungIDs(levels []lane.LevelBudget) []string {
	out := make([]string, 0, len(levels))
	for _, l := range levels {
		out = append(out, l.ID)
	}
	return out
}

// A provider that publishes rungs owns the picker. Advertising the gateway's
// own light/balanced/deep instead would put words on the client that the
// vendor's endpoint has never heard.
func TestEffortLadderUsesTheVendorRungs(t *testing.T) {
	caps := modalityCaps{Reasoning: true,
		Efforts: []string{"low", "medium", "xhigh"}, EffortDefault: "medium"}
	levels, def := effortLadder(lane.ModelInfo{ID: "Qwen3.8-Flash", Reasoning: true}, caps, rungCfg())
	got := rungIDs(levels)
	want := []string{"low", "medium", "xhigh"}
	if len(got) != len(want) {
		t.Fatalf("rungs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rung %d = %q, want %q", i, got[i], want[i])
		}
	}
	if def != "medium" {
		t.Errorf("default = %q, want the vendor's starred rung, not the gateway's configured one", def)
	}
	var starred []string
	for _, l := range levels {
		if l.IsDefault {
			starred = append(starred, l.ID)
		}
	}
	if len(starred) != 1 || starred[0] != "medium" {
		t.Errorf("isDefault marked %v, want only medium", starred)
	}
	// Each rung still carries a real output budget, and heavier means more.
	for i := 1; i < len(levels); i++ {
		if levels[i].Tokens < levels[i-1].Tokens {
			t.Errorf("rung %s (%d tokens) is not heavier than %s (%d)",
				levels[i].ID, levels[i].Tokens, levels[i-1].ID, levels[i-1].Tokens)
		}
	}
	if levels[0].ZH == "" {
		t.Error("a known vendor rung has no Chinese label for the picker")
	}
}

// Nothing declared: the gateway's own budget ladder is the honest answer.
func TestEffortLadderFallsBackWhenNothingIsDeclared(t *testing.T) {
	levels, def := effortLadder(lane.ModelInfo{ID: "glm-4.6", Reasoning: true}, modalityCaps{}, rungCfg())
	got := rungIDs(levels)
	want := []string{"light", "balanced", "deep"}
	if len(got) != len(want) {
		t.Fatalf("rungs = %v, want the gateway ladder %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rung %d = %q, want %q", i, got[i], want[i])
		}
	}
	if def != "balanced" {
		t.Errorf("default = %q, want the configured one", def)
	}
}

// The word the picker produced, in the naming this vendor answers to.
func TestForwardEffortSpeaksTheVendorsLanguage(t *testing.T) {
	caps := modalityCaps{Efforts: []string{"low", "medium", "xhigh"}, EffortDefault: "medium"}
	silent := modalityCaps{}
	cases := []struct {
		name string
		caps modalityCaps
		want string
		in   string
	}{
		{"vendor word verbatim", caps, "xhigh", "xhigh"},
		{"another vendor word verbatim", caps, "low", "low"},
		{"gateway deep maps to the heaviest rung", caps, "xhigh", "deep"},
		{"gateway balanced maps to the middle", caps, "medium", "balanced"},
		{"gateway light maps to the lightest", caps, "low", "light"},
		{"off stays off", caps, "none", "none"},
		{"off is not softened into the lightest rung", caps, "off", "off"},
		{"a source that states nothing keeps the caller's word", silent, "xhigh", "xhigh"},
		{"a source that states nothing gets no gateway invention", silent, "deep", "deep"},
		{"nothing declared sends nothing", caps, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := forwardEffort(c.caps, c.in); got != c.want {
				t.Fatalf("forwardEffort(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// A vendor ladder may state its rungs in any order (and a hand-edited config
// will): heaviest and lightest are decided by rank, not position.
func TestRungAtUsesRankNotPosition(t *testing.T) {
	rungs := []string{"xhigh", "low", "medium"}
	if got := rungAt(rungs, true); got != "low" {
		t.Errorf("lightest = %q, want low", got)
	}
	if got := rungAt(rungs, false); got != "xhigh" {
		t.Errorf("heaviest = %q, want xhigh", got)
	}
}
