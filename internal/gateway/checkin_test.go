package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func getAdminJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return resp.StatusCode, out
}

// Three daily jobs used to be scattered across two goroutines in main.go and
// one bespoke button. The registry has to show all three, plus any supplier
// that declares checkin:daily in its node list — as 待接入, never as done.
func TestCheckinStatusListsWiredTasksAndNodeDeclarations(t *testing.T) {
	_, ts := newNodesServer(t)
	defer ts.Close()
	text := `[{"name":"签到站","baseUrl":"https://checkin.test/v1","checkin":"daily","signupUrl":"https://checkin.test/signup"},` +
		`{"name":"普通站","baseUrl":"https://plain.test/v1","billing":"free"}]`
	if out := postAdmin(t, ts.URL+"/admin/api/nodes/import",
		mustJSONNodes(t, map[string]any{"text": text})); out["ok"] != true {
		t.Fatalf("node import failed: %+v", out)
	}

	code, j := getAdminJSON(t, ts.URL+"/admin/api/checkin/status")
	if code != http.StatusOK || j["ok"] != true {
		t.Fatalf("status = %d %+v", code, j)
	}
	tasks, ok := j["tasks"].([]any)
	if !ok || len(tasks) != 4 {
		t.Fatalf("tasks = %+v, want 3 wired + 1 declared", j["tasks"])
	}
	wired, planned := 0, 0
	for _, raw := range tasks {
		row := raw.(map[string]any)
		if row["wired"] == true {
			wired++
			continue
		}
		planned++
		if row["available"] != false || row["doneToday"] != false || row["lastDate"] != "" {
			t.Errorf("a declaration must not look claimable or claimed: %+v", row)
		}
		if id, _ := row["id"].(string); len(id) < 5 || id[:5] != "node:" {
			t.Errorf("declared task id = %v", row["id"])
		}
		if detail, _ := row["detail"].(string); !strings.Contains(detail, "https://checkin.test/signup") {
			t.Errorf("declared task lost its signup URL: %+v", row)
		}
	}
	if wired != 3 || planned != 1 {
		t.Errorf("wired/planned = %d/%d, want 3/1", wired, planned)
	}
}

// With no accounts logged in, "全部签到" must skip cleanly: no upstream calls,
// no errors, and a per-row result the panel can explain.
func TestCheckinAllSkipsUnavailableTasksWithoutErrors(t *testing.T) {
	_, ts := newNodesServer(t)
	defer ts.Close()
	out := postAdmin(t, ts.URL+"/admin/api/checkin/all", `{}`)
	if out["ok"] != true || out["claimed"].(float64) != 0 || out["failed"].(float64) != 0 {
		t.Fatalf("all = %+v", out)
	}
	results := out["results"].([]any)
	if len(results) != 3 {
		t.Fatalf("results = %+v, want 3 wired rows", results)
	}
	for _, raw := range results {
		row := raw.(map[string]any)
		if row["skipped"] != true || row["ran"] != false {
			t.Errorf("unavailable task must be skipped, not run: %+v", row)
		}
	}
}

func TestCheckinOneValidatesIDAndReportsOneRow(t *testing.T) {
	_, ts := newNodesServer(t)
	defer ts.Close()
	url := ts.URL + "/admin/api/checkin/one"
	if out := postAdmin(t, url, `{"id":""}`); out["ok"] != false {
		t.Errorf("empty id accepted: %+v", out)
	}
	if out := postAdmin(t, url, `{"id":"does-not-exist"}`); out["ok"] != false {
		t.Errorf("unknown id accepted: %+v", out)
	}
	out := postAdmin(t, url, `{"id":"codearts"}`)
	if out["ok"] != true {
		t.Fatalf("known id failed: %+v", out)
	}
	if results := out["results"].([]any); len(results) != 1 {
		t.Errorf("one task should return one row: %+v", results)
	}
}
