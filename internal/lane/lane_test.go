package lane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// --- identity minting -------------------------------------------------------

func TestMintSessionIdShape(t *testing.T) {
	re := regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
	for i := 0; i < 50; i++ {
		id := MintSessionId(1790925320157)
		if !re.MatchString(id) {
			t.Fatalf("minted session id does not match gateway shape: %s", id)
		}
	}
}

func TestSessionForConversationStable(t *testing.T) {
	a := SessionForConversation("conv-123")
	b := SessionForConversation("conv-123")
	if a != b {
		t.Fatalf("same conversation must map to one session: %s != %s", a, b)
	}
	re := regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
	if !re.MatchString(a) {
		t.Fatalf("derived session id has wrong shape: %s", a)
	}
	if SessionForConversation("") == SessionForConversation("other") {
		t.Fatal("empty seed must not collide with a named seed")
	}
}

func TestRequestIdForStablePerTurn(t *testing.T) {
	s := SessionForConversation("c1")
	if RequestIdFor(s, "turn-1") != RequestIdFor(s, "turn-1") {
		t.Fatal("retries of one turn must share a request id")
	}
	if RequestIdFor(s, "turn-1") == RequestIdFor(s, "turn-2") {
		t.Fatal("different turns must differ")
	}
}

// --- fingerprint gate -------------------------------------------------------

func TestApplyFingerprintPromotesRealShell(t *testing.T) {
	body := map[string]any{"tools": []any{
		map[string]any{"type": "function", "function": map[string]any{
			"name": "pwsh", "description": "run shell", "parameters": map[string]any{"type": "object"},
		}},
	}}
	rename := ApplyFingerprint(body, "chat")
	tools := body["tools"].([]any)
	if len(tools) != 4 {
		t.Fatalf("quartet must be complete, got %d", len(tools))
	}
	if rename["bash"] != "pwsh" {
		t.Fatalf("bash slot must be donated by pwsh, rename=%v", rename)
	}
	// The promoted tool carries the bash name on the wire.
	first := tools[0].(map[string]any)["function"].(map[string]any)["name"]
	if first != "bash" {
		t.Fatalf("promoted slot must be sent as bash, got %v", first)
	}
	// Remaining slots get self-disabling decoys.
	for _, tt := range tools[1:] {
		m := tt.(map[string]any)["function"].(map[string]any)
		if m["description"] != "This tool is currently unavailable and must not be used." {
			t.Fatalf("missing slot must get a decoy, got %v", m)
		}
	}
}

func TestApplyFingerprintNoDuplicates(t *testing.T) {
	body := map[string]any{"tools": []any{
		map[string]any{"type": "function", "function": map[string]any{"name": "Bash", "parameters": map[string]any{}}},
		map[string]any{"type": "function", "function": map[string]any{"name": "bash", "parameters": map[string]any{}}},
	}}
	ApplyFingerprint(body, "chat")
	tools := body["tools"].([]any)
	count := 0
	for _, tt := range tools {
		m := tt.(map[string]any)
		if fn := m["function"].(map[string]any)["name"]; fn == "bash" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("case variants must canonicalise to one bash, got %d", count)
	}
}

func TestApplyFingerprintRealBashUntouched(t *testing.T) {
	body := map[string]any{"tools": []any{
		map[string]any{"type": "function", "function": map[string]any{"name": "bash", "parameters": map[string]any{}}},
	}}
	rename := ApplyFingerprint(body, "chat")
	if len(rename) != 0 {
		t.Fatalf("a real bash needs no rename, got %v", rename)
	}
}

// --- pairing repair -----------------------------------------------------------

func TestRepairToolPairingDropsDangling(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Parts: []Part{TextPart{"hi"}}},
		{Role: RoleAssistant, Parts: []Part{ToolCallPart{ID: "call_1", Name: "bash", Arguments: "{}"}}},
		// result for call_2 only — call_1 is dangling
		{Role: RoleTool, Parts: []Part{ToolResultPart{ToolCallID: "call_2", Text: "orphan"}}},
	}
	out := RepairToolPairing(msgs)
	if len(out) != 2 {
		t.Fatalf("assistant-only-dangling turn must be dropped, got %d messages", len(out))
	}
	for _, m := range out {
		for _, p := range m.Parts {
			if _, ok := p.(ToolCallPart); ok {
				t.Fatal("dangling tool call survived")
			}
			if _, ok := p.(ToolResultPart); ok {
				t.Fatal("orphan tool result survived")
			}
		}
	}
}

