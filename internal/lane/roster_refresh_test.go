package lane

import (
	"strings"
	"testing"
)

// The 2026-10-15 upstream refresh added five free-lane models and retired four.
// These pin the roster itself — a model the table forgot is invisible to every
// picker, and a retired id in the cold-start fallback sends the first request
// of a fresh install to a model that no longer exists.
func TestFreeRosterCoversLiveListing(t *testing.T) {
	// The live listing as of 2026-10-15, filtered by IsFreeLane.
	live := []string{
		"beam-free", "deepseek-v4.1-flash-free", "glm-5.3-flash-free", "jev-1.13-free",
		"ll-alpha-free", "longcat-2.5-preview-free", "mimo-v2.6-flash-free",
		"muse-spark-1.2-contributor-free", "muse-spark-1.3-contributor-free",
		"nemotron-3.5-lightning-free", "nemotron-3.5-super-vl-free", "space-bunny-free",
	}
	cat := BuildCatalog(live)
	if len(cat) != len(live) {
		t.Fatalf("catalog kept %d of %d live free models: %+v", len(cat), len(live), cat)
	}
	for _, m := range cat {
		if m.Name == m.ID {
			t.Errorf("%s has no display name", m.ID)
		}
		if m.ContextWindow <= 0 || m.MaxOutput <= 0 {
			t.Errorf("%s has no measured capacities: ctx=%d out=%d", m.ID, m.ContextWindow, m.MaxOutput)
		}
		if m.Blurb == "" {
			t.Errorf("%s has no blurb", m.ID)
		}
	}
}

// The fallback list is what a no-network cold start serves; a stale id there is
// a 404 on the user's very first request.
func TestFallbackCatalogHasNoRetiredModels(t *testing.T) {
	retired := []string{"mimo-v2.5-free", "ling-3.0-flash-fin-free", "nemotron-3-ultra-free", "deepseek-v4-flash-free"}
	for _, id := range FallbackCatalogIDs {
		for _, gone := range retired {
			if id == gone {
				t.Errorf("fallback roster still lists retired model %s", id)
			}
		}
	}
	for _, want := range []string{"deepseek-v4.1-flash-free", "ll-alpha-free", "nemotron-3.5-super-vl-free", "glm-5.3-flash-free", "beam-free"} {
		found := false
		for _, id := range FallbackCatalogIDs {
			if id == want {
				found = true
			}
		}
		if !found {
			t.Errorf("fallback roster is missing new model %s", want)
		}
	}
}

// The 2026-10-15 measurements: each row's capacities come from a live probe, and
// the specific rules must win over the family defaults they sit next to
// (deepseek-v4.1 sees vision + a 256K output, nemotron super-vl leaves the
// 128K family default behind).
func TestMeasuredCapabilitiesWinOverFamilyDefaults(t *testing.T) {
	byID := map[string]ModelInfo{}
	for _, m := range BuildCatalog([]string{
		"deepseek-v4.1-flash-free", "nemotron-3.5-super-vl-free", "ll-alpha-free",
		"glm-5.3-flash-free", "beam-free", "nemotron-3.5-lightning-free",
	}) {
		byID[m.ID] = m
	}

	ds := byID["deepseek-v4.1-flash-free"]
	if !ds.Vision {
		t.Error("deepseek-v4.1-flash-free accepted a PNG in the probe — vision must be true")
	}
	if ds.MaxOutput != 262144 {
		t.Errorf("deepseek-v4.1 maxOutput = %d, want 262144 (256k was accepted)", ds.MaxOutput)
	}
	if ds.ContextWindow != 1048576 {
		t.Errorf("deepseek-v4.1 context = %d, want 1048576", ds.ContextWindow)
	}

	nv := byID["nemotron-3.5-super-vl-free"]
	if !nv.Vision || nv.MaxOutput != 262144 || nv.ContextWindow != 1048576 {
		t.Errorf("nemotron super-vl = ctx %d out %d vision %v, want 1048576/262144/true",
			nv.ContextWindow, nv.MaxOutput, nv.Vision)
	}
	// The family default must still apply to the non-VL sibling.
	light := byID["nemotron-3.5-lightning-free"]
	if light.ContextWindow != 128000 || light.MaxOutput != 32768 {
		t.Errorf("nemotron lightning kept its family row: ctx %d out %d", light.ContextWindow, light.MaxOutput)
	}

	ll := byID["ll-alpha-free"]
	if !ll.Vision || ll.MaxOutput != 131072 {
		t.Errorf("ll-alpha = out %d vision %v, want 131072/true", ll.MaxOutput, ll.Vision)
	}

	glm := byID["glm-5.3-flash-free"]
	if glm.MaxOutput != 262144 {
		t.Errorf("glm maxOutput = %d, want 262144", glm.MaxOutput)
	}
	// Vision stayed unproven: the probe only ever drew transient 429s, and an
	// unverified modality must not receive image traffic.
	if glm.Vision {
		t.Error("glm vision was never proven — it must stay false")
	}

	// beam-free answers RegionError from a CN egress, so it must be marked
	// region-sensitive or routing wastes turns on it.
	if !byID["beam-free"].RegionSensitive {
		t.Error("beam-free refuses CN egress with RegionError — must be regionSensitive")
	}
}

// Capabilities are looked up by first match, so a family row placed above its
// own sibling would shadow it. This pins the ordering the table depends on.
func TestSpecificRowsPrecedeTheirFamilyRows(t *testing.T) {
	order := map[string]int{}
	for i, c := range capabilities {
		order[c.match.String()] = i
	}
	checks := []struct{ specific, family string }{
		{`^nemotron.*super.?vl`, `^nemotron`},
		{`^deepseek.*v4\.1`, `^deepseek`},
		{`^mimo.*v2\.6`, `^mimo`},
	}
	for _, c := range checks {
		if order[c.specific] > order[c.family] {
			t.Errorf("%s must precede %s or it is dead code", c.specific, c.family)
		}
	}
}

func TestNewModelNamesAreHuman(t *testing.T) {
	for id, want := range map[string]string{
		"deepseek-v4.1-flash-free":   "DeepSeek V4.1 Flash",
		"nemotron-3.5-super-vl-free": "Nemotron 3.5 Super VL",
		"ll-alpha-free":              "LL Alpha",
		"glm-5.3-flash-free":         "GLM 5.3 Flash",
	} {
		if got := DisplayModelName(id); got != want {
			t.Errorf("DisplayModelName(%q) = %q, want %q", id, got, want)
		}
		if b := blurbs[id]; !strings.Contains(b, "实测") && id == "deepseek-v4.1-flash-free" {
			t.Errorf("blurb for %s should state the measurement", id)
		}
	}
}