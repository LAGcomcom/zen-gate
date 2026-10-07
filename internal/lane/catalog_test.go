package lane

import (
	"encoding/json"
	"testing"
)

func decodeListing(t *testing.T, body string) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("listing body is not JSON: %v", err)
	}
	return payload
}

func TestParseListingMetaReadsDeclaredCapacities(t *testing.T) {
	// NVIDIA NIM declares input/output separately; OpenRouter declares one
	// total window plus a completion cap. Both must land in the same shape.
	nim := decodeListing(t, `{"data":[{"id":"nvidia/llama-3.1-nemotron-70b",
		"max_input_tokens":131072,"max_output_tokens":4096,
		"viewer_total_token_limit":128000}]}`)
	rows := ParseListingMeta(nim)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].ID != "nvidia/llama-3.1-nemotron-70b" {
		t.Errorf("ID = %q", rows[0].ID)
	}
	if rows[0].ContextWindow != 131072 {
		t.Errorf("ContextWindow = %d, want 131072 (max_input_tokens wins over the viewer total)", rows[0].ContextWindow)
	}
	if rows[0].MaxOutput != 4096 {
		t.Errorf("MaxOutput = %d, want 4096", rows[0].MaxOutput)
	}

	or := decodeListing(t, `{"data":[{"id":"deepseek/deepseek-chat",
		"context_length":163840,"max_completion_tokens":8192}]}`)
	rows = ParseListingMeta(or)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].ContextWindow != 163840 || rows[0].MaxOutput != 8192 {
		t.Errorf("got ctx=%d out=%d, want ctx=163840 out=8192", rows[0].ContextWindow, rows[0].MaxOutput)
	}
}

func TestParseListingMetaKeepsUnnumberedRowsInOrder(t *testing.T) {
	// The plain OpenAI shape (and Anthropic's /v1/models) ships ids only: the
	// row must still be returned, with zero capacities, so callers can tell
	// "the provider said nothing" apart from "the provider was not reached".
	bare := decodeListing(t, `{"data":[{"id":"gpt-4o-mini"},{"object":"model"},{"id":"claude-3-5-sonnet"}]}`)
	rows := ParseListingMeta(bare)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (rows without an id are skipped, rows without numbers are kept)", len(rows))
	}
	if rows[0].ID != "gpt-4o-mini" || rows[1].ID != "claude-3-5-sonnet" {
		t.Errorf("order/ids lost: %+v", rows)
	}
	for _, r := range rows {
		if r.ContextWindow != 0 || r.MaxOutput != 0 {
			t.Errorf("%s invented capacities ctx=%d out=%d", r.ID, r.ContextWindow, r.MaxOutput)
		}
	}

	if got := ParseListingMeta(decodeListing(t, `{"models":["a","b"]}`)); len(got) != 2 || got[1].ID != "b" {
		t.Errorf("bare string array: %+v", got)
	}
}

// internAIRow is a trimmed real row from discovery-api.intern-ai.org.cn's
// /v1/models (schema_version 2.4): capacities and modalities are nested
// per-modality objects, not flat fields.
const internAIRow = `{"schema_version":"2.4","id":"Agents-A1","name":"Agents-A1",
	"input_modalities":[
		{"type":"text","supported_inputs":{"max_context_length":{"value":262144,"unit":"token"}}},
		{"type":"image","supported_inputs":{"sources":{"type":"enum","values":["url","base64"]}}}],
	"output_modalities":[{"type":"text","max_length":{"value":262144,"unit":"token"},
		"supported_parameters":{"max_tokens":{"type":"integer","max":262144},"tools":{"type":"boolean"}}}],
	"object":"model"}`

func TestParseListingMetaReadsNestedModalitySchema(t *testing.T) {
	row := ParseListingMeta(decodeListing(t, `{"data":[`+internAIRow+`]}`))[0]
	if row.ContextWindow != 262144 {
		t.Errorf("ContextWindow = %d, want 262144 from input_modalities.max_context_length", row.ContextWindow)
	}
	if row.MaxOutput != 262144 {
		t.Errorf("MaxOutput = %d, want 262144 from output_modalities.max_length", row.MaxOutput)
	}
	if row.Name != "Agents-A1" {
		t.Errorf("Name = %q, want the provider's own display name", row.Name)
	}
	if !row.Vision || !row.InputDeclared {
		t.Errorf("an image input modality must read as vision: %+v", row)
	}
	if row.Reasoning || !row.OutputDeclared {
		t.Errorf("no reasoning parameter was declared, so reasoning must be an explicit no: %+v", row)
	}
}

func TestParseListingMetaReasoningSwitch(t *testing.T) {
	body := `{"data":[{"id":"glm-5.3","input_modalities":[{"type":"text",
		"supported_inputs":{"max_context_length":{"value":1048576}}}],
		"output_modalities":[{"type":"text","supported_parameters":{
			"reasoning":{"type":"boolean"},"max_tokens":{"max":1048576}}}]}]}`
	row := ParseListingMeta(decodeListing(t, body))[0]
	if !row.Reasoning {
		t.Errorf("declared reasoning switch dropped: %+v", row)
	}
	if row.InputDeclared && row.Vision {
		t.Errorf("text-only inputs must not claim vision: %+v", row)
	}
	if row.ContextWindow != 1048576 || row.MaxOutput != 1048576 {
		t.Errorf("capacities = ctx %d out %d, want 1048576/1048576 (max_tokens.max fills in when max_length is absent)", row.ContextWindow, row.MaxOutput)
	}
}

func TestParseListingMetaDistinguishesUndeclaredFromDenied(t *testing.T) {
	// A plain OpenAI row states no modalities: "unknown", so the router keeps it
	// a soft pass for image traffic. A row that does state them and omits image
	// is a real "no".
	plain := ParseListingMeta(decodeListing(t, `{"data":[{"id":"gpt-4o-mini"}]}`))[0]
	if plain.InputDeclared || plain.OutputDeclared || plain.Vision {
		t.Errorf("undeclared listing read as a verdict: %+v", plain)
	}
	textOnly := ParseListingMeta(decodeListing(t, `{"data":[{"id":"strict-text",
		"input_modalities":[{"type":"text"}]}]}`))[0]
	if !textOnly.InputDeclared || textOnly.Vision {
		t.Errorf("declared text-only input must be an explicit no-vision: %+v", textOnly)
	}
	audio := ParseListingMeta(decodeListing(t, `{"data":[{"id":"omni",
		"input_modalities":[{"type":"text"},{"type":"audio"},{"type":"file"}]}]}`))[0]
	if !audio.Audio || !audio.File {
		t.Errorf("audio/file input modalities dropped: %+v", audio)
	}
}
