package store

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Billing tiers for an upstream node. Unknown is a first-class value, not a
// gap to be filled with a guess: COKEY's "honest quotas" rule (no rate-limit
// header means Quota: Unknown, never invented) applies to pricing too. A tier
// is only as good as the evidence behind it, so Free remains a claim the user
// or the provider's own listing has to make.
const (
	BillingFree     = "free"
	BillingFreemium = "freemium"
	BillingPaid     = "paid"
	BillingUnknown  = "unknown"
)

// NodeQuota is a published free-tier ceiling. Every number is optional and 0
// means "not stated", never "unlimited". Source records who said it: a
// provider's own docs ("declared") outranks a hand-entered guess, a live probe
// ("measured") outranks both — the same ladder the capability verdicts use.
type NodeQuota struct {
	RPM    int    `json:"rpm,omitempty"`
	RPD    int    `json:"rpd,omitempty"`
	TPD    int    `json:"tpd,omitempty"`
	Reset  string `json:"reset,omitempty"` // daily | monthly | none
	Source string `json:"source,omitempty"` // declared | measured
	Note   string `json:"note,omitempty"`
}

// NodeSpec is one node's importable declaration. It is deliberately data, not
// code: adding another supplier should be a line in a list, in the spirit of
// FreeLLMPool's "adding a provider is a key and a YAML block, never code" and
// COKEY's "adding a provider is a data change: append to the catalog".
//
// Keys are import-only. ExportNodeSpecs never fills them, so a portable node
// list can be shared, committed or pasted into another machine without leaking
// a credential (COKEY: "portable config (never secrets)").
type NodeSpec struct {
	Name      string     `json:"name"`
	BaseURL   string     `json:"baseUrl"`
	Protocol  string     `json:"protocol,omitempty"`
	Billing   string     `json:"billing,omitempty"`
	Quota     *NodeQuota `json:"quota,omitempty"`
	Models    []string   `json:"models,omitempty"`
	SignupURL string     `json:"signupUrl,omitempty"`
	// Checkin declares whether this node has a daily credit claim and which
	// family handles it. The existing WorkBuddy / CodeArts check-ins are two
	// separate implementations today; this field is the data contract a single
	// registry and scheduler will read next.
	Checkin string   `json:"checkin,omitempty"` // daily | none | ""
	Note    string   `json:"note,omitempty"`
	Keys    []string `json:"keys,omitempty"`
}

// ParseNodeSpecs accepts three shapes so a paste from anywhere works:
//
//  1. a JSON array (or single object) of node specs;
//  2. one line per node: name | baseUrl | protocol | billing | key
//     (trailing columns optional; tabs also work as separators);
//  3. a bare URL line — the name comes from its host.
//
// A malformed line is reported and skipped, never fatal to the whole batch:
// losing 20 good nodes because one had a typo is exactly the failure mode a
// bulk import exists to avoid. Empty lines and # comments are ignored.
func ParseNodeSpecs(text string) ([]NodeSpec, []string) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, []string{"清单为空"}
	}
	var specs []NodeSpec
	var problems []string
	if trimmed[0] == '[' || trimmed[0] == '{' {
		var arr []NodeSpec
		if trimmed[0] == '{' {
			var one NodeSpec
			if err := json.Unmarshal([]byte(trimmed), &one); err != nil {
				return nil, []string{"JSON 解析失败: " + err.Error()}
			}
			arr = []NodeSpec{one}
		} else if err := json.Unmarshal([]byte(trimmed), &arr); err != nil {
			return nil, []string{"JSON 解析失败: " + err.Error()}
		}
		for i, s := range arr {
			n, err := normalizeNodeSpec(s)
			if err != nil {
				problems = append(problems, fmt.Sprintf("第 %d 条: %v", i+1, err))
				continue
			}
			specs = append(specs, n)
		}
		return specs, problems
	}
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		s, err := parseNodeLine(line)
		if err != nil {
			problems = append(problems, fmt.Sprintf("第 %d 行: %v", i+1, err))
			continue
		}
		specs = append(specs, s)
	}
	return specs, problems
}

func parseNodeLine(line string) (NodeSpec, error) {
	parts := strings.Split(line, "|")
	if len(parts) == 1 {
		parts = strings.Split(line, "\t")
	}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	var s NodeSpec
	if len(parts) == 1 && strings.Contains(parts[0], "://") {
		s.BaseURL = parts[0]
	} else {
		s.Name = parts[0]
		if len(parts) > 1 {
			s.BaseURL = parts[1]
		}
		if len(parts) > 2 {
			s.Protocol = parts[2]
		}
		if len(parts) > 3 {
			s.Billing = parts[3]
		}
		if len(parts) > 4 && parts[4] != "" {
			s.Keys = []string{parts[4]}
		}
	}
	return normalizeNodeSpec(s)
}

