package relay

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"zen-gate/internal/store"
)

// SmallwebPreset is a supplier candidate discovered from the local crawler
// (netforce). It is NOT verified to answer — the dashboard marks it 待验证 and
// only promotes it to a real provider after the user probes it. We never claim
// these are working lanes; that would be inventing availability from a bare
// domain list.
type SmallwebPreset struct {
	Preset
	Confidence string `json:"confidence"` // high | medium | low
	Source     string `json:"source"`     // where it came from
}

// smallwebPath returns the crawler's harvested domain pool, overridable via
// ZENGATE_SMALLWEB. Returns "" when no source is configured or readable.
func smallwebPath() string {
	if p := os.Getenv("ZENGATE_SMALLWEB"); p != "" {
		return p
	}
	cands := []string{filepath.Join("..", "netforce", "_harvested.json")}
	if exe, err := os.Executable(); err == nil {
		cands = append([]string{filepath.Join(filepath.Dir(exe), "..", "..", "netforce", "_harvested.json")}, cands...)
	}
	for _, c := range cands {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// signals that a domain is plausibly an AI/LLM API surface (not a blog/wiki/course).
var (
	swAiRoot    = regexp.MustCompile(`(?i)(^|[\._-])(ai|llm|api|gpt|openai|chat|model|infer|intell|cognitive|nlp|ml|neuro|gen|synth|agent|bot)([\._-]|$)`)
	swApiPrefix = regexp.MustCompile(`(?i)^api\.|^llm\.|^ai\.|^ml\.|^infer\.`)
	swJunk      = regexp.MustCompile(`(?i)(blog|forum|news|wiki|shop|store|doc|help|support|video|music|img|pic|photo|game|download|cdn|static|mail|pay|bbs|tieba|zhihu|weibo|github|gitee|course|book|pdf|slide|ppt|\.edu|\.edu\.cn|\.ac\.|\.ac\.cn|\.edu\.au|\.ac\.uk)`)
	// course / brand / analyst noise that matches swAiRoot but is NOT a service.
	swNoise = regexp.MustCompile(`(?i)(fast\.ai|deeplearning\.ai|karpathy|artificialanalysis|lakera|dgl\.ai|fiddler|cognitiveclass|catboost|classyvision|gandalf|metadataremover|generateprompt|gipity|horizon3|lenso|nansen|cryptoaml|linnk|graphyn|kolosal|datasaur|deepware|arthur\.ai|codeflash|codeground|myportal|semanticscholar|tintup)`)
)

// SmallwebPresets loads the locally-crawled domain pool and returns the subset
// that looks like a small / niche AI API surface. Returns nil (never errors)
// when the crawler output is absent — the feature simply has no candidates.
// Candidates are unverified: the dashboard must label them 待验证 and let the
// user probe before treating them as real lanes.
func SmallwebPresets() []SmallwebPreset {
	path := smallwebPath()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doms []string
	if err := json.Unmarshal(data, &doms); err != nil {
		return nil
	}
	seen := map[string]bool{}
	out := make([]SmallwebPreset, 0, 32)
	for _, d := range doms {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" || seen[d] || !strings.Contains(d, ".") {
			continue
		}
		seen[d] = true
		if swJunk.MatchString(d) || swNoise.MatchString(d) {
			continue
		}
		if !swAiRoot.MatchString(d) {
			continue
		}
		conf := "low"
		if swApiPrefix.MatchString(d) {
			conf = "high"
		} else if !strings.Contains(d, "edu") && !strings.Contains(d, "org") {
			conf = "medium"
		}
		out = append(out, SmallwebPreset{
			Preset: Preset{
				ID:       "smallweb:" + d,
				Name:     d,
				BaseURL:  "https://" + d + "/v1",
				Protocol: store.ProtocolOpenAI,
				Note:     "本地爬取发现（尚未探测，可用性未知）· 点「探测」验证后再用",
				Tier:     "smallweb",
			},
			Confidence: conf,
			Source:     "netforce",
		})
	}
	sort.Slice(out, func(i, j int) bool {
		rank := map[string]int{"high": 0, "medium": 1, "low": 2}
		ri, rj := rank[out[i].Confidence], rank[out[j].Confidence]
		if ri != rj {
			return ri < rj
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > 60 {
		out = out[:60]
	}
	return out
}
