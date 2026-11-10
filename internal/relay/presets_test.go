package relay

import (
	"net/url"
	"strings"
	"testing"

	"zen-gate/internal/store"
)

// TestPresetsAreWellFormed guards the one-click catalog: a duplicate id hides a
// row from the dashboard, a blank link strands the user on a dead chip, and a
// plain-http remote endpoint would ship the API key in the clear.
func TestPresetsAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range Presets {
		if p.ID == "" || p.Name == "" || p.BaseURL == "" || p.KeyURL == "" {
			t.Errorf("preset %q has a blank field: %+v", p.ID, p)
		}
		if seen[p.ID] {
			t.Errorf("duplicate preset id %q", p.ID)
		}
		seen[p.ID] = true
		if p.Protocol != store.ProtocolOpenAI && p.Protocol != store.ProtocolAnthropic {
			t.Errorf("preset %q has unknown protocol %q", p.ID, p.Protocol)
		}
		u, err := url.Parse(p.BaseURL)
		if err != nil || u.Host == "" {
			t.Errorf("preset %q has an unusable base URL %q", p.ID, p.BaseURL)
			continue
		}
		local := u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost"
		if u.Scheme != "https" && !local {
			t.Errorf("preset %q is plain http on a remote host: %q", p.ID, p.BaseURL)
		}
		if !strings.HasPrefix(p.KeyURL, "https://") {
			t.Errorf("preset %q key URL is not https: %q", p.ID, p.KeyURL)
		}
	}
}
