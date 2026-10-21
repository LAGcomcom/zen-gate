//go:build live

// Live probe: which host + method actually answers the WorkBuddy balance meter?
//
// Run it against a COPY of the live config (never the running gateway's own
// file, so nothing can race it):
//
//	ZEN_GATE_HOME=D:\tmp\wbprobe-home ZEN_WB_PROBE=1 \
//	  go test ./internal/lane/ -tags live -run TestLiveWBMeterProbe -v
//
// Prints status codes, envelope codes and where numbers live in the JSON —
// never a token.
package lane

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLiveWBMeterProbe(t *testing.T) {
	if os.Getenv("ZEN_WB_PROBE") == "" {
		t.Skip("live probe; set ZEN_WB_PROBE=1")
	}
	home := os.Getenv("ZEN_GATE_HOME")
	if home == "" {
		t.Fatal("ZEN_GATE_HOME must point at a copy of the live config")
	}
	raw, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Read the pool straight off the JSON: importing internal/store from a lane
	// test is an import cycle (store imports lane).
	var cfg struct {
		WorkBuddyAccounts []WBAccount `json:"workBuddyAccounts"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.WorkBuddyAccounts) == 0 {
		t.Fatal("no WorkBuddy accounts in the copied config")
	}
	for _, acct := range cfg.WorkBuddyAccounts {
		t.Logf("=== account %q realm=%s uid=%.8s…", acct.Nickname, wbRealmOf(acct), acct.UID)
		rc := wbRealmConfig(wbRealmOf(acct))
		for _, base := range dedupe([]string{rc.ChatBase, rc.BillingBase}) {
			for _, c := range meterCandidates() {
				code, body := probeDo(t, c.method, base+c.path, acct)
				t.Logf("  %-4s %s%s -> HTTP %d %s", c.method, base, c.path, code, probeSummarize(body))
			}
		}
	}
}

type meterCase struct {
	name, method, path string
}

// TestLiveWBMeterShape dumps one real get-user-resource envelope with every
// long string cut short, so the parser is written against the field names the
// meter actually publishes rather than the ones a doc claims it has.
//
//	ZEN_GATE_HOME=… ZEN_WB_DUMP=1 go test ./internal/lane/ -tags live -run TestLiveWBMeterShape -v
func TestLiveWBMeterShape(t *testing.T) {
	if os.Getenv("ZEN_WB_DUMP") == "" {
		t.Skip("live dump; set ZEN_WB_DUMP=1")
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
	for _, acct := range cfg.WorkBuddyAccounts {
		code, body := probeDo(t, http.MethodPost,
			wbRealmConfig(wbRealmOf(acct)).BillingBase+wbUserResourcePath, acct)
		t.Logf("=== %q realm=%s HTTP %d", acct.Nickname, wbRealmOf(acct), code)
		t.Logf("%s", shrinkJSON(body, 24))
		return // one account is enough to read the shape
	}
}

// shrinkJSON re-prints JSON with string values cut to n runes, so a field dump
// cannot leak a token, a mail address or a body of prose.
func shrinkJSON(body []byte, n int) string {
	var v any
	if json.Unmarshal(body, &v) != nil {
		return probeCut(probeOneline(string(body)), 200)
	}
	out, err := json.MarshalIndent(shrink(v, n), "", "  ")
	if err != nil {
		return "(marshal)"
	}
	return string(out)
}

func shrink(v any, n int) any {
	switch t := v.(type) {
	case string:
		r := []rune(t)
		if len(r) > n {
			return string(r[:n]) + "…"
		}
		return t
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[k] = shrink(val, n)
		}
		return m
	case []any:
		a := make([]any, 0, len(t))
		for _, val := range t {
			a = append(a, shrink(val, n))
		}
		return a
	default:
		return t
	}
}

// meterCandidates are read-only shapes. daily-checkin is deliberately absent:
// probing it would claim the day's reward as a side effect of diagnosis.
func meterCandidates() []meterCase {
	return []meterCase{
		{"resource-GET", http.MethodGet, wbUserResourcePath},
		{"resource-POST", http.MethodPost, wbUserResourcePath},
		{"activity-status", http.MethodPost, "/v2/billing/meter/checkin-activity-status"},
		{"dosage", http.MethodPost, "/v2/billing/meter/get-dosage-notify"},
	}
}

// probeDo sends one meter request with the account's desktop identity headers,
// exactly as wbBillingGet/wbBillingPost do, and returns the raw answer.
func probeDo(t *testing.T, method, url string, acct WBAccount) (int, []byte) {
	t.Helper()
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader("{}")
	}
	req, err := http.NewRequestWithContext(t.Context(), method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range workBuddyHeaders(acct, wbRequestID()) {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: laneHTTP.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return 0, []byte("transport error: " + err.Error())
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, data
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// probeSummarize reports the envelope code, its message and the JSON paths that
// carry balance-shaped numbers, without dumping a whole (large, personal) body.
func probeSummarize(body []byte) string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return "(empty)"
	}
	var v any
	if json.Unmarshal(body, &v) != nil {
		return "non-JSON: " + probeCut(probeOneline(s), 90)
	}
	msg, code := "", "-"
	if m, ok := v.(map[string]any); ok {
		for _, k := range []string{"msg", "message", "error"} {
			if str, ok := m[k].(string); ok && str != "" {
				msg = probeCut(probeOneline(str), 60)
				break
			}
		}
		if f, ok := toNum(m["code"]); ok {
			code = fmt.Sprintf("%.0f", f)
		}
	}
	hits := findNumericPaths(v, 6, "credit", "balance", "capacity", "remain", "point", "quota", "dosage")
	return fmt.Sprintf("code=%s msg=%q numbers: %s", code, msg, strings.Join(hits, ", "))
}

func probeCut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func probeOneline(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// findNumericPaths walks JSON and reports "<path>=<value>" for numeric leaves
// whose key matches one of the wanted words (case-insensitive).
func findNumericPaths(v any, maxDepth int, want ...string) []string {
	var out []string
	var walk func(prefix string, node any, depth int)
	walk = func(prefix string, node any, depth int) {
		if depth > maxDepth || len(out) >= 14 {
			return
		}
		switch n := node.(type) {
		case map[string]any:
			for k, val := range n {
				p := prefix + "." + k
				if f, ok := toNum(val); ok {
					for _, w := range want {
						if strings.Contains(strings.ToLower(k), w) {
							out = append(out, fmt.Sprintf("%s=%.4g", p, f))
							break
						}
					}
				}
				walk(p, val, depth+1)
			}
		case []any:
			for i, val := range n {
				if i >= 3 {
					return
				}
				walk(fmt.Sprintf("%s[%d]", prefix, i), val, depth+1)
			}
		}
	}
	walk("$", v, 0)
	return out
}
