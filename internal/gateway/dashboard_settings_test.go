package gateway

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// runDashboardJS evaluates the shipped dashboard helper (extracted from the
// embedded page, not a copy) inside a small harness and decodes what it wrote
// to stdout.
func runDashboardJS(t *testing.T, marker, prelude, tail string) any {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not available")
	}
	src := dashboardFunc(t, marker)
	harness := filepath.Join(t.TempDir(), "h.mjs")
	body := prelude + "\n" + src + "\n" + tail + "\n"
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

// 系统信息 printed the raw mode key for every proxy mode outside the two-word
// map, so the overview showed "system" / "rotate" where 设置 shows Chinese.
func TestDashboardProxyLabelCoversEveryMode(t *testing.T) {
	got := runDashboardJS(t, "function proxyLabel(", "", `
const cases = {
  system: "跟随系统",
  env: "跟随环境变量",
  direct: "直连",
  custom: "socks5://127.0.0.1:7891",
  rotate: "订阅轮询 (sing-box)",
};
const seen = {};
for (const [mode, want] of Object.entries(cases)) {
  const v = want === "socks5://127.0.0.1:7891" ? proxyLabel(mode, want) : proxyLabel(mode, "");
  if (!/[\u4e00-\u9fff]/.test(v) && mode !== "custom") {
    throw new Error("mode " + mode + " still prints its raw key: " + v);
  }
  seen[mode] = v;
}
if (proxyLabel("custom", "") === "") throw new Error("custom with no URL must say something");
process.stdout.write(JSON.stringify(seen));
`)
	labels := got.(map[string]any)
	if labels["custom"] != "socks5://127.0.0.1:7891" {
		t.Errorf("custom = %v, want the URL the user typed", labels["custom"])
	}
	if labels["rotate"] != "订阅轮询 (sing-box)" {
		t.Errorf("rotate = %v", labels["rotate"])
	}
}

// The 10 second state refresh rewrote the number inputs, so a half-typed
// 默认输出上限 vanished mid-edit.
func TestDashboardFillSkipsTheFocusedInput(t *testing.T) {
	got := runDashboardJS(t, "function fill(", `
const document = { activeElement: null };
`, `
const el = { value: "8192" };
document.activeElement = el;
fill(el, 4096);
const whileTyping = el.value;
document.activeElement = null;
fill(el, 4096);
process.stdout.write(JSON.stringify([whileTyping, el.value]));
`)
	pair := got.([]any)
	if pair[0] != "8192" {
		t.Errorf("the focused input was overwritten: %v, want the user's 8192 kept", pair[0])
	}
	if pair[1] != "4096" {
		t.Errorf("an unfocused input = %v, want the server value applied", pair[1])
	}
}

// 设置 rebuilds its field values from the server state every 10 seconds, so any
// input assigned straight from that refresh erases what the user is typing.
// fill() is the guarded path; #set-proxy-url still used a bare assignment.
func TestDashboardSettingsInputsSkipTheFocusedField(t *testing.T) {
	assign := regexp.MustCompile(`\("#set-[a-z-]+"\)(?:\?[.\w]*)?\.value\s*=[^=]`)
	var offenders []string
	for i, line := range strings.Split(string(dashboardHTML), "\n") {
		if !assign.MatchString(line) {
			continue
		}
		if strings.Contains(line, "fill(") {
			continue
		}
		offenders = append(offenders, fmt.Sprintf("index.html:%d: %s", i+1, strings.TrimSpace(line)))
	}
	if len(offenders) > 0 {
		t.Errorf("these 设置 inputs are rewritten on every refresh, so typing gets clobbered — route them through fill():\n%s",
			strings.Join(offenders, "\n"))
	}
}

// The log auto-refresh timer was created only by the sidebar's click handler,
// so opening 日志 from the overview (or starting on it) left a static page.
func TestDashboardLogTimerFollowsTheVisiblePage(t *testing.T) {
	got := runDashboardJS(t, "function syncLogTimer(", `
let cur = "";
const started = [], cleared = [];
let next = 1;
const setInterval = (fn, ms) => { started.push(ms); return next++; };
const clearInterval = (h) => { cleared.push(h); };
const renderLogs = () => {};
`, `
cur = "logs";
let h = syncLogTimer(null);
const afterOpen = { started: started.slice(), cleared: cleared.slice(), handle: h };
h = syncLogTimer(h);
const afterRepeat = { started: started.slice(), cleared: cleared.slice(), handle: h };
cur = "dash";
h = syncLogTimer(h);
const afterLeave = { started: started.slice(), cleared: cleared.slice(), handle: h };
process.stdout.write(JSON.stringify([afterOpen, afterRepeat, afterLeave]));
`)
	rows := got.([]any)
	open := rows[0].(map[string]any)
	if len(open["started"].([]any)) != 1 {
		t.Errorf("opening 日志 started %v timers, want one", open["started"])
	}
	repeat := rows[1].(map[string]any)
	if len(repeat["started"].([]any)) != 1 || repeat["handle"] != open["handle"] {
		t.Errorf("rendering 日志 again stacked a timer: %v", repeat)
	}
	leave := rows[2].(map[string]any)
	if len(leave["cleared"].([]any)) != 1 || leave["handle"] != nil {
		t.Errorf("leaving 日志 left the timer running: %v", leave)
	}
}
