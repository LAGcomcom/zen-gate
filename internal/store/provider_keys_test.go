package store

import "testing"

// Issue #104: a provider may carry several API keys for per-request load
// balancing. The store invariant is one key in APIKey, two or more in
// APIKeys, and EffectiveKeys is the only reader the rest of the codebase
// should need.
func TestEffectiveKeysDedupsAndFallsBack(t *testing.T) {
	p := Provider{ID: "p", APIKey: "legacy"}
	if got := p.EffectiveKeys(); len(got) != 1 || got[0] != "legacy" {
		t.Fatalf("legacy fallback = %v, want [legacy]", got)
	}
	p.APIKeys = []string{" k2 ", "k1", "k2", "", "k1"}
	got := p.EffectiveKeys()
	if len(got) != 2 || got[0] != "k2" || got[1] != "k1" {
		t.Fatalf("dedup/trim = %v, want [k2 k1] in list order", got)
	}
	// The returned slice must be a copy: trimming the caller's copy must not
	// touch the stored list.
	got[0] = "mutated"
	if p.APIKeys[0] != " k2 " {
		t.Fatalf("EffectiveKeys aliased the stored slice: %v", p.APIKeys)
	}
}

func TestCloneCopiesProviderKeyList(t *testing.T) {
	c := &Config{Providers: []Provider{{ID: "p", APIKeys: []string{"k1", "k2"}}}}
	next := c.clone()
	next.Providers[0].APIKeys[0] = "mutated"
	if c.Providers[0].APIKeys[0] != "k1" {
		t.Fatalf("clone aliased APIKeys: %v", c.Providers[0].APIKeys)
	}
}

// A masked export is the file people paste into issues: every balanced key
// must mask, not just the legacy field.
func TestMaskedHidesBalancedKeys(t *testing.T) {
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	st.Mutate(func(c *Config) {
		c.Providers = []Provider{{ID: "p", APIKeys: []string{"sk-alpha", "sk-beta"}}}
	})
	for _, k := range st.Masked().Providers[0].APIKeys {
		if k == "sk-alpha" || k == "sk-beta" {
			t.Fatalf("masked export leaked a key: %v", st.Masked().Providers[0].APIKeys)
		}
	}
}
