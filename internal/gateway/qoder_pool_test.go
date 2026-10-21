package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// qoderJSONBody wraps a literal so a test can read like the request it sends.
func qoderJSONBody(s string) *strings.Reader { return strings.NewReader(s) }

// qoderContainsFold is the case-insensitive contains the secret scan needs:
// "Cosy_Key" and "cosy_key" are the same leak.
func qoderContainsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

// forceQoderProbe clears the cached probe so the stub is actually read.
func forceQoderProbe(t *testing.T) {
	t.Helper()
	qoderProbeMu.Lock()
	qoderProbeAt = time.Time{}
	qoderProbeMu.Unlock()
}

// The Qoder card reads three things from the sidecar: whether it runs, whether
// the login is alive, and the account roster. These tests drive the real
// handlers against a stub sidecar, because the wiring (probe base URL, roster
// pass-through, action forwarding) is exactly where a card silently goes blank.
func withQoderStub(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	old := qoderBaseURL
	qoderBaseURL = srv.URL
	t.Cleanup(func() {
		qoderBaseURL = old
		qoderProbeMu.Lock()
		qoderProbeAt = time.Time{}
		qoderProbeMu.Unlock()
	})
}

// A sidecar older than the pool endpoint answers 404 there. That is "single
// account", not "broken": the card must still render the login and the models.
func TestQoderCardSurvivesSidecarWithoutPool(t *testing.T) {
	withQoderStub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"DeepSeek-Flash"},{"id":"Qwen3.8-Flash"}]}`))
		case "/pool":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"not found"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	forceQoderProbe(t)
	st := probeQoderCached(true)
	if !st.Running || !st.LoggedIn {
		t.Fatalf("stub sidecar was not read as running+logged in: %+v", st)
	}
	if len(st.ModelIDs) != 2 {
		t.Errorf("models = %v, want the two listed", st.ModelIDs)
	}
	if st.PoolEnabled {
		t.Error("a 404 on /pool must read as poolEnabled=false, not as an enabled empty pool")
	}
	if st.Pool != nil {
		t.Errorf("pool = %v, want nil when the sidecar has no pool", st.Pool)
	}
}

// With a pool, the roster and the IDE-login flag come straight through, and no
// credential material is added anywhere on the way.
func TestQoderCardPassesThePoolThrough(t *testing.T) {
	withQoderStub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"DeepSeek-Flash"}]}`))
		case "/pool":
			_, _ = w.Write([]byte(`{"ok":true,"enabled":true,"dir":"C:/x/accounts","ideLogin":true,
				"accounts":[
					{"id":"acc-1","label":"one","uid":"uid-1","enabled":true,"state":"ok"},
					{"id":"acc-2","label":"two","uid":"uid-2","enabled":false,"state":"disabled"},
					{"id":"acc-3","label":"three","uid":"uid-3","enabled":true,"state":"cooldown",
					 "cooldownUntil":"2026-01-01T00:00:00Z","lastError":"credit gone"}
				]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	forceQoderProbe(t)
	st := probeQoderCached(true)
	if !st.PoolEnabled {
		t.Fatal("poolEnabled = false, want true")
	}
	if st.PoolDir != "C:/x/accounts" {
		t.Errorf("poolDir = %q", st.PoolDir)
	}
	if !st.IDELogin {
		t.Error("ideLogin = false, want the sidecar's true")
	}
	if len(st.Pool) != 3 {
		t.Fatalf("pool rows = %d, want 3", len(st.Pool))
	}
	if st.Pool[2].State != "cooldown" || st.Pool[2].LastError != "credit gone" {
		t.Errorf("row 3 = %+v, want the cooldown state and its note", st.Pool[2])
	}
	raw, _ := json.Marshal(st)
	for _, secret := range []string{"cosy", "machine", "access_token"} {
		if qoderContainsFold(string(raw), secret) {
			t.Errorf("the card's status response carries %q:\n%s", secret, raw)
		}
	}
}

// The sidecar serves {"data":[…]} and the capability flags live on the rows:
// is_reasoning decides whether a model gets a thinking picker in the Agent, so
// reading the body as a bare array silently loses every flag and leaves the
// vendor's reasoning models looking like plain-text ones.
func TestParseQoderCatalogReadsFlagsFromTheRows(t *testing.T) {
	body := []byte(`{"data":[
		{"id":"DeepSeek-Flash","is_reasoning":true,"is_vl":false,"max_input_tokens":200000},
		{"id":"Qwen3.8-Flash","is_reasoning":false,"is_vl":true,"max_input_tokens":200000}
	]}`)
	catalog := parseQoderCatalog(body)
	if len(catalog) != 2 {
		t.Fatalf("catalog rows = %d, want 2", len(catalog))
	}
	byID := map[string]laneListMeta{}
	for _, m := range catalog {
		byID[m.ID] = laneListMeta{Reasoning: m.Reasoning, Vision: m.Vision, ContextWindow: m.ContextWindow}
	}
	if m := byID["DeepSeek-Flash"]; !m.Reasoning || m.Vision {
		t.Errorf("DeepSeek-Flash = %+v, want reasoning-only", m)
	}
	if m := byID["Qwen3.8-Flash"]; !m.Vision {
		t.Errorf("Qwen3.8-Flash = %+v, want the vision flag through", m)
	}
	if m := byID["DeepSeek-Flash"]; m.ContextWindow != 200000 {
		t.Errorf("DeepSeek-Flash context = %d, want the declared 200000", m.ContextWindow)
	}
}

// laneListMeta is the three fields the test above asserts on, kept as a local
// struct so a typo in a field name fails to compile rather than comparing zeros.
type laneListMeta struct {
	Reasoning     bool
	Vision        bool
	ContextWindow int
}

// The card's buttons go through the gateway, which forwards one action to the
// sidecar and invalidates the probe cache so the next paint is fresh.
func TestQoderPoolActionForwardsToSidecar(t *testing.T) {
	var got struct {
		Action  string `json:"action"`
		ID      string `json:"id"`
		Enabled *bool  `json:"enabled"`
	}
	withQoderStub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/pool/account":
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			_ = json.NewDecoder(r.Body).Decode(&got)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	s := &Server{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/qoder/pool",
		qoderJSONBody(`{"action":"toggle","id":"acc-2","enabled":false}`))
	s.adminQoderPool(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if got.Action != "toggle" || got.ID != "acc-2" || got.Enabled == nil || *got.Enabled {
		t.Errorf("sidecar received %+v, want toggle acc-2 enabled=false", got)
	}
}

// A sidecar that is down must produce a readable failure, not a 200 with an
// empty body: the card would otherwise look like "no accounts".
func TestQoderPoolActionReportsAnUnreachableSidecar(t *testing.T) {
	withQoderStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream down"}}`))
	})
	s := &Server{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/qoder/pool", qoderJSONBody(`{"action":"import"}`))
	s.adminQoderPool(rec, req)
	if rec.Code == 200 {
		t.Fatalf("an upstream failure was reported as success: %s", rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["ok"] != false {
		t.Errorf("body = %v, want ok:false", body)
	}
	if msg, _ := body["error"].(string); msg == "" {
		t.Errorf("no error text: %v", body)
	}
}
