package gateway

import (
	"testing"

	"zen-gate/internal/lane"
)

// A channel switched off on the wall must disappear from every model picker.
// The catalog is a merge of six channel buckets and is only rebuilt on a
// refresh, so a switch that is not applied at read time leaves the picker
// advertising rows no request can reach (issue #128-1) — and the row count is
// the user's only signal that the switch did anything.
func TestVisibleModelsDropsDisabledChannel(t *testing.T) {
	up := fakeUpstream(t, []string{`{"choices":[{"delta":{"content":"x"}}]}`, `data: [DONE]`})
	defer up.Close()
	s := newTestServer(t, up)

	before := s.VisibleModels()
	if len(before) == 0 {
		t.Fatal("冷启动目录不应为空：测试前提不成立")
	}
	zenIDs := map[string]bool{}
	for _, m := range before {
		if m.Channel == "" {
			zenIDs[m.ID] = true
		}
	}
	if len(zenIDs) == 0 {
		t.Fatal("fallback 目录应含 zen 行（Channel 为空），测试前提不成立")
	}

	if err := s.Store.SetChannelEnabled(lane.ChannelZen, false); err != nil {
		t.Fatalf("停用 zen 失败: %v", err)
	}
	for _, m := range s.VisibleModels() {
		if zenIDs[m.ID] {
			t.Fatalf("停用 zen 后 %q 仍在模型列表里", m.ID)
		}
	}

	// Re-enabling has to bring them straight back: the switch is a read-time
	// filter, not a destructive roster edit.
	if err := s.Store.SetChannelEnabled(lane.ChannelZen, true); err != nil {
		t.Fatalf("重新启用 zen 失败: %v", err)
	}
	back := false
	for _, m := range s.VisibleModels() {
		if zenIDs[m.ID] {
			back = true
			break
		}
	}
	if !back {
		t.Fatal("重新启用后 zen 模型应立刻回到列表")
	}
}

// A disabled channel must also leave the agent-facing injection list, which is
// derived from VisibleModels: otherwise the pickers of every adapter keep
// offering models the gateway will refuse.
func TestInjectableModelsDropsDisabledChannel(t *testing.T) {
	up := fakeUpstream(t, []string{`{"choices":[{"delta":{"content":"x"}}]}`, `data: [DONE]`})
	defer up.Close()
	s := newTestServer(t, up)

	if err := s.Store.SetChannelEnabled(lane.ChannelZen, false); err != nil {
		t.Fatal(err)
	}
	for _, m := range s.InjectableModels() {
		if m.Channel == "" {
			t.Fatalf("停用的 zen 渠道仍把 %q 注入给 agent 适配器", m.ID)
		}
	}
}
