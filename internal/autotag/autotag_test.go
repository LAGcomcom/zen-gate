package autotag

import (
	"strings"
	"testing"
)

func TestParseTagJSONClean(t *testing.T) {
	in := `[{"id":"gpt-4o-mini","vision":true,"audio":false,"file":true,"reasoning":false,"contextWindow":128000,"maxOutput":16384}]`
	tags := parseTagJSON(in, []string{"gpt-4o-mini"})
	tag, ok := tags["gpt-4o-mini"]
	if !ok {
		t.Fatalf("missing tag: %v", tags)
	}
	if !tag.Vision || tag.Audio || !tag.File || tag.ContextWindow != 128000 || tag.MaxOutput != 16384 {
		t.Fatalf("tag = %+v", tag)
	}
}

func TestParseTagJSONFencedAndProse(t *testing.T) {
	in := "好的，以下是结果：\n```json\n" +
		`[{"id":"m-one","vision":true},{"id":"m-two","audio":true}]` +
		"\n```\n以上内容仅供参考。"
	tags := parseTagJSON(in, []string{"m-one", "m-two"})
	if len(tags) != 2 {
		t.Fatalf("tags = %v", tags)
	}
	if !tags["m-one"].Vision || !tags["m-two"].Audio {
		t.Fatalf("tags = %+v", tags)
	}
}

func TestParseTagJSONDropsUnrequestedIds(t *testing.T) {
	in := `[{"id":"gpt-4o-mini","vision":true},{"id":"hallucinated-model","vision":true}]`
	tags := parseTagJSON(in, []string{"gpt-4o-mini"})
	if _, ok := tags["hallucinated-model"]; ok {
		t.Fatal("unrequested id must be dropped")
	}
	if len(tags) != 1 {
		t.Fatalf("tags = %v", tags)
	}
}

func TestParseTagJSONGarbage(t *testing.T) {
	for _, in := range []string{"", "no json here", "[{broken", `{"id":"x"}`} {
		if tags := parseTagJSON(in, []string{"x"}); len(tags) != 0 {
			t.Fatalf("parseTagJSON(%q) = %v, want empty", in, tags)
		}
	}
}

func TestParseTagJSONPartialFields(t *testing.T) {
	// An omitted bool means the model did not claim the capability — false.
	in := `[{"id":"m-one","vision":true,"contextWindow":999}]`
	tags := parseTagJSON(in, []string{"m-one"})
	if got := tags["m-one"]; !got.Vision || got.Audio || got.File || got.ContextWindow != 999 {
		t.Fatalf("tag = %+v", got)
	}
}

func TestParseTagJSONKeepsReasoning(t *testing.T) {
	// The 思考 badge on a user-added model is the tagger's verdict; dropping it
	// here leaves the card silent even though the answer carried it.
	in := `[{"id":"m-one","reasoning":true,"maxOutput":4096}]`
	tags := parseTagJSON(in, []string{"m-one"})
	got := tags["m-one"]
	if !got.Reasoning {
		t.Errorf("reasoning verdict dropped: %+v", got)
	}
	if got.MaxOutput != 4096 {
		t.Errorf("maxOutput = %d, want 4096", got.MaxOutput)
	}
}

func TestTagPromptListsEveryModel(t *testing.T) {
	p := tagPrompt([]string{"a-model", "b-model"})
	for _, id := range []string{"a-model", "b-model", "vision", "audio", "file"} {
		if !strings.Contains(p, id) {
			t.Fatalf("prompt missing %q:\n%s", id, p)
		}
	}
}