func TestRepairToolPairingKeepsPaired(t *testing.T) {
	msgs := []Message{
		{Role: RoleAssistant, Parts: []Part{TextPart{"thinking"}, ToolCallPart{ID: "call_1", Name: "bash", Arguments: "{\"c\":1}"}}},
		{Role: RoleTool, Parts: []Part{ToolResultPart{ToolCallID: "call_1", Text: "done"}}},
	}
	out := RepairToolPairing(msgs)
	if len(out) != 2 {
		t.Fatalf("paired history must survive, got %d", len(out))
	}
	if len(out[0].Parts) != 2 || len(out[1].Parts) != 1 {
		t.Fatal("paired parts must not be rewritten")
	}
}

// --- body-shape sniffing ------------------------------------------------------

func TestSniffBodyByShapeNotHeader(t *testing.T) {
	cases := map[string]string{
		"data: {\"a\":1}\n\n":          "sse",
		"event: message\ndata: {}\n\n": "sse",
		"{\"choices\": []}":            "json",
		"":                             "empty",
		"   \r\n":                      "empty",
		"<html>hello</html>":           "unknown",
	}
	for body, want := range cases {
		if got := sniffBody([]byte(body)); got != want {
			t.Fatalf("sniffBody(%q) = %s, want %s", body, got, want)
		}
	}
}

// --- fake gateway round trips -------------------------------------------------

func chatSSE(frames ...string) string {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: " + f + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func TestPostStreamedChatHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") != "Bearer public" {
			t.Errorf("missing pooled credential")
		}
		if r.Header.Get("x-opencode-session") == "" || r.Header.Get("user-agent") != ClientUA {
			t.Errorf("fingerprint headers missing")
		}
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, chatSSE(
			`{"choices":[{"delta":{"role":"assistant","content":"he"}}]}`,
			`{"choices":[{"delta":{"content":"llo"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
		))
	}))
	defer srv.Close()
	old := UpstreamBase
	UpstreamBase = srv.URL
	defer func() { UpstreamBase = old }()

	var chunks [][]byte
	usage, err := PostStreamed(context.Background(), "/zen/v1/chat/completions",
		map[string]any{"model": "m", "stream": true}, "ses_x", "msg_y", func(p []byte) error {
			chunks = append(chunks, p)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 3 {
		t.Fatalf("expected 3 frames, got %d", len(chunks))
	}
	if usage == nil || usage.Input != 10 || usage.Output != 2 {
		t.Fatalf("usage not captured: %+v", usage)
	}
}

func TestPostStreamedSSPUnderJSONHeader(t *testing.T) {
	// Issue #6: the gateway answers with SSE frames under application/json.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, chatSSE(`{"choices":[{"delta":{"content":"ok"}}]}`, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	old := UpstreamBase
	UpstreamBase = srv.URL
	defer func() { UpstreamBase = old }()

	var texts []string
	dec := NewDecoder("chat", nil, func(c Chunk) {
		if c.Kind == ChunkTextDelta {
			texts = append(texts, c.Delta)
		}
	})
	_, err := PostStreamed(context.Background(), "/zen/v1/chat/completions",
		map[string]any{"model": "m", "stream": true}, "s", "r", func(p []byte) error {
			dec.Decode(p)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	res := dec.Finish()
	if strings.Join(texts, "") != "ok" || res.Finish != FinishStop {
		t.Fatalf("SSE-under-JSON not decoded: %v finish=%s", texts, res.Finish)
	}
}

func TestPostStreamedCutStreamDetected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"deep thought\"}}]}\n\n")
		// no finish frame, no [DONE], connection just ends
	}))
	defer srv.Close()
	old := UpstreamBase
	UpstreamBase = srv.URL
	defer func() { UpstreamBase = old }()

	dec := NewDecoder("chat", nil, func(Chunk) {})
	_, err := PostStreamed(context.Background(), "/zen/v1/chat/completions",
		map[string]any{"model": "m", "stream": true}, "s", "r", func(p []byte) error {
			dec.Decode(p)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	res := dec.Finish()
	if res.SawFinish {
		t.Fatal("a stream closed without a finish frame must not be marked finished")
	}
	if !res.SawReasoning || res.ReasoningText != "deep thought" {
		t.Fatalf("checkpoint reasoning lost: %+v", res)
	}
}

func TestClassifyFailures(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{403, `{"error":{"type":"RegionError","message":"not available in your country"}}`, CodeRegion},
		{429, `{"error":{"type":"FreeUsageLimitError","message":"usage limit"}}`, CodeQuota},
		{503, "Service Unavailable", CodeServer},
		{400, `{"error":{"message":"Model is unavailable."}}`, CodeServer},
		{200, `{"error":{"type":"ModelError","message":"Model is unavailable."}}`, CodeServer},
	}
	for _, c := range cases {
		got := ClassifyFailure(c.status, c.body, 0)
		if got.Code != c.want {
			t.Fatalf("ClassifyFailure(%d,%q) = %s, want %s", c.status, c.body, got.Code, c.want)
		}
	}
	// A 503 generic reason phrase must NOT read as a model verdict.
	e := ClassifyFailure(503, "Service Unavailable", 0)
	if e.Unavailable {
		t.Fatal("generic 5xx must not mark the model unavailable")
	}
	e2 := ClassifyFailure(400, `{"error":{"message":"Model is unavailable."}}`, 0)
	if !e2.Unavailable {
		t.Fatal("a refusal naming the model must mark it unavailable")
	}
}

// --- wire decoders ---------------------------------------------------------

func TestDecoderChatToolCallsByIndex(t *testing.T) {
	dec := NewDecoder("chat", nil, func(Chunk) {})
	dec.Decode([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"bash","arguments":"{\"x\":"}}]}}]}`))
	dec.Decode([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`))
	dec.Decode([]byte(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`))
	res := dec.Finish()
	if res.Finish != FinishToolCalls || res.BrokenToolCall {
		t.Fatalf("valid args must not break: %+v", res)
	}
}

