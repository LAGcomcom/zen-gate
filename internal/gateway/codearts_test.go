package gateway

import (
	"encoding/json"
	"testing"

	"zen-gate/internal/lane"
)

// The CodeArts channel follows the Qoder channel's contract: the managed
// provider exists only while the embedded hub can actually serve a turn. A
// static fallback roster is NOT a login — the vendored server publishes one
// before any account is loaded, and pointing a provider at it would hand every
// picker a model that answers 401.
func TestCodeartsProviderFollowsTheAccountState(t *testing.T) {
	s := newTestServer(t, laneUpstreamByModel(t, nil))

	// No account: no provider at all.
	if wired := s.ensureCodeartsProvider(codeartsStatus{Running: true, LoggedIn: false}); wired {
		t.Fatal("no account must not create the provider")
	}
	for _, p := range s.Store.Config().Providers {
		if p.ID == codeartsProviderID {
			t.Fatal("a provider was created without a login")
		}
	}

	// Logged in: created, enabled, pointed at the embedded hub, models wired.
	st := codeartsStatus{
		Running: true, LoggedIn: true, Accounts: 1,
		ModelIDs: []string{"GLM-5.2", "deepseek-v4-pro-0813"},
		Catalog: []lane.ListingMeta{
			{ID: "GLM-5.2", ContextWindow: 200000, InputDeclared: true, OutputDeclared: true},
		},
	}
	if wired := s.ensureCodeartsProvider(st); !wired {
		t.Fatal("a logged-in hub must wire the provider")
	}
	var got *struct{ enabled bool; models int; base string }
	for _, p := range s.Store.Config().Providers {
		if p.ID == codeartsProviderID {
			got = &struct{ enabled bool; models int; base string }{p.Enabled, len(p.Models), p.BaseURL}
		}
	}
	if got == nil || !got.enabled || got.models != 2 {
		t.Fatalf("provider = %+v, want enabled with 2 models", got)
	}
	if got.base != "http://127.0.0.1:8096/v1" {
		t.Fatalf("base url = %q, want the embedded hub", got.base)
	}

	// Login lost: the provider is disabled, its models leave the pickers, and
	// the roster survives so a later re-login restores it unchanged.
	if wired := s.ensureCodeartsProvider(codeartsStatus{Running: true, LoggedIn: false}); wired {
		t.Fatal("a lost login must disable the provider")
	}
	for _, p := range s.Store.Config().Providers {
		if p.ID == codeartsProviderID {
			if p.Enabled {
				t.Fatal("provider stayed enabled after the login was lost")
			}
			if len(p.Models) != 2 {
				t.Fatalf("roster = %d models, want it preserved for the re-login", len(p.Models))
			}
		}
	}
}

// parseCodeartsCatalog must mark rows it describes as declared, otherwise the
// capability chain discards the context window the upstream just published.
func TestParseCodeartsCatalogKeepsDeclaredContext(t *testing.T) {
	body := []byte(`{"data":[{"id":"GLM-5.2","context_window":200000}]}`)
	rows := parseCodeartsCatalog(body)
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	if rows[0].ContextWindow != 200000 {
		t.Fatalf("context = %d, want 200000", rows[0].ContextWindow)
	}
	if !rows[0].InputDeclared || !rows[0].OutputDeclared {
		t.Fatal("a row the upstream describes must be marked declared")
	}
	_, _ = json.Marshal(rows) // the shape must stay serialisable for the store
}
