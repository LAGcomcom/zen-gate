package store

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseNodeSpecsJSON(t *testing.T) {
	in := `[
	  {"name":"Alpha","baseUrl":"https://api.alpha.test/v1/","billing":"free",
	   "quota":{"rpd":2000,"reset":"daily","source":"declared"},
	   "models":["m1","m1","m2"],"keys":["sk-one"],"checkin":"daily"},
	  {"name":"Beta","baseUrl":"https://api.beta.test/v1","protocol":"anthropic","billing":"付费"}
	]`
	specs, problems := ParseNodeSpecs(in)
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if len(specs) != 2 {
		t.Fatalf("specs = %d, want 2", len(specs))
	}
	a := specs[0]
	if a.BaseURL != "https://api.alpha.test/v1" {
		t.Errorf("trailing slash not trimmed: %q", a.BaseURL)
	}
	if a.Billing != BillingFree || a.Quota == nil || a.Quota.RPD != 2000 || a.Quota.Source != "declared" {
		t.Errorf("billing/quota lost: %+v", a)
	}
	if len(a.Models) != 2 {
		t.Errorf("models not deduped: %v", a.Models)
	}
	if a.Protocol != ProtocolOpenAI || a.Checkin != "daily" {
		t.Errorf("defaults: %+v", a)
	}
	if specs[1].Billing != BillingPaid || specs[1].Protocol != ProtocolAnthropic {
		t.Errorf("Chinese billing / protocol not normalized: %+v", specs[1])
	}
}

// A paste from a note or a spreadsheet should work without converting it to
// JSON first. Three shapes are accepted: pipe columns, tabs, and a bare URL.
func TestParseNodeSpecsLines(t *testing.T) {
	in := `# 一行一个节点：名称 | baseUrl | 协议 | 计费 | key
Gamma | https://gamma.test/v1 | openai | free | sk-gamma
Delta	https://delta.test/v1	anthropic	paid	sk-delta
https://bare.test/v1
this line has no url`
	specs, problems := ParseNodeSpecs(in)
	if len(specs) != 3 {
		t.Fatalf("specs = %d, want 3: %+v (problems=%v)", len(specs), specs, problems)
	}
	if len(problems) != 1 {
		t.Errorf("the bad line must be reported, got %v", problems)
	}
	if specs[0].Name != "Gamma" || specs[0].Keys[0] != "sk-gamma" || specs[0].Billing != BillingFree {
		t.Errorf("pipe row = %+v", specs[0])
	}
	if specs[1].Name != "Delta" || specs[1].Protocol != ProtocolAnthropic {
		t.Errorf("tab row = %+v", specs[1])
	}
	if specs[2].Name != "bare.test" || specs[2].Billing != BillingUnknown {
		t.Errorf("bare URL row = %+v", specs[2])
	}
}

// One typo must not cost the other twenty nodes.
func TestParseNodeSpecsKeepsGoodLines(t *testing.T) {
	in := "Good | https://good.test/v1\nftp://not-http.test/v1\nAlsoGood | https://also.test/v1 | bogus-protocol\nLast | https://last.test/v1"
	specs, problems := ParseNodeSpecs(in)
	if len(specs) != 2 || len(problems) != 2 {
		t.Fatalf("specs=%d problems=%v, want 2/2", len(specs), problems)
	}
}

func TestNormalizeBillingRefusesGuesses(t *testing.T) {
	if got, _ := NormalizeBilling(""); got != BillingUnknown {
		t.Errorf("empty must stay unknown, got %q", got)
	}
	if _, err := NormalizeBilling("probably-free"); err == nil {
		t.Error("an unknown spelling must be refused, not silently classified")
	}
	for raw, want := range map[string]string{
		"免费": BillingFree, "混合": BillingFreemium, "paid": BillingPaid, "未知": BillingUnknown,
	} {
		if got, err := NormalizeBilling(raw); err != nil || got != want {
			t.Errorf("NormalizeBilling(%q) = %q, %v", raw, got, err)
		}
	}
}

// The portable list must be safe to paste into a chat, a gist or another
// machine: no key, in either field, ever.
func TestExportNodeSpecsDropsEveryCredential(t *testing.T) {
	providers := []Provider{{
		ID: "alpha", Name: "Alpha", BaseURL: "https://api.alpha.test/v1",
		APIKey: "sk-single", APIKeys: []string{"sk-a", "sk-b"},
		Billing: BillingFreemium, Quota: &NodeQuota{RPD: 500, Source: "declared"},
		SignupURL: "https://alpha.test/keys", Checkin: "daily",
		Models: []string{"m1"},
	}}
	out := ExportNodeSpecs(providers)
	if len(out) != 1 {
		t.Fatalf("export = %+v", out)
	}
	if len(out[0].Keys) != 0 {
		t.Errorf("export leaked keys: %v", out[0].Keys)
	}
	if out[0].Billing != BillingFreemium || out[0].Quota == nil || out[0].Quota.RPD != 500 ||
		out[0].SignupURL == "" || out[0].Checkin != "daily" {
		t.Errorf("declarations lost on export: %+v", out[0])
	}
	// A round trip must parse cleanly and still carry no credential.
	specs, problems := ParseNodeSpecs(mustJSON(t, out))
	if len(problems) != 0 || len(specs) != 1 || len(specs[0].Keys) != 0 {
		t.Errorf("round trip = %+v / %v", specs, problems)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Config snapshots are immutable. A Quota pointer shared across snapshots
// would let a later edit change what an in-flight request already read.
func TestConfigCloneDoesNotAliasNodeQuota(t *testing.T) {
	st := newAuditStore(t)
	st.Mutate(func(c *Config) {
		c.Providers = []Provider{{ID: "a", Name: "A", BaseURL: "https://a.test/v1",
			Quota: &NodeQuota{RPD: 100}, Billing: BillingFree}}
	})
	first := st.Config().Providers[0]
	st.Mutate(func(c *Config) {
		c.Providers[0].Quota.RPD = 999
		c.Providers[0].Billing = BillingPaid
	})
	if first.Quota.RPD != 100 || first.Billing != BillingFree {
		t.Errorf("old snapshot mutated: %+v", first)
	}
	if got := st.Config().Providers[0].Quota.RPD; got != 999 {
		t.Errorf("new snapshot = %d, want 999", got)
	}
}

func TestNormalizedNodeURLIsMergeKey(t *testing.T) {
	a := NormalizedNodeURL("  HTTPS://API.TEST/V1/  ")
	b := NormalizedNodeURL("https://api.test/v1")
	if a != b || strings.Contains(a, " ") {
		t.Errorf("merge key not stable: %q vs %q", a, b)
	}
}