func TestDecoderChatBrokenToolArgs(t *testing.T) {
	var finish string
	dec := NewDecoder("chat", nil, func(Chunk) {})
	dec.Decode([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"bash","arguments":"{\"x\":"}}]}}]}`))
	dec.Decode([]byte(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`))
	res := dec.Finish()
	_ = finish
	if !res.BrokenToolCall {
		t.Fatal("arguments that never close JSON must mark the call broken")
	}
	if res.Finish != FinishMaxTokens {
		t.Fatalf("broken tool call must downgrade to max-tokens, got %s", res.Finish)
	}
}

func TestDecoderMessagesWire(t *testing.T) {
	var texts, reasons []string
	dec := NewDecoder("messages", nil, func(c Chunk) {
		switch c.Kind {
		case ChunkTextDelta:
			texts = append(texts, c.Delta)
		case ChunkReasonDelta:
			reasons = append(reasons, c.Delta)
		}
	})
	dec.Decode([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":5}}}`))
	dec.Decode([]byte(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`))
	dec.Decode([]byte(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`))
	dec.Decode([]byte(`{"type":"content_block_stop","index":0}`))
	dec.Decode([]byte(`{"type":"content_block_start","index":1,"content_block":{"type":"text"}}`))
	dec.Decode([]byte(`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"hi"}}`))
	dec.Decode([]byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`))
	dec.Decode([]byte(`{"type":"message_stop"}`))
	res := dec.Finish()
	if strings.Join(reasons, "") != "hmm" || strings.Join(texts, "") != "hi" {
		t.Fatalf("messages wire decode broken: %v %v", texts, reasons)
	}
	if res.Finish != FinishStop {
		t.Fatalf("finish %s", res.Finish)
	}
	// Usage accounting lives in the SSE pump (mapUsage), not the decoder.
	var u *Usage
	for _, f := range []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":5}}}`,
		`{"type":"message_delta","usage":{"output_tokens":3}}`,
	} {
		var m map[string]any
		_ = json.Unmarshal([]byte(f), &m)
		if mu := mapUsage(m); mu != nil {
			if u == nil {
				u = mu
			} else {
				u.Merge(mu)
			}
		}
	}
	if u == nil || u.Input != 5 || u.Output != 3 {
		t.Fatalf("usage merge wrong: %+v", u)
	}
}

func TestDecoderResponsesWire(t *testing.T) {
	var texts []string
	var toolName string
	dec := NewDecoder("responses", nil, func(c Chunk) {
		switch c.Kind {
		case ChunkTextDelta:
			texts = append(texts, c.Delta)
		case ChunkBlockStart:
			if c.BlockType == "tool-call" {
				toolName = c.Name
			}
		}
	})
	dec.Decode([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"type":"message"}}`))
	dec.Decode([]byte(`{"type":"response.output_text.delta","output_index":0,"delta":"answer"}`))
	dec.Decode([]byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message"}}`))
	dec.Decode([]byte(`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"c1","name":"read"}}`))
	dec.Decode([]byte(`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{}"}`))
	dec.Decode([]byte(`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call"}}`))
	dec.Decode([]byte(`{"type":"response.completed"}`))
	res := dec.Finish()
	if strings.Join(texts, "") != "answer" || toolName != "read" {
		t.Fatalf("responses wire decode broken: %v %s", texts, toolName)
	}
	if res.Finish != FinishStop || res.BrokenToolCall {
		t.Fatalf("terminal state wrong: %+v", res)
	}
}

