package gateway

import "testing"

func TestMergeProviderKeys(t *testing.T) {
	got := mergeProviderKeys("x", []string{"x", "y", " y "})
	if len(got) != 2 || got[0] != "x" || got[1] != "y" {
		t.Fatalf("merge-dedupe got %v", got)
	}
	if got := mergeProviderKeys("only", nil); len(got) != 1 || got[0] != "only" {
		t.Fatalf("single got %v", got)
	}
	if got := mergeProviderKeys("", nil); len(got) != 0 {
		t.Fatalf("empty got %v", got)
	}
}

func TestFirstMergedKey(t *testing.T) {
	if got := firstMergedKey(providerIn{APIKey: "a", APIKeys: []string{"b"}}); got != "b" {
		t.Fatalf("order got %q", got)
	}
	if got := firstMergedKey(providerIn{APIKeys: []string{"x", "y"}}); got != "x" {
		t.Fatalf("list got %q", got)
	}
	if got := firstMergedKey(providerIn{}); got != "" {
		t.Fatalf("empty got %q", got)
	}
}
