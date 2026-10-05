package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// --- custom-upstream passthrough -------------------------------------------
//
// The contract these tests pin down: a request naming a user-configured model
// reaches that upstream with its body byte-identical, and the answer comes back
// untouched. The free lane's fingerprint rewrite must never touch this path —
// a local llama.cpp server rejects a body carrying the injected quartet.

func newUpstreamServer(t *testing.T, ups []store.Upstream, upstreamURL string) *Server {
	t.Helper()
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	cfg := st.Config()
	cfg.Upstreams = ups
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	// A real Lane (never nil): the catalog endpoints read it for the free-lane
	// half, and a nil one would panic before the custom half is even reached.
	return New(lane.NewLane(), st)
}

// apiReq builds a gateway request carrying the main key: the completion routes
// are authenticated, and the custom-upstream branch sits behind that check.
func apiReq(t *testing.T, s *Server, method, path, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	if k := s.Store.Config().MainKey; k != "" {
		req.Header.Set("authorization", "Bearer "+k)
	}
	return req
}

// echoServer answers with whatever body it received, so a test can assert the
// payload was forwarded unmodified.
func echoServer(t *testing.T, seen *[]byte, hitPath *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		*seen = buf
		*hitPath = r.URL.Path
		w.Header().Set("content-type", "application/json")
		// A local server's own field, which the gateway's decoders would drop.
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"本地回复"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10},"local_extra":{"vendor":"x"}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func localUpstream(id, url string, models ...store.UpstreamModel) store.Upstream {
	return store.Upstream{ID: id, Name: "本地模型", BaseURL: url, Models: models}
}

func TestPassthroughChatBodyUnmodified(t *testing.T) {
	var seen []byte
	var hitPath string
	echo := echoServer(t, &seen, &hitPath)
	s := newUpstreamServer(t, []store.Upstream{
		localUpstream("local", echo.URL, store.UpstreamModel{ID: "meissa-local", Name: "Meissa"}),
	}, echo.URL)

	// Deliberately declares no tools and carries a vendor-specific body field:
	// a fingerprint rewrite would add the quartet and drop the field.
	body := `{"model":"meissa-local","messages":[{"role":"user","content":"hi"}],"vendor_flag":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	if k := s.Store.Config().MainKey; k != "" {
		req.Header.Set("authorization", "Bearer "+k)
	}
	rec := httptest.NewRecorder()
	s.route(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if string(seen) != body {
		t.Errorf("upstream body mutated\n got: %s\nwant: %s", seen, body)
	}
	if strings.Contains(string(seen), "\"bash\"") {
		t.Errorf("fingerprint quartet leaked into a passthrough body: %s", seen)
	}
	if hitPath != "/v1/chat/completions" {
		t.Errorf("upstream path = %q, want /v1/chat/completions", hitPath)
	}
	if got := rec.Header().Get("x-zen-gate-upstream"); got != "local" {
		t.Errorf("x-zen-gate-upstream = %q, want local", got)
	}
	// The upstream's own extra field must survive to the client.
	if !strings.Contains(rec.Body.String(), "local_extra") {
		t.Errorf("upstream response was rewritten, lost local_extra: %s", rec.Body.String())
	}
}

func TestPassthroughStreamIsNotBuffered(t *testing.T) {
	hit := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, f := range []string{
			`{"choices":[{"delta":{"content":"第一段"}}]}`,
			`{"choices":[{"delta":{"content":"第二段"}}]}`,
			`[DONE]`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", f)
			if fl != nil {
				fl.Flush()
			}
		}
		close(hit)
	}))
	defer srv.Close()
	s := newUpstreamServer(t, []store.Upstream{
		localUpstream("local", srv.URL, store.UpstreamModel{ID: "meissa-local"}),
	}, srv.URL)

	req := apiReq(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"meissa-local","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()
	s.route(rec, req)
	<-hit

	out := rec.Body.String()
	if !strings.Contains(out, "第一段") || !strings.Contains(out, "第二段") {
		t.Errorf("stream frames lost: %s", out)
	}
	if !strings.HasPrefix(out, "data: ") {
		t.Errorf("SSE framing not preserved verbatim: %q", out)
	}
}

func TestUnknownModelSkipsPassthrough(t *testing.T) {
	// A model nobody configured must not be routed to any custom endpoint; it
	// falls through to the free lane. The lane here points at a dead address, so
	// the assertion is that the answer is an upstream failure rather than the
	// echo server's payload.
	echo := echoServer(t, &[]byte{}, new(string))
	s := newUpstreamServer(t, []store.Upstream{
		localUpstream("local", echo.URL, store.UpstreamModel{ID: "meissa-local"}),
	}, "http://127.0.0.1:1")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req := apiReq(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"mimo-v2.6-flash-free","messages":[{"role":"user","content":"hi"}]}`)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	s.route(rec, req)

	if strings.Contains(rec.Body.String(), "local_extra") {
		t.Errorf("a free-lane model was wrongly proxied to the custom upstream: %s", rec.Body.String())
	}
}

