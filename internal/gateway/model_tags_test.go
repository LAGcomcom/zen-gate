package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Every free-lane badge comes from the curated capability table — a rule, not a
// measurement. The row used to leave the source blank, so a rule looked exactly
// like a live probe verdict on the 模型 page.
func TestStateLabelsRuleSourcedCapabilities(t *testing.T) {
	up := fakeUpstream(t, []string{`{"choices":[{"delta":{"content":"x"}}]}`})
	defer up.Close()
	s := newTestServer(t, up)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/admin/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st struct {
		Models []struct {
			ID        string `json:"id"`
			TagSource string `json:"tagSource"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	for _, m := range st.Models {
		if m.ID == "mimo-v2.6-flash-free" {
			if want := "规则"; m.TagSource != want {
				t.Fatalf("tagSource = %q, want %q", m.TagSource, want)
			}
			return
		}
	}
	t.Fatal("mimo-v2.6-flash-free missing from state")
}
