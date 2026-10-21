package gateway

import "testing"

// QoderCN states its thinking rungs per model in thinking_config and its
// context capacity per tier in context_config. The card's picker must be built
// from those, because a ladder the gateway invented is a claim the vendor
// never made — and the names the vendor answers to are low/medium/xhigh, not
// the gateway's own light/balanced/deep.
const qoderListingQwen = `{
	"data": [{
		"id": "Qwen3.8-Flash",
		"is_reasoning": true,
		"is_vl": true,
		"max_input_tokens": 180000,
		"thinking_config": {
			"disabled": {},
			"enabled": {
				"is_default": true,
				"efforts": {"low": {}, "medium": {"is_default": true}, "xhigh": {}}
			}
		},
		"context_config": {
			"200K": {"is_default": true, "token_count": 200000},
			"400K": {"token_count": 400000},
			"1M":   {"token_count": 1000000}
		}
	}]
}`

func TestQoderCatalogReadsTheThinkingRungs(t *testing.T) {
	rows := parseQoderCatalog([]byte(qoderListingQwen))
	if len(rows) != 1 {
		t.Fatalf("catalog rows = %d, want 1", len(rows))
	}
	m := rows[0]
	want := []string{"low", "medium", "xhigh"}
	if len(m.Efforts) != len(want) {
		t.Fatalf("efforts = %v, want %v", m.Efforts, want)
	}
	for i := range want {
		if m.Efforts[i] != want[i] {
			t.Errorf("efforts[%d] = %q, want %q (rungs must come back lightest first, "+
				"not in whatever order the JSON object happened to iterate)", i, m.Efforts[i], want[i])
		}
	}
	if m.EffortDefault != "medium" {
		t.Errorf("effortDefault = %q, want the rung the listing starred", m.EffortDefault)
	}
	// The window is the widest tier the vendor sells, not the row's working
	// default: max_input_tokens 180000 is what the IDE sets at 200K, while the
	// same model publishes a 1M tier.
	if m.ContextWindow != 1000000 {
		t.Errorf("contextWindow = %d, want the widest declared tier (1000000)", m.ContextWindow)
	}
	if !m.Reasoning || !m.Vision {
		t.Errorf("flags = reasoning:%v vision:%v, want both from the rows", m.Reasoning, m.Vision)
	}
}

// A model without a thinking_config has no declared rungs: the gateway must
// report none rather than dressing its own ladder up as the vendor's.
func TestQoderCatalogWithoutThinkingConfigDeclaresNoRungs(t *testing.T) {
	body := `{"data":[{"id":"DeepSeek-Flash","is_reasoning":false,"is_vl":true,
		"max_input_tokens":180000,"context_config":{"200K":{"token_count":200000}}}]}`
	rows := parseQoderCatalog([]byte(body))
	if len(rows) != 1 {
		t.Fatalf("catalog rows = %d, want 1", len(rows))
	}
	if len(rows[0].Efforts) != 0 {
		t.Errorf("efforts = %v, want none for a model that publishes no thinking_config", rows[0].Efforts)
	}
	if rows[0].EffortDefault != "" {
		t.Errorf("effortDefault = %q, want empty", rows[0].EffortDefault)
	}
	if rows[0].ContextWindow != 200000 {
		t.Errorf("contextWindow = %d, want the one declared tier", rows[0].ContextWindow)
	}
}

// thinking_config can carry an "enabled" block with no efforts map at all: the
// model thinks, but states no levels. That is still "no rungs", not a crash.
func TestQoderCatalogThinkingWithoutEffortsMap(t *testing.T) {
	body := `{"data":[{"id":"Kimi-K3","is_reasoning":true,
		"thinking_config":{"disabled":{},"enabled":{"is_default":true}}}]}`
	rows := parseQoderCatalog([]byte(body))
	if len(rows) != 1 {
		t.Fatalf("catalog rows = %d, want 1", len(rows))
	}
	if len(rows[0].Efforts) != 0 {
		t.Errorf("efforts = %v, want none", rows[0].Efforts)
	}
	if !rows[0].Reasoning {
		t.Error("a thinking_config means the model reasons; Reasoning = false")
	}
}
