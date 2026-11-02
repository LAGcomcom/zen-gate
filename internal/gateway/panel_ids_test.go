package gateway

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// The panel moved the account management cards into the channel modal (they
// live in .chan-pane[data-chan=…] now), but the bind functions kept their old
// `const card = $("#ca-card"); if(!card…) return` guard. Those ids no longer
// exist, so every click handler in 退出登录 / 优先使用 / 停用 / 授权登录 silently
// never bound — the buttons render and do nothing (issue #126).
//
// A guard that can never be true is invisible to every test that drives the
// handlers directly, so the cheapest permanent defence is a static one: every
// id the shipped script looks up through $("#…") must be declared somewhere in
// the same file.
func TestDashboardLookupsResolveToDeclaredIDs(t *testing.T) {
	idDecl := regexp.MustCompile(`id="([A-Za-z0-9_-]+)"`)
	lookup := regexp.MustCompile(`\$\("#([A-Za-z0-9_-]+)"\)`)
	declared := map[string]bool{}
	for _, m := range idDecl.FindAllStringSubmatch(string(dashboardHTML), -1) {
		declared[m[1]] = true
	}
	var offenders []string
	// 注释里也会出现 $("#id") 的写法，那不是查找，跳过整行注释。
	var code []string
	for _, line := range strings.Split(string(dashboardHTML), "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") || strings.HasPrefix(trimmed, "/*") {
			continue
		}
		code = append(code, line)
	}
	for _, m := range lookup.FindAllStringSubmatch(strings.Join(code, "\n"), -1) {
		if !declared[m[1]] {
			offenders = append(offenders, fmt.Sprintf("#%s", m[1]))
		}
	}
	if len(offenders) > 0 {
		t.Errorf("these ids are looked up but never declared, so their handlers never bind: %s",
			strings.Join(uniqStrings(offenders), ", "))
	}
}

func uniqStrings(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}