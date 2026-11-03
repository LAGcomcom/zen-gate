package gateway

import (
	"net/http"
	"strings"

	"zen-gate/internal/lane"
)

// The 渠道 wall's one data source.
//
// Every channel this gateway serves appears here exactly once: the two keyless
// free lanes, the two device-authorization channels (ZCode / TRAE), and the six
// preset channels whose credential is a pasted token. The wall used to assemble
// this from four separate endpoints (three *status calls plus the channel files
// inside state), which meant a card could render with no account count for the
// two seconds those calls were in flight, and a fifth channel type would have
// needed a fifth fetch on the client.
//
// Kept off /admin/api/state on purpose: state is polled every 10 seconds and
// this payload carries every account of every channel.

// How a card gets an account. This is the only thing the UI needs in order to
// decide which controls to draw, so the backend states it instead of the client
// guessing from the channel id.
const (
	wallLoginNone    = ""        // keyless: nothing to add (zen / kilo)
	wallLoginPaste   = "paste"   // the user pastes a token from the vendor's app
	wallLoginBrowser = "browser" // this gateway runs the authorization flow
)

// wallAccounts counts one channel's pool.
type wallAccounts struct {
	Total   int `json:"total"`
	Enabled int `json:"enabled"`
}

// channelWallRow is one card's worth of state.
type channelWallRow struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Blurb   string `json:"blurb,omitempty"`
	Login   string `json:"login"`
	Enabled bool   `json:"enabled"`
	// Accounts counts the pool. A keyless lane reports 0/0 rather than 1/1: a
	// synthetic "one account" would draw a 已登录 badge on a channel that has no
	// login at all.
	Accounts wallAccounts `json:"accounts"`
	Models   int          `json:"models"`
	// Extra is the one identity line the card shows (a ZCode userId, a TRAE
	// nickname, the first account's label). Optional.
	Extra string `json:"extra,omitempty"`
}

// adminChannelWall serves every channel with its live account pool.
func (s *Server) adminChannelWall(w http.ResponseWriter) {
	models := map[string]int{}
	for _, v := range s.Store.ChannelViews() {
		id, _ := v["id"].(string)
		if id == "" {
			continue
		}
		if n, ok := v["models"].(int); ok {
			models[id] = n
		}
	}
	rows := make([]channelWallRow, 0, 12)
	rows = append(rows, s.wallKeylessRows(models)...)
	rows = append(rows, s.wallWorkBuddyRow(models)...)
	rows = append(rows, s.wallCodeArtsRow(models)...)
	rows = append(rows, s.wallQoderRow(models)...)
	rows = append(rows, s.wallZCodeRow(models)...)
	rows = append(rows, s.wallTraeRow(models)...)
	rows = append(rows, s.wallExtRows(models)...)
	out := map[string]any{"ok": true, "channels": rows}
	writeJSON(w, 200, out)
}

// wallKeylessRows covers zen and kilo: channels with no account pool at all.
func (s *Server) wallKeylessRows(models map[string]int) []channelWallRow {
	out := make([]channelWallRow, 0, 2)
	for _, id := range []string{lane.ChannelZen, lane.ChannelKilo} {
		out = append(out, channelWallRow{
			ID: id, Label: channelLabel(id), Blurb: channelBlurb(id),
			Login: wallLoginNone, Enabled: s.Store.ChannelEnabled(id),
			Models: models[id],
		})
	}
	return out
}

// wallWorkBuddyRow covers the WorkBuddy account pool (intl + CN realms in one
// pool; each account names its own realm).
func (s *Server) wallWorkBuddyRow(models map[string]int) []channelWallRow {
	views := s.Lane.WorkBuddyViews()
	acc := wallAccounts{Total: len(views)}
	extra := ""
	for _, v := range views {
		if v.Enabled && strings.TrimSpace(v.AccessToken) != "" {
			acc.Enabled++
		}
		if extra == "" {
			extra = wallFirstNonEmpty(v.Nickname, v.UID, v.ID)
		}
	}
	return []channelWallRow{{
		ID: lane.ChannelWorkBuddy, Label: "WorkBuddy 账号渠道", Blurb: channelBlurb(lane.ChannelWorkBuddy),
		Login: wallLoginBrowser, Enabled: s.Store.ChannelEnabled(lane.ChannelWorkBuddy),
		Accounts: acc, Models: models[lane.ChannelWorkBuddy], Extra: extra,
	}}
}

// wallCodeArtsRow covers the Huawei Cloud CodeArts account. It holds exactly one
// account (a single SSO identity), so the pool is 0/0 or 1/1 — never "2".
func (s *Server) wallCodeArtsRow(models map[string]int) []channelWallRow {
	acct := s.Lane.CodeArtsSnapshot()
	acc := wallAccounts{}
	extra := ""
	if acct.SecurityToken != "" {
		if acct.Enabled {
			acc.Enabled = 1
		}
		acc.Total = 1
		extra = wallFirstNonEmpty(acct.UserName, acct.UserID)
	}
	return []channelWallRow{{
		ID: lane.ChannelCodeArts, Label: "CodeArts 账号渠道", Blurb: channelBlurb(lane.ChannelCodeArts),
		Login: wallLoginBrowser, Enabled: s.Store.ChannelEnabled(lane.ChannelCodeArts),
		Accounts: acc, Models: models[lane.ChannelCodeArts], Extra: extra,
	}}
}

