package gateway

import (
	"strings"
	"testing"

	"zen-gate/internal/lane"
)

func toolMsg(text string) lane.Message {
	return lane.Message{Role: lane.RoleTool, Parts: []lane.Part{lane.TextPart{Text: text}}}
}

func TestShrinkToolResultsShortTextUntouched(t *testing.T) {
	in := []lane.Message{toolMsg("ok"), toolMsg("short"), {Role: lane.RoleUser, Parts: []lane.Part{lane.TextPart{Text: "hi"}}}}
	out := shrinkToolResults(in, 6000)
	if got := out[0].TextOf(); got != "ok" {
		t.Fatalf("short text changed: %q", got)
	}
}

func TestShrinkToolResultsTrimsLongOutput(t *testing.T) {
	long := strings.Repeat("a", 12000)
	in := []lane.Message{toolMsg(long), {Role: lane.RoleUser, Parts: []lane.Part{lane.TextPart{Text: "next"}}}, {Role: lane.RoleAssistant, Parts: []lane.Part{lane.TextPart{Text: "tail"}}}}
	out := shrinkToolResults(in, 6000)
	got := out[0].TextOf()
	if len(got) >= len(long) {
		t.Fatalf("expected trimming, got %d chars", len(got))
	}
	if !strings.Contains(got, "[truncated") {
		t.Fatalf("missing truncation marker: %q", got)
	}
	if !strings.HasPrefix(got, "aaaa") {
		t.Fatalf("head lost")
	}
}

func TestShrinkToolResultsSkipsLastTwo(t *testing.T) {
	long := strings.Repeat("x", 9000)
	in := []lane.Message{toolMsg(long), toolMsg(long), toolMsg(long)}
	out := shrinkToolResults(in, 1000)
	if out[1].TextOf() != long || out[2].TextOf() != long {
		t.Fatalf("last two messages must stay intact")
	}
	if out[0].TextOf() == long {
		t.Fatalf("first message should be trimmed")
	}
}

func TestShrinkToolResultsMultibyteSafe(t *testing.T) {
	long := strings.Repeat("中", 5000)
	in := []lane.Message{toolMsg(long), toolMsg("a"), toolMsg("b")}
	out := shrinkToolResults(in, 1000)
	got := out[0].TextOf()
	if strings.ContainsRune(got, '\uFFFD') {
		t.Fatalf("truncation split a multi-byte rune")
	}
	if !strings.HasPrefix(got, "中") {
		t.Fatalf("head lost")
	}
}

func TestShrinkToolResultsDisabled(t *testing.T) {
	long := strings.Repeat("z", 9000)
	in := []lane.Message{toolMsg(long)}
	if out := shrinkToolResults(in, 0); out[0].TextOf() != long {
		t.Fatalf("max<=0 must pass through")
	}
}

func TestShrinkToolResultsUserToolResultPart(t *testing.T) {
	long := strings.Repeat("q", 9000)
	in := []lane.Message{
		{Role: lane.RoleUser, Parts: []lane.Part{lane.ToolResultPart{ToolCallID: "1", Text: long}}},
		toolMsg("a"), toolMsg("b"),
	}
	out := shrinkToolResults(in, 1000)
	tr, ok := out[0].Parts[0].(lane.ToolResultPart)
	if !ok {
		t.Fatalf("part type changed")
	}
	if len(tr.Text) >= len(long) || !strings.Contains(tr.Text, "[truncated") {
		t.Fatalf("tool result part was not trimmed")
	}
	if tr.ToolCallID != "1" {
		t.Fatalf("tool call id lost")
	}
}
