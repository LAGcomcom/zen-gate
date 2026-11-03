package lane

import (
	"embed"
	"encoding/json"
	"log"
	"os"
	"regexp"
	"sync"
	"time"
)

//go:embed fingerprints.json
var fpEmbedFS embed.FS

// fpRule is one marker → stand-in rewrite the WAF would otherwise match inside
// request text.
type fpRule struct {
	re *regexp.Regexp
	to string
}

var (
	fpRulesMu sync.RWMutex
	fpRules   []fpRule
)

func init() {
	loadFingerprintRules()
	watchFingerprintRules()
}

// loadFingerprintRules compiles the rewrite table. When ZENGATE_FP_RULES points
// at a readable JSON file it overrides the embedded defaults, so an upstream
// harness change is a dropped file, not a recompile. A malformed or empty
// result keeps the previous table rather than wiping sanitization.
func loadFingerprintRules() {
	data, err := fpEmbedFS.ReadFile("fingerprints.json")
	if err != nil {
		log.Printf("[fp] embedded rules missing: %v", err)
		return
	}
	src := "embedded"
	if p := os.Getenv("ZENGATE_FP_RULES"); p != "" {
		if b, rerr := os.ReadFile(p); rerr == nil {
			data, src = b, p
		} else {
			log.Printf("[fp] override %s unreadable (%v); using embedded defaults", p, rerr)
		}
	}

	var raw []struct {
		RE string `json:"re"`
		To string `json:"to"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		log.Printf("[fp] %s rules malformed (%v); keeping previous table", src, err)
		return
	}

	compiled := make([]fpRule, 0, len(raw))
	for _, r := range raw {
		re, err := regexp.Compile(r.RE)
		if err != nil {
			log.Printf("[fp] skip bad rule %q: %v", r.RE, err)
			continue
		}
		compiled = append(compiled, fpRule{re: re, to: r.To})
	}
	if len(compiled) == 0 {
		log.Printf("[fp] %s produced no usable rules; keeping previous table", src)
		return
	}

	fpRulesMu.Lock()
	fpRules = compiled
	fpRulesMu.Unlock()
	log.Printf("[fp] loaded %d fingerprint rewrite rules from %s", len(compiled), src)
}

// watchFingerprintRules reloads the override file when it changes on disk, so
// live edits take effect without a restart. A 30s mtime poll is plenty for a
// config that changes rarely, and avoids pulling in an fs-notify dependency.
func watchFingerprintRules() {
	p := os.Getenv("ZENGATE_FP_RULES")
	if p == "" {
		return
	}
	fi, err := os.Stat(p)
	if err != nil {
		return
	}
	last := fi.ModTime()
	for range time.Tick(30 * time.Second) {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if fi.ModTime().After(last) {
			last = fi.ModTime()
			loadFingerprintRules()
		}
	}
}

// wbRewriteText applies the current rewrite table to a single string. It reads
// the table under a read lock so a background reload never races a request.
func wbRewriteText(text string) string {
	fpRulesMu.RLock()
	rules := fpRules
	fpRulesMu.RUnlock()
	for _, r := range rules {
		text = r.re.ReplaceAllString(text, r.to)
	}
	return text
}