func TestUpstreamUnreachableReports502(t *testing.T) {
	// 203.0.113.0/24 is TEST-NET-3 (RFC 5737) — reserved for documentation and
	// never routable, so the dial stalls rather than being refused instantly.
	// The assertion is that the failure surfaces as a 502 naming the upstream,
	// not as a hung request or a bogus 200.
	s := newUpstreamServer(t, []store.Upstream{
		localUpstream("local", "http://203.0.113.1:9", store.UpstreamModel{ID: "meissa-local"}),
	}, "")

	req := apiReq(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"meissa-local","messages":[{"role":"user","content":"hi"}]}`)
	// A context deadline stands in for the caller's own timeout so the test
	// cannot hang the suite.
	ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	s.route(rec, req)

	if rec.Code != 502 {
		t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "local") {
		t.Errorf("error should name the upstream id: %s", rec.Body.String())
	}
}

func TestUpstreamAPIKeySent(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("authorization")
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()

	s := newUpstreamServer(t, []store.Upstream{{
		ID: "remote", BaseURL: srv.URL, APIKey: "sk-local-test",
		Models: []store.UpstreamModel{{ID: "some-model"}},
	}}, srv.URL)

	req := apiReq(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"some-model","messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()
	s.route(rec, req)

	if gotAuth != "Bearer sk-local-test" {
		t.Errorf("authorization = %q, want Bearer sk-local-test", gotAuth)
	}
}

func TestUpstreamBaseURLPathPrefixHonoured(t *testing.T) {
	var hitPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()

	// A base URL carrying a path prefix is common behind a reverse proxy.
	s := newUpstreamServer(t, []store.Upstream{{
		ID: "proxied", BaseURL: srv.URL + "/llama",
		Models: []store.UpstreamModel{{ID: "meissa-local"}},
	}}, srv.URL)

	req := apiReq(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"meissa-local","messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()
	s.route(rec, req)

	if hitPath != "/llama/v1/chat/completions" {
		t.Errorf("upstream path = %q, want /llama/v1/chat/completions", hitPath)
	}
}

// --- catalog integration ----------------------------------------------------

func TestCustomModelsAppearInCodexCatalog(t *testing.T) {
	echo := echoServer(t, &[]byte{}, new(string))
	s := newUpstreamServer(t, []store.Upstream{{
		ID: "local", Name: "本地", BaseURL: echo.URL, ExposeRegion: false,
		Models: []store.UpstreamModel{
			{ID: "meissa-local", Name: "Meissa Qwen", ContextWindow: 32768, MaxOutput: 8192, Reasoning: true},
		},
	}}, echo.URL)

	req := httptest.NewRequest(http.MethodGet, "/v1/codex-catalog", nil)
	rec := httptest.NewRecorder()
	s.route(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("catalog is not valid JSON: %v", err)
	}
	var found map[string]any
	for _, m := range out.Models {
		if m["slug"] == "meissa-local" {
			found = m
		}
	}
	if found == nil {
		t.Fatalf("meissa-local missing from codex catalog: %s", rec.Body.String())
	}
	// Codex rejects the whole catalog unless every entry is fully populated.
	for _, k := range []string{"base_instructions", "supported_reasoning_levels", "shell_type",
		"truncation_policy", "experimental_supported_tools", "input_modalities",
		"provider_id", "context_window", "max_output_tokens"} {
		if _, ok := found[k]; !ok {
			t.Errorf("catalog entry missing required key %q", k)
		}
	}
	if found["display_name"] != "Meissa Qwen" {
		t.Errorf("display_name = %v, want Meissa Qwen", found["display_name"])
	}
	if got := found["context_window"].(float64); int(got) != 32768 {
		t.Errorf("context_window = %v, want 32768", found["context_window"])
	}
}

func TestCustomModelsAppearInModelsList(t *testing.T) {
	echo := echoServer(t, &[]byte{}, new(string))
	s := newUpstreamServer(t, []store.Upstream{{
		ID: "local", BaseURL: echo.URL,
		Models: []store.UpstreamModel{{ID: "meissa-local"}},
	}}, echo.URL)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("authorization", "Bearer "+s.Store.Config().MainKey)
	rec := httptest.NewRecorder()
	s.route(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "meissa-local") {
		t.Errorf("meissa-local missing from /v1/models: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "zen-gate/local") {
		t.Errorf("owned_by should mark the upstream: %s", rec.Body.String())
	}
}

func TestDefaultCapacitiesFilled(t *testing.T) {
	// Codex rejects a catalog entry without context_window/max_output_tokens,
	// so an upstream that declares neither must still produce a valid entry.
	echo := echoServer(t, &[]byte{}, new(string))
	s := newUpstreamServer(t, []store.Upstream{{
		ID: "local", BaseURL: echo.URL,
		Models: []store.UpstreamModel{{ID: "bare-model"}},
	}}, echo.URL)

	req := httptest.NewRequest(http.MethodGet, "/v1/codex-catalog", nil)
	rec := httptest.NewRecorder()
	s.route(rec, req)

	var out struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, m := range out.Models {
		if m["slug"] == "bare-model" {
			if m["context_window"] == nil || m["max_output_tokens"] == nil {
				t.Errorf("bare-model entry lacks capacities: %v", m)
			}
			return
		}
	}
	t.Errorf("bare-model missing from catalog: %s", rec.Body.String())
}

// --- store validation -------------------------------------------------------

func TestUpstreamValidate(t *testing.T) {
	ok := store.Upstream{ID: "local_1", BaseURL: "http://127.0.0.1:8090",
		Models: []store.UpstreamModel{{ID: "m"}}}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid upstream rejected: %v", err)
	}
	bad := []store.Upstream{
		{ID: "", BaseURL: "http://x", Models: []store.UpstreamModel{{ID: "m"}}},
		{ID: "Bad-Id", BaseURL: "http://x", Models: []store.UpstreamModel{{ID: "m"}}},
		{ID: "ok", BaseURL: "", Models: []store.UpstreamModel{{ID: "m"}}},
		{ID: "ok", BaseURL: "ftp://x", Models: []store.UpstreamModel{{ID: "m"}}},
		{ID: "ok", BaseURL: "http://x", Models: nil},
		{ID: "ok", BaseURL: "http://x", Models: []store.UpstreamModel{{ID: ""}}},
		{ID: "ok", BaseURL: "http://x", Models: []store.UpstreamModel{{ID: "dup"}, {ID: "dup"}}},
	}
	for i, u := range bad {
		if err := u.Validate(); err == nil {
			t.Errorf("case %d: expected rejection, got nil", i)
		}
	}
}

func TestUpstreamTrim(t *testing.T) {
	u := store.Upstream{ID: "  local ", BaseURL: " http://127.0.0.1:8090 ",
		Models: []store.UpstreamModel{{ID: " m ", Name: " Meissa ", Wire: " responses "}}}
	u.Trim()
	if u.ID != "local" || u.BaseURL != "http://127.0.0.1:8090" {
		t.Errorf("trim failed: %+v", u)
	}
	if u.Models[0].ID != "m" || u.Models[0].Name != "Meissa" {
		t.Errorf("model trim failed: %+v", u.Models[0])
	}
	if u.Models[0].Wire_() != "responses" {
		t.Errorf("Wire_ = %q, want responses", u.Models[0].Wire_())
	}
	if got := (store.UpstreamModel{Wire: "nonsense"}).Wire_(); got != "chat" {
		t.Errorf("unknown wire should default to chat, got %q", got)
	}
}

// --- admin write path -------------------------------------------------------

func TestAdminStateRedactsAPIKey(t *testing.T) {
	// The dashboard must never receive the key: a secret in the DOM is one XSS
	// away from leaving the machine.
	echo := echoServer(t, &[]byte{}, new(string))
	s := newUpstreamServer(t, []store.Upstream{{
		ID: "remote", BaseURL: echo.URL, APIKey: "sk-super-secret",
		Models: []store.UpstreamModel{{ID: "m"}},
	}}, echo.URL)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/state", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	rec := httptest.NewRecorder()
	s.route(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "sk-super-secret") {
		t.Fatalf("API key leaked into admin state: %s", body)
	}
	// The UI still needs to know one is set.
	if !strings.Contains(body, `"hasKey":true`) {
		t.Errorf("hasKey marker missing: %s", body)
	}
	if !strings.Contains(body, `"baseUrl":"`+echo.URL+`"`) {
		t.Errorf("non-secret fields should still be present: %s", body)
	}
}

func TestUpstreamListRedactsAPIKey(t *testing.T) {
	echo := echoServer(t, &[]byte{}, new(string))
	s := newUpstreamServer(t, []store.Upstream{{
		ID: "remote", BaseURL: echo.URL, APIKey: "sk-also-secret",
		Models: []store.UpstreamModel{{ID: "m"}},
	}}, echo.URL)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/upstreams", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	rec := httptest.NewRecorder()
	s.route(rec, req)

	if strings.Contains(rec.Body.String(), "sk-also-secret") {
		t.Errorf("API key leaked into upstreams listing: %s", rec.Body.String())
	}
}

func TestAdminUpstreamsSetRejectsInvalidWholesale(t *testing.T) {
	echo := echoServer(t, &[]byte{}, new(string))
	s := newUpstreamServer(t, nil, echo.URL)

	body := `{"upstreams":[
		{"id":"good","baseUrl":"` + echo.URL + `","models":[{"id":"m"}]},
		{"id":"BAD ID","baseUrl":"http://x","models":[{"id":"m"}]}
	]}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/upstreams", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:5000"
	rec := httptest.NewRecorder()
	s.route(rec, req)

	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	// All-or-nothing: the valid first entry must not have been persisted.
	if len(s.Store.Config().Upstreams) != 0 {
		t.Errorf("partial write applied despite validation failure: %+v", s.Store.Config().Upstreams)
	}
}