// --- effort budgets ----------------------------------------------------------

func TestBudgetForAlwaysThinkingDoubled(t *testing.T) {
	m := ModelInfo{MaxOutput: 131072, Reasoning: true, CanDisableThinking: false}
	if got := BudgetFor("light", m, 0, 0); got != 4096 {
		t.Fatalf("light on always-thinking model must be 4096, got %d", got)
	}
	if got := BudgetFor("balanced", m, 0, 0); got != 16384 {
		t.Fatalf("balanced must be 16384, got %d", got)
	}
	m2 := ModelInfo{MaxOutput: 131072, Reasoning: true, CanDisableThinking: true}
	if got := BudgetFor("light", m2, 0, 0); got != 2048 {
		t.Fatalf("light on normal model must be 2048, got %d", got)
	}
	// A zero/negative requested ceiling is *no* ceiling, not zero.
	if got := BudgetFor("deep", m2, 0, 0); got != 131072 {
		t.Fatalf("deep must inherit capacity, got %d", got)
	}
}

// --- catalog -----------------------------------------------------------------

func TestBuildCatalogFiltersAndDedupes(t *testing.T) {
	ids := []string{
		"mimo-v2.6-flash-free", "mimo-v2.6-flash-free", "gpt-5.5",
		"muse-spark-1.3-contributor-free", "union-alpha",
	}
	cat := BuildCatalog(ids)
	if len(cat) != 3 {
		t.Fatalf("want 3 free models, got %d: %+v", len(cat), cat)
	}
	if cat[0].ID != "mimo-v2.6-flash-free" || cat[0].CanDisableThinking {
		t.Fatalf("mimo v2.6 capability wrong: %+v", cat[0])
	}
	if cat[1].Wire != "responses" {
		t.Fatalf("muse-spark must ride the responses wire, got %s", cat[1].Wire)
	}
	if cat[2].Wire != "messages" {
		t.Fatalf("union-alpha must ride the messages wire, got %s", cat[2].Wire)
	}
}

func TestParseListingShapes(t *testing.T) {
	a := ParseListing(map[string]any{"data": []any{map[string]any{"id": "x-free"}, "y-free"}})
	if len(a) != 2 || a[0] != "x-free" || a[1] != "y-free" {
		t.Fatalf("data shape parse wrong: %v", a)
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(`{"models":[{"id":"z-free"}]}`), &m)
	if b := ParseListing(m); len(b) != 1 || b[0] != "z-free" {
		t.Fatalf("models shape parse wrong: %v", b)
	}
}

// --- messages projection ------------------------------------------------------

func TestToClaudeMessagesToolResultLeads(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Parts: []Part{TextPart{"q"}}},
		{Role: RoleAssistant, Parts: []Part{ToolCallPart{ID: "c1", Name: "bash", Arguments: "{}"}}},
		{Role: RoleTool, Parts: []Part{ToolResultPart{ToolCallID: "c1", Text: "22°C"}}},
		{Role: RoleUser, Parts: []Part{TextPart{"thanks"}}},
	}
	system, out := ToClaudeMessages(msgs)
	if system != "" {
		t.Fatalf("no system expected, got %q", system)
	}
	// user q | assistant tool_use | user [tool_result, text]
	if len(out) != 3 {
		t.Fatalf("want 3 turns, got %d: %+v", len(out), out)
	}
	third := out[2]["content"].([]map[string]any)
	if third[0]["type"] != "tool_result" {
		t.Fatalf("tool_result must lead the user turn, got %v", third[0]["type"])
	}
}

