package store

import "testing"

func TestProviderEffectiveKeys(t *testing.T) {
	cases := []struct {
		name string
		p    Provider
		want []string
	}{
		{"single", Provider{APIKey: "solo"}, []string{"solo"}},
		{"multi-list", Provider{APIKeys: []string{"a", "b", "c"}}, []string{"a", "b", "c"}},
		{"merge-dedupe", Provider{APIKey: "x", APIKeys: []string{"x", "y", " y "}}, []string{"x", "y"}},
		{"empty", Provider{}, nil},
		{"list-only", Provider{APIKeys: []string{"q"}}, []string{"q"}},
	}
	for _, c := range cases {
		got := c.p.EffectiveKeys()
		if len(got) != len(c.want) {
			t.Fatalf("%s: len %d want %d (%v)", c.name, len(got), len(c.want), got)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%s: got[%d]=%q want %q (%v)", c.name, i, got[i], c.want[i], got)
			}
		}
	}
}