// normalizeNodeSpec fills defaults and refuses anything we would otherwise
// have to guess about. A URL without a scheme is a typo, not an http endpoint.
func normalizeNodeSpec(s NodeSpec) (NodeSpec, error) {
	s.Name = strings.TrimSpace(s.Name)
	s.BaseURL = strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
	s.SignupURL = strings.TrimSpace(s.SignupURL)
	s.Note = strings.TrimSpace(s.Note)
	if s.BaseURL == "" {
		return NodeSpec{}, fmt.Errorf("缺少 baseUrl")
	}
	u, err := url.Parse(s.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return NodeSpec{}, fmt.Errorf("baseUrl 不是合法的 http(s) 地址: %q", s.BaseURL)
	}
	if s.Name == "" {
		s.Name = u.Host
	}
	s.Protocol = strings.ToLower(strings.TrimSpace(s.Protocol))
	if s.Protocol == "" {
		s.Protocol = ProtocolOpenAI
	}
	if s.Protocol != ProtocolOpenAI && s.Protocol != ProtocolAnthropic {
		return NodeSpec{}, fmt.Errorf("协议只能是 %s / %s，得到 %q", ProtocolOpenAI, ProtocolAnthropic, s.Protocol)
	}
	s.Billing, err = NormalizeBilling(s.Billing)
	if err != nil {
		return NodeSpec{}, err
	}
	s.Checkin = strings.ToLower(strings.TrimSpace(s.Checkin))
	if s.Checkin != "" && s.Checkin != "daily" && s.Checkin != "none" {
		return NodeSpec{}, fmt.Errorf("checkin 只能是 daily / none，得到 %q", s.Checkin)
	}
	if s.Quota != nil {
		q := *s.Quota
		if q.RPM < 0 || q.RPD < 0 || q.TPD < 0 {
			return NodeSpec{}, fmt.Errorf("额度不能为负数")
		}
		q.Source = strings.ToLower(strings.TrimSpace(q.Source))
		if q.Source != "" && q.Source != "declared" && q.Source != "measured" {
			return NodeSpec{}, fmt.Errorf("额度来源只能是 declared / measured")
		}
		q.Reset = strings.ToLower(strings.TrimSpace(q.Reset))
		if q.Reset != "" && q.Reset != "daily" && q.Reset != "monthly" && q.Reset != "none" {
			return NodeSpec{}, fmt.Errorf("额度重置只能是 daily / monthly / none")
		}
		s.Quota = &q
	}
	seen := map[string]bool{}
	models := make([]string, 0, len(s.Models))
	for _, m := range s.Models {
		m = strings.TrimSpace(m)
		if m != "" && !seen[m] {
			seen[m] = true
			models = append(models, m)
		}
	}
	s.Models = models
	keys := make([]string, 0, len(s.Keys))
	for _, k := range s.Keys {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	s.Keys = keys
	return s, nil
}

// NormalizeBilling accepts the spellings a human would actually type. Unknown
// is the default, not an error: refusing to classify is better than guessing.
func NormalizeBilling(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "unknown", "未知":
		return BillingUnknown, nil
	case "free", "免费":
		return BillingFree, nil
	case "freemium", "mixed", "混合", "部分免费":
		return BillingFreemium, nil
	case "paid", "付费":
		return BillingPaid, nil
	default:
		return "", fmt.Errorf("计费类型无法识别: %q（可用 free/freemium/paid/unknown）", raw)
	}
}

// ExportNodeSpecs renders a portable, secret-free node list. Every credential
// is dropped; the receiver supplies their own keys on import.
func ExportNodeSpecs(providers []Provider) []NodeSpec {
	out := make([]NodeSpec, 0, len(providers))
	for _, p := range providers {
		var q *NodeQuota
		if p.Quota != nil {
			cp := *p.Quota
			q = &cp
		}
		out = append(out, NodeSpec{
			Name:      p.Name,
			BaseURL:   p.BaseURL,
			Protocol:  p.Protocol,
			Billing:   p.Billing,
			Quota:     q,
			Models:    append([]string{}, p.Models...),
			SignupURL: p.SignupURL,
			Checkin:   p.Checkin,
			Note:      p.Note,
			// Keys intentionally absent: never export a credential.
		})
	}
	return out
}

// NormalizedNodeURL is the merge key. Two spellings of the same endpoint must
// not create two providers; case, trailing slash and whitespace are not
// identity. Exported so the gateway's bulk importer dedupes against the live
// config with exactly the same rule the parser uses.
func NormalizedNodeURL(raw string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(raw)), "/")
}