// --- reasoning spellings -----------------------------------------------------

func TestDecoderChatReasoningSpellings(t *testing.T) {
	var reasons []string
	for _, spelling := range []string{"reasoning", "reasoning_content", "reasoning_text", "thinking"} {
		reasons = nil
		dec := NewDecoder("chat", nil, func(c Chunk) {
			if c.Kind == ChunkReasonDelta {
				reasons = append(reasons, c.Delta)
			}
		})
		dec.Decode([]byte(`{"choices":[{"delta":{` + `"` + spelling + `":"deep thought"}}]}`))
		if strings.Join(reasons, "") != "deep thought" {
			t.Fatalf("spelling %q not read as reasoning", spelling)
		}
	}
	// reasoning_details array form, and only the first non-empty spelling
	// counting when a frame carries two names for the same text.
	reasons = nil
	dec := NewDecoder("chat", nil, func(c Chunk) {
		if c.Kind == ChunkReasonDelta {
			reasons = append(reasons, c.Delta)
		}
	})
	dec.Decode([]byte(`{"choices":[{"delta":{"reasoning_details":[{"text":"part "},{"text":"two"}]}}]}`))
	if strings.Join(reasons, "") != "part two" {
		t.Fatalf("reasoning_details not read: %v", reasons)
	}
	reasons = nil
	dec.Decode([]byte(`{"choices":[{"delta":{"reasoning_content":"a","reasoning":"b"}}]}`))
	// "reasoning" heads the priority order, so the duplicated frame reads once.
	if strings.Join(reasons, "") != "b" {
		t.Fatalf("duplicated spelling must not double the block: %v", reasons)
	}
}

// --- nameless tool calls -----------------------------------------------------

func TestDecoderChatNamelessToolCallBroken(t *testing.T) {
	dec := NewDecoder("chat", nil, func(Chunk) {})
	dec.Decode([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"arguments":"{}"}}]}}]}`))
	dec.Decode([]byte(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`))
	res := dec.Finish()
	if !res.BrokenToolCall || res.Finish != FinishMaxTokens {
		t.Fatalf("a call that never learned its name must break the turn: %+v", res)
	}
}

func TestRepairToolPairingDropsNamelessAnsweredCall(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Parts: []Part{TextPart{"q"}}},
		{Role: RoleAssistant, Parts: []Part{ToolCallPart{ID: "c1", Name: "", Arguments: "{}"}}},
		{Role: RoleTool, Parts: []Part{ToolResultPart{ToolCallID: "c1", Text: "Error: unknown tool \"\""}}},
	}
	out := RepairToolPairing(msgs)
	if len(out) != 2 {
		t.Fatalf("nameless call and its result must both go: %+v", out)
	}
	for _, m := range out {
		for _, p := range m.Parts {
			switch p.(type) {
			case ToolCallPart, ToolResultPart:
				t.Fatalf("call/result husk survived the repair: %+v", out)
			}
		}
	}
	// A named, answered call is untouched.
	paired := []Message{
		{Role: RoleAssistant, Parts: []Part{ToolCallPart{ID: "c2", Name: "bash", Arguments: "{}"}}},
		{Role: RoleTool, Parts: []Part{ToolResultPart{ToolCallID: "c2", Text: "ok"}}},
	}
	if got := RepairToolPairing(paired); len(got) != 2 {
		t.Fatalf("paired history must be untouched: %+v", got)
	}
}

// --- recovery shapes ---------------------------------------------------------