// wallQoderRow covers the Qoder sidecar's pool. The sidecar is a separate
// process: an unreachable one reports an empty pool rather than an error,
// because the reason it is unreachable is stated in its own card.
//
// Its model count comes from the synced provider entry, not from
// channels/qoder.json: Qoder publishes its models as a custom provider
// ("qoder/…") that the gateway keeps in sync with the sidecar, so the channel
// file never holds a roster. Reading the file would show 0 models next to a
// working pool.
func (s *Server) wallQoderRow(models map[string]int) []channelWallRow {
	st := probeQoderCached(false)
	acc := wallAccounts{}
	extra := ""
	if st.Running {
		acc.Total = len(st.Pool)
		for _, a := range st.Pool {
			if a.Enabled {
				acc.Enabled++
			}
			if extra == "" {
				extra = wallFirstNonEmpty(a.Label, a.UID, a.ID)
			}
		}
	}
	qmodels := models[lane.ChannelQoder]
	enabled := true
	for _, p := range s.Store.Config().Providers {
		if p.ID != lane.ChannelQoder {
			continue
		}
		qmodels = len(p.Models)
		enabled = p.Enabled
		break
	}
	return []channelWallRow{{
		ID: lane.ChannelQoder, Label: "Qoder 账号渠道", Blurb: channelBlurb(lane.ChannelQoder),
		Login: wallLoginBrowser, Enabled: enabled,
		Accounts: acc, Models: qmodels, Extra: extra,
	}}
}

// wallZCodeRow covers the 智谱 device-authorization channel.
func (s *Server) wallZCodeRow(models map[string]int) []channelWallRow {
	accts := s.Lane.ZCodeAccounts()
	acc := wallAccounts{Total: len(accts)}
	extra := ""
	for _, a := range accts {
		if a.Enabled && strings.TrimSpace(a.JWT) != "" {
			acc.Enabled++
		}
		if extra == "" {
			extra = wallFirstNonEmpty(a.Label, a.UserID, a.ID)
		}
	}
	return []channelWallRow{{
		ID: lane.ChannelZCode, Label: "ZCode (智谱)", Blurb: channelBlurb(lane.ChannelZCode),
		Login: wallLoginBrowser, Enabled: s.Store.ChannelEnabled(lane.ChannelZCode),
		Accounts: acc, Models: models[lane.ChannelZCode], Extra: extra,
	}}
}

// wallTraeRow covers the 字节 browser-callback channel.
func (s *Server) wallTraeRow(models map[string]int) []channelWallRow {
	accts := s.Lane.TraeAccounts()
	acc := wallAccounts{Total: len(accts)}
	extra := ""
	for _, a := range accts {
		if a.Enabled && strings.TrimSpace(a.AccessToken) != "" {
			acc.Enabled++
		}
		if extra == "" {
			extra = wallFirstNonEmpty(a.Nickname, a.UID, a.ID)
		}
	}
	return []channelWallRow{{
		ID: lane.ChannelTrae, Label: "TRAE (字节)", Blurb: channelBlurb(lane.ChannelTrae),
		Login: wallLoginBrowser, Enabled: s.Store.ChannelEnabled(lane.ChannelTrae),
		Accounts: acc, Models: models[lane.ChannelTrae], Extra: extra,
	}}
}

// wallExtRows covers the six preset channels, one row each, in display order.
func (s *Server) wallExtRows(models map[string]int) []channelWallRow {
	kinds := lane.ExtKinds()
	out := make([]channelWallRow, 0, len(kinds))
	for _, kind := range kinds {
		accts := s.Lane.ExtAccounts(kind)
		acc := wallAccounts{Total: len(accts)}
		extra := ""
		for _, a := range accts {
			if a.Enabled && strings.TrimSpace(a.Secret) != "" {
				acc.Enabled++
			}
			if extra == "" {
				extra = a.Label
			}
		}
		out = append(out, channelWallRow{
			ID: kind, Label: lane.ExtPresetLabel(kind), Blurb: channelBlurb(kind),
			Login: wallLoginPaste, Enabled: s.Store.ChannelEnabled(kind),
			Accounts: acc, Models: models[kind], Extra: extra,
		})
	}
	return out
}

// channelLabel is the display name of the two keyless lanes. The preset
// channels name themselves through lane.ExtPresetLabel, so they are not listed
// here — a second copy of a vendor's name is a second place to rename it.
func channelLabel(id string) string {
	switch id {
	case lane.ChannelZen:
		return "OpenCode Zen 车道"
	case lane.ChannelKilo:
		return "Kilo 免费池"
	}
	return id
}

// channelBlurb is the one-line "what this channel is and what it costs you"
// that the card shows. Written once, in the backend, so no two surfaces can
// disagree about a channel's nature.
func channelBlurb(id string) string {
	switch id {
	case lane.ChannelZen:
		return "免登录免费池，按出口 IP 限额，不需要任何 Key"
	case lane.ChannelKilo:
		return "免凭证渠道，模型在名单里即可用"
	case lane.ChannelWorkBuddy:
		return "CodeBuddy 账号，浏览器授权后出模型，可加多个号"
	case lane.ChannelCodeArts:
		return "华为云账号，含轮换福利模型，每日自动签到"
	case lane.ChannelQoder:
		return "Qoder 侧车托管，可加多个号，按额度轮换"
	case lane.ChannelZCode:
		return "智谱账号，浏览器授权后模型进目录"
	case lane.ChannelTrae:
		return "字节 TRAE 账号，浏览器授权后模型进目录"
	}
	if p, ok := lane.ExtPreset(id); ok {
		return p.Blurb
	}
	return ""
}

func wallFirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