func TestAdminUpstreamsSetPersists(t *testing.T) {
	echo := echoServer(t, &[]byte{}, new(string))
	s := newUpstreamServer(t, nil, echo.URL)

	body := `{"upstreams":[{"id":"local","name":"本地","baseUrl":"` + echo.URL +
		`","models":[{"id":"meissa-local","contextWindow":32768}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/upstreams", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:5000"
	rec := httptest.NewRecorder()
	s.route(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := s.Store.Config().Upstreams
	if len(got) != 1 || got[0].ID != "local" {
		t.Fatalf("not persisted: %+v", got)
	}
	if _, ok := s.upstreamFor("meissa-local"); !ok {
		t.Errorf("saved upstream is not routable")
	}
}

func TestAdminUpstreamDelete(t *testing.T) {
	echo := echoServer(t, &[]byte{}, new(string))
	s := newUpstreamServer(t, []store.Upstream{
		localUpstream("local", echo.URL, store.UpstreamModel{ID: "meissa-local"}),
	}, echo.URL)

	req := httptest.NewRequest(http.MethodDelete, "/admin/api/upstreams/local", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	rec := httptest.NewRecorder()
	s.route(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, ok := s.upstreamFor("meissa-local"); ok {
		t.Errorf("deleted upstream is still routable")
	}
}

// TestPassthroughPreservesSSEFraming guards the specific failure that motivated
// the feature: a Codex-style streaming request to a local endpoint must not be
// re-framed into the gateway's own chunk shape.
func TestPassthroughPreservesSSEFraming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"A\"}}]}\n\n")
		fmt.Fprint(w, ": keepalive comment\n\n")
		fmt.Fprint(w, "data: {\"vendor_field\":1}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	s := newUpstreamServer(t, []store.Upstream{
		localUpstream("local", srv.URL, store.UpstreamModel{ID: "meissa-local"}),
	}, srv.URL)

	req := apiReq(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"meissa-local","stream":true,"messages":[]}`)
	rec := httptest.NewRecorder()
	s.route(rec, req)

	sc := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	frames := []string{}
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data:") {
			frames = append(frames, strings.TrimSpace(strings.TrimPrefix(sc.Text(), "data:")))
		}
	}
	if len(frames) != 3 {
		t.Fatalf("frames = %v, want 3 data frames", frames)
	}
	if frames[2] != "[DONE]" {
		t.Errorf("last frame = %q, want [DONE]", frames[2])
	}
	if !strings.Contains(frames[1], "vendor_field") {
		t.Errorf("vendor field lost mid-stream: %q", frames[1])
	}
	if !strings.Contains(rec.Body.String(), "keepalive") {
		t.Errorf("SSE comment dropped: %q", rec.Body.String())
	}
}
