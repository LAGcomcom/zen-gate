package gateway

import (
	"fmt"
	"net/http"
	"strings"

	"zen-gate/internal/logx"
	"zen-gate/internal/store"
)

// Bulk node import/export. The point is to make "接更多 API" a data operation:
// paste a list of suppliers once, keep their billing tier and published free
// quota as structured claims, and be able to carry that list to another machine
// without carrying anyone's keys.
//
// Merging follows the house rule: never delete, never overwrite a credential.
// A node whose endpoint already exists gets its metadata refreshed; its name,
// keys, models and enabled flag stay exactly as the user left them.

type nodePlan struct {
	Spec   store.NodeSpec
	Action string // add | update
	Index  int    // index into the live config for updates
}

type nodeResult struct {
	Name   string `json:"name"`
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
}

// planNodes dedupes within the batch (two spellings of one endpoint are one
// node) and classifies each against the live config. No writes happen here, so
// a dry run is a genuine preview rather than a rollback of a half-applied edit.
func planNodes(specs []store.NodeSpec, existing []store.Provider) ([]nodePlan, []nodeResult) {
	byURL := map[string]int{}
	for i, p := range existing {
		byURL[store.NormalizedNodeURL(p.BaseURL)] = i
	}
	seen := map[string]bool{}
	plans := make([]nodePlan, 0, len(specs))
	var results []nodeResult
	for _, sp := range specs {
		key := store.NormalizedNodeURL(sp.BaseURL)
		if seen[key] {
			results = append(results, nodeResult{Name: sp.Name, Action: "skip", Reason: "批内重复"})
			continue
		}
		seen[key] = true
		if idx, ok := byURL[key]; ok {
			plans = append(plans, nodePlan{Spec: sp, Action: "update", Index: idx})
			results = append(results, nodeResult{Name: sp.Name, Action: "update", Reason: "端点已存在，只刷新计费/额度/签到声明"})
		} else {
			plans = append(plans, nodePlan{Spec: sp, Action: "add", Index: -1})
			results = append(results, nodeResult{Name: sp.Name, Action: "add"})
		}
	}
	return plans, results
}

// slugNodeID mirrors mintProviderID's spelling but works against a local taken
// set: Config() is an immutable snapshot, so calling mintProviderID in a loop
// inside one Mutate would hand every new node the same id.
func slugNodeID(name string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	id := strings.Trim(b.String(), "-")
	if id == "" {
		return "api"
	}
	return id
}

func uniqueNodeID(name string, taken map[string]bool) string {
	base := slugNodeID(name)
	id := base
	for n := 2; taken[id]; n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	taken[id] = true
	return id
}

// refreshNodeMeta writes only the declarative fields a node list owns. A name
// the user typed, their keys, the model selection and the enabled switch are
// all user state and must survive a re-import untouched.
func refreshNodeMeta(p *store.Provider, sp store.NodeSpec) {
	if sp.Billing != "" && sp.Billing != store.BillingUnknown {
		p.Billing = sp.Billing
	}
	if sp.Quota != nil {
		q := *sp.Quota
		p.Quota = &q
	}
	if sp.SignupURL != "" {
		p.SignupURL = sp.SignupURL
	}
	if sp.Checkin != "" {
		p.Checkin = sp.Checkin
	}
	// The balance contract is a declaration, not user-entered state: re-import
	// refreshes it just like billing/quota. An omitted field leaves the old one
	// alone (incremental merge, never delete what the user did not overwrite).
	if sp.CheckinResetHour != 0 {
		p.CheckinResetHour = sp.CheckinResetHour
	}
	if sp.BalanceURL != "" {
		p.BalanceURL = sp.BalanceURL
	}
	if sp.BalancePath != "" {
		p.BalancePath = sp.BalancePath
	}
	if sp.BalanceKind != "" {
		p.BalanceKind = sp.BalanceKind
	}
	if sp.Note != "" {
		p.Note = sp.Note
	}
	if p.Protocol == "" {
		p.Protocol = sp.Protocol
	}
	if len(p.Models) == 0 && len(sp.Models) > 0 {
		p.Models = append([]string{}, sp.Models...)
	}
}

func providerFromNode(id string, sp store.NodeSpec) store.Provider {
	p := store.Provider{
		ID: id, Name: sp.Name, BaseURL: sp.BaseURL, Protocol: sp.Protocol,
		Enabled: true, Billing: sp.Billing, SignupURL: sp.SignupURL,
		Checkin: sp.Checkin, Note: sp.Note,
		CheckinResetHour: sp.CheckinResetHour,
		BalanceURL:       sp.BalanceURL,
		BalancePath:      sp.BalancePath,
		BalanceKind:      sp.BalanceKind,
		Models:           append([]string{}, sp.Models...),
	}
	if sp.Quota != nil {
		q := *sp.Quota
		p.Quota = &q
	}
	if len(sp.Keys) == 1 {
		p.APIKey = sp.Keys[0]
	} else if len(sp.Keys) > 1 {
		p.APIKeys = append([]string{}, sp.Keys...)
	}
	return p
}

// adminNodesImport parses, previews (dryRun) or applies a batch in one call.
// Bad lines are skipped and reported individually instead of voiding the batch.
func (s *Server) adminNodesImport(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text   string `json:"text"`
		DryRun bool   `json:"dryRun"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	specs, problems := store.ParseNodeSpecs(in.Text)
	if len(specs) == 0 {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "没有可导入的节点", "problems": problems})
		return
	}
	plans, results := planNodes(specs, s.Store.Config().Providers)
	added, updated, skipped := 0, 0, 0
	for _, res := range results {
		switch res.Action {
		case "add":
			added++
		case "update":
			updated++
		default:
			skipped++
		}
	}
	if in.DryRun {
		writeJSON(w, 200, map[string]any{
			"ok": true, "dryRun": true, "added": added, "updated": updated,
			"skipped": skipped, "items": results, "problems": problems,
		})
		return
	}

	var newModels []string
	s.Store.Mutate(func(c *store.Config) {
		taken := map[string]bool{}
		for _, p := range c.Providers {
			taken[p.ID] = true
		}
		for _, pl := range plans {
			if pl.Action == "update" {
				if pl.Index < 0 || pl.Index >= len(c.Providers) {
					continue
				}
				p := &c.Providers[pl.Index]
				if store.NormalizedNodeURL(p.BaseURL) != store.NormalizedNodeURL(pl.Spec.BaseURL) {
					continue // config changed under us; never edit the wrong row
				}
				refreshNodeMeta(p, pl.Spec)
				continue
			}
			id := uniqueNodeID(pl.Spec.Name, taken)
			np := providerFromNode(id, pl.Spec)
			c.Providers = append(c.Providers, np)
			newModels = append(newModels, np.Models...)
		}
	})
	if err := s.Store.Save(); err != nil {
		writeJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if s.tagger != nil && len(newModels) > 0 {
		s.tagger.Enqueue(newModels...)
	}
	s.logCat(logx.CatAdmin, "info", "节点清单批量注入: 新增 %d / 更新 %d / 跳过 %d", added, updated, skipped)
	writeJSON(w, 200, map[string]any{
		"ok": true, "added": added, "updated": updated,
		"skipped": skipped, "items": results, "problems": problems,
	})
}

// adminNodesExport returns a portable, secret-free node list that can be
// pasted straight back into the import box on this or another machine.
func (s *Server) adminNodesExport(w http.ResponseWriter, r *http.Request) {
	nodes := store.ExportNodeSpecs(s.Store.Config().Providers)
	writeJSON(w, 200, map[string]any{"ok": true, "nodes": nodes, "count": len(nodes)})
}
