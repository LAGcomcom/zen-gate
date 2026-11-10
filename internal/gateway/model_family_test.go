package gateway

import (
	"testing"

	"zen-gate/internal/lane"
)

// 一个模型在免费车道、-free 变体、自定义供应商里各有一个 id。列表是把
// "选择"摆给用户看的地方：两条 id 同一份权重不是两个选择，是一条选择开了
// 两个门。合并只能保留一条，且要留下能正常答话的那条。
func TestCollapseFamiliesKeepsHealthyDoor(t *testing.T) {
	s := &Server{}
	list := []lane.ModelInfo{
		{ID: "deepseek-v4.1-flash-free", Channel: lane.ChannelZen}, // 限流中
		{ID: "deepseek-v4.1-flash", Channel: lane.ChannelCodeArts}, // 刚刚答过
		{ID: "api/deepseek-ai/DeepSeek-V4.1-Flash", Channel: ""},   // 自定义供应商，没测过
		{ID: "glm-5.3-flash", Channel: lane.ChannelWorkBuddy},      // 另一个模型
	}
	av := map[string]lane.ProbeResult{
		"deepseek-v4.1-flash-free": {State: lane.StateThrottled},
		"deepseek-v4.1-flash":      {State: lane.StateAvailable},
		"glm-5.3-flash":            {State: lane.StateUnknown},
	}

	got := s.collapseFamilies(list, av)
	if len(got) != 2 {
		t.Fatalf("应合并成 2 个模型，得到 %d 个：%+v", len(got), got)
	}
	seen := map[string]bool{}
	for _, m := range got {
		fam := lane.ModelFamily(m.ID)
		if seen[fam] {
			t.Fatalf("家族 %q 仍出现两次", fam)
		}
		seen[fam] = true
	}
	if !seen["deepseek-v4.1-flash"] {
		t.Fatal("deepseek 家族应留下可用的那条")
	}
	// 留下的必须是被判定可用的那个 id，不是列表里第一个。
	for _, m := range got {
		if lane.ModelFamily(m.ID) == "deepseek-v4.1-flash" && m.ID != "deepseek-v4.1-flash" {
			t.Fatalf("应保留刚答过的 id，得到 %q", m.ID)
		}
	}
}

// 已经判死的条目不该压过没测过的条目：未知比已知坏更值得一试。
func TestCollapseFamiliesPrefersUnknownOverDead(t *testing.T) {
	s := &Server{}
	list := []lane.ModelInfo{
		{ID: "api/deepseek-ai/DeepSeek-V4-Flash"},
		{ID: "deepseek-v4-flash"},
	}
	av := map[string]lane.ProbeResult{
		"deepseek-v4-flash": {State: lane.StateUnavailable},
	}
	got := s.collapseFamilies(list, av)
	if len(got) != 1 {
		t.Fatalf("应合并成 1 条，得到 %d", len(got))
	}
	if got[0].ID != "api/deepseek-ai/DeepSeek-V4-Flash" {
		t.Fatalf("判死的条目不应压过未测的供应商条目，留下的是 %q", got[0].ID)
	}
}

// 不同代/不同快照不能被合并——那会把请求改路由到用户没选的权重。
func TestCollapseFamiliesKeepsDistinctSnapshots(t *testing.T) {
	s := &Server{}
	list := []lane.ModelInfo{
		{ID: "deepseek-v4-pro"},
		{ID: "deepseek-v4-pro-0813"},
		{ID: "deepseek-v4.1-flash"},
	}
	got := s.collapseFamilies(list, nil)
	if len(got) != 3 {
		t.Fatalf("三个不同快照不应被合并，得到 %d 条：%+v", len(got), got)
	}
}
