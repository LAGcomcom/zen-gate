package lane

import "testing"

// ModelFamily decides what counts as "the same model". Getting it wrong in the
// permissive direction is worse than getting it wrong in the strict direction:
// folding two different sets of weights together routes a request to a model
// the caller did not pick, and nothing downstream can tell.
func TestModelFamily(t *testing.T) {
	cases := []struct{ id, want, why string }{
		{"deepseek-v4.1-flash", "deepseek-v4.1-flash", "车道原始 id 就是家族本身"},
		{"deepseek-v4.1-flash-free", "deepseek-v4.1-flash", "-free 说的是花谁的额度，不是哪个模型"},
		{"deepseek-v4.1-flash:free", "deepseek-v4.1-flash", "OpenRouter 风格的 :free 后缀"},
		{"codearts/deepseek-v4.1-flash", "deepseek-v4.1-flash", "供应商前缀是路由，不是身份"},
		{"ernie/deepseek-v4.1-flash", "deepseek-v4.1-flash", "同一个模型换了个门"},
		{"deepseek-ai/DeepSeek-V4.1-Flash", "deepseek-v4.1-flash", "厂商命名空间 + 大小写"},
		{"deepseek-v4.1-flash (deep)", "deepseek-v4.1-flash", "档位后缀属于同一份权重"},

		// 这些必须保持分开：合并它们会静默改写到另一份权重。
		{"deepseek-v4-pro", "deepseek-v4-pro", "与 4.1 不是同一个模型"},
		{"deepseek-v4.1-flash-0731", "deepseek-v4.1-flash-0731", "快照日期是不同权重"},
		{"deepseek-v4-flash-0731", "deepseek-v4-flash-0731", "4 与 4.1 不同代"},
		{"sn-deepseek-v4-1-flash-1017", "sn-deepseek-v4-1-flash-1017", "带渠道自己的前缀，不可安全合并"},
	}
	for _, c := range cases {
		if got := ModelFamily(c.id); got != c.want {
			t.Errorf("ModelFamily(%q) = %q，期望 %q（%s）", c.id, got, c.want, c.why)
		}
	}
}

// 家族键必须幂等：合并视图会把它当分组键反复使用。
func TestModelFamilyIdempotent(t *testing.T) {
	for _, id := range []string{"deepseek-v4.1-flash-free", "api/x/y", "GLM-5.3-Flash"} {
		once := ModelFamily(id)
		if twice := ModelFamily(once); twice != once {
			t.Errorf("ModelFamily 不幂等：%q → %q → %q", id, once, twice)
		}
	}
}
