package gateway

import (
	"strings"
	"testing"
)

// The channels page is one card wall: a card exists for every channel and every
// custom provider, and clicking it opens a modal that shows exactly one
// .chan-pane. The two lists live in different places — the cards come from
// /admin/api/channel/wall at runtime, the panes are static HTML — so a channel
// can render a card that opens onto an empty modal. This test is that seam:
// every channel the backend can put on the wall needs a pane.
func TestChannelWallHasPaneForEveryCard(t *testing.T) {
	html := string(dashboardHTML)

	// The pane ids must cover every channel the backend serves. lane.Channels()
	// is the source of truth for "which channels exist"; the two extras are
	// "pv" (every custom provider, targets are "pv:<id>") and "ext" (the six
	// preset channels share one paste-a-token pane, see chanPaneFor).
	want := append(channelsNotInWall(), "pv", "ext")
	for _, id := range want {
		marker := `data-chan="` + id + `"`
		if !strings.Contains(html, marker) {
			t.Errorf("no .chan-pane carries %s — clicking that card opens an empty modal", marker)
		}
	}
}

// channelsNotInWall lists the channel ids that must have their own pane.
// Qoder is excluded: its card routes to the "qoder" pane, which exists, but the
// list is derived from lane.Channels() minus the preset kinds (they share
// "ext"), so this helper keeps the expectation written down in one place.
func channelsNotInWall() []string {
	return []string{
		"zen", "kilo", "workbuddy", "codearts", "qoder", "zcode", "trae",
	}
}

// The six preset channels share one pane: they all log in the same way (paste a
// token from the vendor's own app), so six near-identical panes would be six
// places to fix the same bug. Getting this wrong sends their cards to a pane
// named after the channel, which does not exist.
func TestChanPaneForRoutesPresetChannelsToSharedPane(t *testing.T) {
	// The routing table itself is extracted from the page, not restated here:
	// a test that hard-codes the preset list would pass while the page routed
	// them somewhere else.
	prelude := dashboardBlock(t, "const EXT_PANES")
	src := dashboardBlock(t, "function chanPaneFor(")
	tail := `
const cases = ["zen", "kilo", "workbuddy", "codearts", "qoder", "zcode", "trae",
  "cline", "loomy", "raccoon", "lobsterai", "astudio", "modelscope",
  "pv", "pv:my-provider"];
process.stdout.write(JSON.stringify(cases.map(c => [c, chanPaneFor(c)])));
`
	rows, ok := runBlockJS(t, src, prelude, tail).([]any)
	if !ok {
		t.Fatalf("harness output is not an array")
	}
	want := map[string]string{
		"zen": "zen", "kilo": "kilo", "workbuddy": "workbuddy",
		"codearts": "codearts", "qoder": "qoder", "zcode": "zcode", "trae": "trae",
		// Every preset channel lands on the shared paste pane.
		"cline": "ext", "loomy": "ext", "raccoon": "ext",
		"lobsterai": "ext", "astudio": "ext", "modelscope": "ext",
		"pv": "pv", "pv:my-provider": "pv",
	}
	for _, row := range rows {
		pair, _ := row.([]any)
		if len(pair) != 2 {
			t.Fatalf("bad row %v", row)
		}
		in, _ := pair[0].(string)
		got, _ := pair[1].(string)
		if want[in] != got {
			t.Errorf("chanPaneFor(%q) = %q, want %q", in, got, want[in])
		}
	}
}

// Every channel the wall can draw must have an icon, or its card falls back to a
// letter tile. The two keyless lanes, the four account channels and the six
// presets are all brand-recognisable; a letter tile there reads as "a channel
// with no identity", which is worse than no icon at all.
func TestChannelIconsAreDataURIs(t *testing.T) {
	src := dashboardBlock(t, "const CHAN_ICONS")
	if !strings.Contains(src, "data:image/") {
		t.Fatalf("CHAN_ICONS carries no data URI at all:\n%s", src)
	}
	for _, id := range []string{
		"zen", "kilo", "workbuddy", "codearts", "qoder", "zcode", "trae",
		"cline", "loomy", "raccoon", "lobsterai", "astudio", "modelscope",
	} {
		if !strings.Contains(src, id+":") {
			t.Errorf("CHAN_ICONS has no %q entry — that card falls back to a letter tile", id)
		}
	}
	// A data URI has no whitespace; a wrapped line would produce a broken src.
	for i, line := range strings.Split(src, "\n") {
		if strings.Contains(line, "data:image/") && strings.Contains(line, " ") {
			if !strings.HasSuffix(strings.TrimSpace(line), `",`) {
				t.Errorf("icon line %d looks truncated or wrapped: %.60s", i+1, line)
			}
		}
	}
}

// The dashboard must not keep a second copy of the channel list. It used to
// carry WALL_CHANNELS = [...], which meant adding a channel in the backend left
// its card missing until someone remembered to edit the HTML too.
func TestChannelWallHasNoSecondChannelList(t *testing.T) {
	html := string(dashboardHTML)
	if strings.Contains(html, "WALL_CHANNELS") {
		t.Error("the dashboard still hard-codes WALL_CHANNELS; cards must come from /admin/api/channel/wall")
	}
	if !strings.Contains(html, "/admin/api/channel/wall") {
		t.Error("the dashboard no longer reads /admin/api/channel/wall — the wall would have no cards")
	}
}
