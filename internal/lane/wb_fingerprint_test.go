package lane

import (
	"encoding/json"
	"strings"
	"testing"
)

// dshSystemPrompt is the body reported in issue #120, trimmed to the parts the
// upstream's content gate matched. Every occurrence here was present in the
// request that came back 400 even after the "sanitized" retry: the old table
// rewrote the first line and left the other thirteen alone.
const dshSystemPrompt = `You are an AI agent powered by DeepSeek Harness.
You are a coding agent powered by the deepseek-v4.1-flash model.
Agent Teams is available in this session
The Team Lead and all teammates share the same working directory and filesystem.

The runtime context names the DeepSeek Harness implementation checkout.
Use this checkout only to inspect or extend DSH itself.
You are interacting with the user through the DeepSeek Harness Web GUI, because only dsh web injects window.__DSH_BOOT__ into the page.

Current DSH file policy: danger-full-access.
Current DeepSeek Harness implementation checkout: "D:\deepseek\resources\app.asar\dsh\".
Current DeepSeek Harness Web GUI URL: "http://127.0.0.1:57533".

- diagnose-windows-sandbox-acl: Use on Windows for unexpected DSH sandbox access denials
`

// A body carrying the DSH fingerprint must come out with no product name left
// anywhere — messages, tools and the standalone system field alike.
func TestWBSanitizeLeavesNoDSHFingerprint(t *testing.T) {
	body := map[string]any{
		"model": "deepseek-v4.1-flash",
		"messages": []map[string]any{
			{"role": "system", "content": dshSystemPrompt},
			{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "Current DSH file policy: danger-full-access."},
			}},
		},
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "diagnose-windows-sandbox-acl",
					"description": "Use on Windows for unexpected DSH sandbox access denials",
				},
			},
		},
	}

	clean := wbSanitizeFingerprints(body)
	raw, err := json.Marshal(clean)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(raw)

	for _, marker := range []string{
		"DeepSeek Harness",
		"DSH",
		"__DSH_BOOT__",
		`app.asar\dsh`,
		`app.asar\\dsh`,
	} {
		if strings.Contains(out, marker) {
			t.Errorf("改写后仍残留 %q:\n%s", marker, out)
		}
	}

	// The identity of the model and the sandbox capability are payload, not
	// fingerprint: losing them would change what the request asks for.
	if !strings.Contains(out, "deepseek-v4.1-flash") {
		t.Error("模型名被误改，请求会打到别的模型")
	}
	if !strings.Contains(out, "danger-full-access") {
		t.Error("沙箱策略被误删，模型会按错误的权限行事")
	}
}

// The caller keeps its own copy for the recovery paths: rewriting in place would
// corrupt the body a later retry or a log line reads.
func TestWBSanitizeDoesNotMutateOriginal(t *testing.T) {
	orig := dshSystemPrompt
	body := map[string]any{
		"messages": []map[string]any{
			{"role": "system", "content": orig},
		},
	}
	_ = wbSanitizeFingerprints(body)

	msgs := body["messages"].([]map[string]any)
	if got := msgs[0]["content"].(string); got != orig {
		t.Fatalf("原 body 被就地修改，后续重试拿到的是改写过的内容")
	}
}

// The table's older entries must keep working: they cover the Codex / Claude
// clients, and 11128 is what the error text itself carries.
func TestWBSanitizeKeepsOlderRewrites(t *testing.T) {
	got := wbRewriteText("You are Claude Code. error 11128. Main branch (you will usually use this for PRs)")
	if strings.Contains(got, "You are Claude Code") {
		t.Errorf("Claude Code 自述未被改写: %q", got)
	}
	if strings.Contains(got, "11128") {
		t.Errorf("错误码未被拆开，会被当成特征: %q", got)
	}
	if strings.Contains(got, "Main branch") {
		t.Errorf("Main branch 未被改写: %q", got)
	}
}

// Plain text must survive untouched — a rewrite pass that mangles ordinary
// prompts would be worse than the gate it is avoiding.
func TestWBSanitizeKeepsPlainText(t *testing.T) {
	const plain = "请把 internal/lane/workbuddy.go 里的超时改成 30 秒，然后跑 subs 包的测试。"
	if got := wbRewriteText(plain); got != plain {
		t.Fatalf("普通文本被改动:\n  before: %q\n  after:  %q", plain, got)
	}
}