func TestRecoveryShapes(t *testing.T) {
	pureReasoning := StreamResult{SawReasoning: true, ReasoningText: "thinking"}
	if !canRecover(pureReasoning, 1000) {
		t.Fatal("cut pure-reasoning turn must recover")
	}
	silentStop := StreamResult{SawFinish: true, Finish: FinishStop, FinishToken: "end_turn", SawReasoning: true, ReasoningText: "thinking"}
	if !canRecoverSilentStop(silentStop, 1000) {
		t.Fatal("clean stop carrying only thinking must recover")
	}
	if canRecoverSilentStop(StreamResult{SawFinish: true, Finish: FinishStop, FinishToken: "failed", SawReasoning: true, ReasoningText: "t"}, 1000) {
		t.Fatal("a failed ending must not read as a silent stop")
	}
	cutAnswer := StreamResult{SawFinish: true, Finish: FinishMaxTokens, SawText: true, AnswerText: "half an answer"}
	if !canContinueFromCut(cutAnswer, 1000) {
		t.Fatal("an answer cut at the output ceiling must continue")
	}
	if canContinueFromCut(cutAnswer, RecoveryTotalTimeoutMS+1) {
		t.Fatal("the wall clock bounds continuations too")
	}
	if canContinueFromCut(StreamResult{SawFinish: true, Finish: FinishMaxTokens, SawToolCall: true}, 1000) {
		t.Fatal("a tool turn must never take the answer-continuation path")
	}
}

// --- failure classification --------------------------------------------------

func TestClassifyHTMLPageAndClientCodes(t *testing.T) {
	page := "<!doctype html><html><body>Request blocked</body></html>"
	if got := ClassifyFailure(403, page, 0); got.Code != CodeClient {
		t.Fatalf("a WAF page at 403 is the front proxy, not the credential store: %s", got.Code)
	}
	if got := ClassifyFailure(413, "payload too large", 0); got.Code != CodeClient {
		t.Fatalf("413 is the request's own fault: %s", got.Code)
	}
	if got := ClassifyFailure(408, "timeout", 0); got.Code != CodeServer {
		t.Fatalf("408 names the gateway's own timing trouble: %s", got.Code)
	}
	e := ClassifyFailure(403, page, 0)
	if strings.Contains(e.Message, "<html") || !strings.Contains(e.Message, "front proxy") {
		t.Fatalf("the message must name the hop, not quote the markup: %q", e.Message)
	}
}

func TestClassifyKiloFailures(t *testing.T) {
	if got := ClassifyKiloFailure(401, `{"error":{"code":"PAID_MODEL_AUTH_REQUIRED","message":"needs an account"}}`, 0); got.Code != CodeClient {
		t.Fatalf("a paid id requested keyless is a request defect: %+v", got)
	}
	got := ClassifyKiloFailure(429, `{"error_type":"rate_limited"}`, 0)
	if got.Code != CodeQuota {
		t.Fatalf("error_type envelope must reshape onto the shared vocabulary: %+v", got)
	}
}

// --- kilo catalog ------------------------------------------------------------

func TestBuildKiloCatalog(t *testing.T) {
	rows := []map[string]any{
		{"id": "nvidia/nemotron-3-ultra:free", "name": "NVIDIA: Nemotron 3 Ultra (free)", "isFree": true,
			"context_length": 128000.0,
			"top_provider":   map[string]any{"max_completion_tokens": 32768.0},
			"architecture":   map[string]any{"input_modalities": []any{"text", "image"}},
			"supported_parameters": []any{"reasoning"}},
		{"id": "stepfun/step-3:free", "name": "StepFun: Step 3 (free)", "isFree": true,
			"supported_parameters": []any{"reasoning"}},
		{"id": "vendor/paid-model", "isFree": false},
		{"id": "nvidia/nemotron-3-ultra:free", "isFree": true},
	}
	cat := BuildKiloCatalog(rows)
	if len(cat) != 2 {
		t.Fatalf("free filter + dedupe wrong: %+v", cat)
	}
	m := cat[0]
	if m.Channel != ChannelKilo || m.Wire != "chat" || !m.Vision || !m.Reasoning {
		t.Fatalf("kilo entry fields wrong: %+v", m)
	}
	if m.ContextWindow != 128000 || m.MaxOutput != 32768 {
		t.Fatalf("capacities wrong: %+v", m)
	}
	if m.Name != "Nemotron 3 Ultra" {
		t.Fatalf("display name wrong: %q", m.Name)
	}
	if !m.CanDisableThinking {
		t.Fatal("nemotron tolerates the off rung")
	}
	if cat[1].CanDisableThinking {
		t.Fatal("stepfun refuses the off rung; its menu must not promise one")
	}
	if kiloEffortRung("light") != "low" || kiloEffortRung("balanced") != "medium" || kiloEffortRung("deep") != "high" {
		t.Fatal("effort rung mapping wrong")
	}
}
