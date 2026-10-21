package gateway

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// dashboardBlock is dashboardFunc for a function that spans several lines: it
// starts at the marker and ends where the braces balance again. The login
// poller is too long to sit on one line, and copying it into the test would
// test the copy instead of the shipped page.
func dashboardBlock(t *testing.T, marker string) string {
	t.Helper()
	lines := strings.Split(string(dashboardHTML), "\n")
	start := -1
	for i, line := range lines {
		if strings.Contains(line, marker) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("the dashboard no longer defines %s", marker)
	}
	depth := 0
	var out []string
	for i := start; i < len(lines); i++ {
		out = append(out, lines[i])
		depth += strings.Count(lines[i], "{") - strings.Count(lines[i], "}")
		if depth == 0 {
			return strings.Join(out, "\n")
		}
	}
	t.Fatalf("unbalanced braces in %s", marker)
	return ""
}

// runBlockJS evaluates one extracted function inside a harness and decodes what
// it wrote to stdout (same contract as runDashboardJS, custom source).
func runBlockJS(t *testing.T, src, prelude, tail string) any {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not available")
	}
	body := prelude + "\n" + src + "\n" + tail + "\n"
	harness := filepath.Join(t.TempDir(), "h.mjs")
	if err := os.WriteFile(harness, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, harness).CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s\n--- script ---\n%s", err, out, body)
	}
	var got any
	line := strings.TrimSpace(string(out))
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("bad harness output %q: %v", line, err)
	}
	return got
}

// Authorizing a second (and third) account happens by clicking 授权登录 again
// while the first tab is still open, so every window must keep its own poller.
// A single shared timer would silently abandon whichever login started first —
// the user finishes that tab and nothing ever lands in the pool.
func TestQoderCardPollsEveryOpenLoginWindow(t *testing.T) {
	src := dashboardBlock(t, "function pollQoderLogin(")
	prelude := `
const qoderLoginTimers = new Map();
const intervals = new Map();
let nextTimer = 1;
const setInterval = (fn, ms) => { const id = nextTimer++; intervals.set(id, fn); return id; };
const clearInterval = (id) => { intervals.delete(id); };
const polled = [], toasts = [];
const api = async (path, body) => {
  polled.push(body.state);
  if (body.state === "s-first")   return { status: "ok", account: { label: "甲账号" } };
  if (body.state === "s-complete") return { status: "ok", account: { label: "乙账号" } };
  return { status: "pending" };
};
const toast = (msg) => { toasts.push(msg); };
const renderQoderCard = () => {};
`
	tail := `
pollQoderLogin("s-first");
pollQoderLogin("s-wait");
pollQoderLogin("s-wait"); // double click must not fork a second poller for one window
const opened = qoderLoginTimers.size;
for (const fn of [...intervals.values()]) await fn();
const afterFirst = [...qoderLoginTimers.keys()];
// A third account can start while the second window is still pending.
pollQoderLogin("s-complete");
const beforeSecond = qoderLoginTimers.size;
for (const fn of [...intervals.values()]) await fn();
process.stdout.write(JSON.stringify({
  opened, afterFirst, beforeSecond,
  left: [...qoderLoginTimers.keys()], toasts,
}));
`
	got, ok := runBlockJS(t, src, prelude, tail).(map[string]any)
	if !ok {
		t.Fatalf("harness output is not an object")
	}
	if got["opened"] != float64(2) {
		t.Errorf("running windows = %v, want 2 (the double click must be ignored)", got["opened"])
	}
	if got["beforeSecond"] != float64(2) {
		t.Errorf("windows while a third login starts = %v, want 2", got["beforeSecond"])
	}
	afterFirst := toStrings(got["afterFirst"])
	if len(afterFirst) != 1 || afterFirst[0] != "s-wait" {
		t.Errorf("after the first window finished, open windows = %v, want [s-wait]", afterFirst)
	}
	left := toStrings(got["left"])
	if len(left) != 1 || left[0] != "s-wait" {
		t.Errorf("open windows = %v, want only the unfinished one", left)
	}
	toasts := toStrings(got["toasts"])
	if len(toasts) != 2 {
		t.Fatalf("toasts = %v, want one per finished window", toasts)
	}
	if !strings.Contains(toasts[0], "甲账号") || !strings.Contains(toasts[1], "乙账号") {
		t.Errorf("each finished window must announce its own account: %v", toasts)
	}
}

func toStrings(v any) []string {
	rows, _ := v.([]any)
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		if s, ok := row.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
