package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zen-gate/internal/store"
)

// The contract says GET only. A balance watch must never be able to trigger a
// claim, even if a declared endpoint happens to be a check-in URL.
func TestNodeBalanceProbeIsReadOnly(t *testing.T) {
	var methods, paths, auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		paths = append(paths, r.URL.Path)
		auths = append(auths, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"data":{"credits":42.5}}`)
	}))
	defer srv.Close()

	p := store.Provider{
		BaseURL: srv.URL + "/v1", APIKey: "sk-test",
		BalanceURL: "/account/credits", BalancePath: "data.credits", BalanceKind: "积分余额",
	}
	v, kind, err := nodeBalanceProbe(p)(context.Background())
	if err != nil || v != 42.5 || kind != "积分余额" {
		t.Fatalf("probe = %v, %q, %v", v, kind, err)
	}
	if len(methods) != 1 || methods[0] != http.MethodGet {
		t.Fatalf("balance watch issued %v, want exactly one GET", methods)
	}
	if paths[0] != "/v1/account/credits" {
		t.Fatalf("relative URL resolved to %q", paths[0])
	}
	if auths[0] != "Bearer sk-test" {
		t.Fatalf("same-origin endpoint did not get the key: %q", auths[0])
	}
}

// A node list is often pasted from a public place. A balance URL pointing at
// a DIFFERENT origin must never receive the user's API key.
func TestNodeBalanceProbeNeverLeaksKeyToForeignOrigin(t *testing.T) {
	var gotAuth = "unset"
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = fmt.Fprint(w, `{"credits":7}`)
	}))
	defer foreign.Close()

	p := store.Provider{
		BaseURL: "https://api.self.test/v1", APIKey: "sk-secret",
		BalanceURL: foreign.URL + "/credits", BalancePath: "credits",
	}
	v, _, err := nodeBalanceProbe(p)(context.Background())
	if err != nil || v != 7 {
		t.Fatalf("probe = %v, %v", v, err)
	}
	if gotAuth != "" {
		t.Fatalf("foreign origin received a credential: %q", gotAuth)
	}
}

func TestNodeBalanceProbeReportsUnreadableBalance(t *testing.T) {
	status := http.StatusInternalServerError
	body := `{"data":{"credits":1}}`
	path := "data.credits"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()
	p := store.Provider{BaseURL: srv.URL, BalanceURL: "/b", BalancePath: path}

	if _, _, err := nodeBalanceProbe(p)(context.Background()); err == nil {
		t.Error("HTTP 500 must be reported, not returned as a zero balance")
	}
	status, body = http.StatusOK, `{"data":{"credits":"not a number"}}`
	if _, _, err := nodeBalanceProbe(p)(context.Background()); err == nil {
		t.Error("a non-numeric value must be reported, not rounded to zero")
	}
	status, body, path = http.StatusOK, `{"data":{"credits":1}}`, "data.missing"
	if _, _, err := nodeBalanceProbe(store.Provider{BaseURL: srv.URL, BalanceURL: "/b", BalancePath: path})(context.Background()); err == nil {
		t.Error("a missing field must be reported")
	}
}

// End-to-end: a node list line declares checkin:daily + reset hour + a
// read-only balance endpoint. It must appear in the check-in registry as
// 待接入 (never "已签到"), expose the declared reset hour, and be watchable
// through the read-only refresh — all without any Go adapter.
func TestNodeDeclarationSuppliesResetHourAndReadOnlyWatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_, _ = fmt.Fprint(w, `{"data":{"credits":42.5}}`)
	}))
	defer srv.Close()

	_, ts := newNodesServer(t)
	defer ts.Close()
	text := fmt.Sprintf(`[{"name":"积分站","baseUrl":%q,"checkin":"daily",`+
		`"checkinResetHour":8,"balanceUrl":"/credits","balancePath":"data.credits",`+
		`"balanceKind":"积分余额","keys":["sk-node"]}]`, srv.URL+"/v1")
	if out := postAdmin(t, ts.URL+"/admin/api/nodes/import",
		mustJSONNodes(t, map[string]any{"text": text})); out["ok"] != true {
		t.Fatalf("node import failed: %+v", out)
	}

	code, j := getAdminJSON(t, ts.URL+"/admin/api/checkin/status")
	if code != http.StatusOK || j["ok"] != true {
		t.Fatalf("status = %d %+v", code, j)
	}
	var row map[string]any
	var nodeID string
	for _, raw := range j["tasks"].([]any) {
		candidate := raw.(map[string]any)
		if id, _ := candidate["id"].(string); strings.HasPrefix(id, "node:") {
			row, nodeID = candidate, id
		}
	}
	if row == nil {
		t.Fatalf("declared node missing from the check-in registry: %+v", j["tasks"])
	}
	if row["wired"] != false || row["doneToday"] != false || row["available"] != false {
		t.Fatalf("a declaration must stay 待接入: %+v", row)
	}
	if row["resetHour"] != float64(8) || row["hasProbe"] != true {
		t.Fatalf("declared reset hour / balance reader lost: %+v", row)
	}
	if next, _ := row["nextCheckAt"].(float64); next <= 0 {
		t.Fatalf("a watchable node needs a read-only verify schedule: %+v", row)
	}

	out := postAdmin(t, ts.URL+"/admin/api/checkin/refresh", fmt.Sprintf(`{"id":%q}`, nodeID))
	if out["ok"] != true {
		t.Fatalf("refresh = %+v", out)
	}
	rows := out["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("id-filtered refresh should return one row: %+v", rows)
	}
	r0 := rows[0].(map[string]any)
	if r0["ok"] != true || r0["balance"] != float64(42.5) {
		t.Fatalf("declared balance was not read: %+v", r0)
	}

	_, j = getAdminJSON(t, ts.URL+"/admin/api/checkin/status")
	for _, raw := range j["tasks"].([]any) {
		candidate := raw.(map[string]any)
		if candidate["id"] != nodeID {
			continue
		}
		if candidate["balance"] != float64(42.5) || candidate["balanceKind"] != "积分余额" {
			t.Fatalf("status did not surface the balance: %+v", candidate)
		}
		if candidate["doneToday"] != false || candidate["confirmed"] != false {
			t.Fatalf("a read-only watch must never confirm a check-in: %+v", candidate)
		}
		return
	}
	t.Fatal("node row vanished after refresh")
}
