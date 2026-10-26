package gateway

import (
	"net/http"
	"testing"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// qoderSeedProvider puts a logged-in-looking Qoder provider in the store, the
// state a previous successful probe would have left behind.
func qoderSeedProvider(t *testing.T) *Server {
	t.Helper()
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	st.Mutate(func(c *store.Config) {
		c.Providers = append(c.Providers, store.Provider{
			ID: qoderProviderID, Name: "Qoder 账号渠道",
			BaseURL: "http://127.0.0.1:8095/v1", Protocol: store.ProtocolOpenAI,
			Enabled: true, Models: []string{"DeepSeek-Flash", "Qwen3.8-Flash"},
		})
	})
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	return New(lane.NewLane(), st)
}

func qoderEnabled(t *testing.T, s *Server) bool {
	t.Helper()
	for _, p := range s.Store.Config().Providers {
		if p.ID == qoderProviderID {
			return p.Enabled
		}
	}
	return false
}

// A probe that could not ask must not rewrite what is stored. The sidecar is a
// loopback process that can be mid-restart; treating one refused connection as
// "not logged in" disabled the provider, and the card then read 已关闭 on the
// next 10-second repaint until some later probe happened to succeed.
func TestQoderProbeFailureKeepsTheProviderEnabled(t *testing.T) {
	s := qoderSeedProvider(t)

	// No sidecar anywhere: qoderGet fails to connect.
	withQoderStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	forceQoderProbe(t)
	st := probeQoderCached(true)
	if st.Definitive {
		t.Fatal("a probe that never reached the sidecar claimed to be definitive")
	}

	if wired := s.ensureQoderProvider(st); !wired {
		t.Error("providerWired = false, want the stored enabled state preserved")
	}
	if !qoderEnabled(t, s) {
		t.Error("an unanswerable probe disabled the managed provider")
	}
}

// The mirror case: an answer that the login is gone IS definitive, and the
// provider must drop out of every picker.
func TestQoderExpiredTokenDisablesTheProvider(t *testing.T) {
	s := qoderSeedProvider(t)

	withQoderStub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/models":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"TOKEN_EXPIRE"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	forceQoderProbe(t)
	st := probeQoderCached(true)
	if !st.Definitive {
		t.Fatal("a 401 from the sidecar is an answer about the credential, want definitive")
	}
	if st.LoggedIn {
		t.Error("loggedIn = true after a 401")
	}

	if wired := s.ensureQoderProvider(st); wired {
		t.Error("providerWired = true while the login is expired")
	}
	if qoderEnabled(t, s) {
		t.Error("an expired login left the managed provider enabled")
	}
}

// A successful listing is definitive and re-enables a provider a previous
// expired token had turned off — the IDE refresh path.
func TestQoderLoggedInReenablesTheProvider(t *testing.T) {
	s := qoderSeedProvider(t)
	s.Store.Mutate(func(c *store.Config) {
		for i := range c.Providers {
			if c.Providers[i].ID == qoderProviderID {
				c.Providers[i].Enabled = false
			}
		}
	})

	withQoderStub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"DeepSeek-Flash"},{"id":"Qwen3.8-Flash"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	forceQoderProbe(t)
	st := probeQoderCached(true)
	if !st.Definitive || !st.LoggedIn {
		t.Fatalf("stub listed models but the probe said definitive=%v loggedIn=%v", st.Definitive, st.LoggedIn)
	}

	if wired := s.ensureQoderProvider(st); !wired {
		t.Error("providerWired = false after a successful listing")
	}
	if !qoderEnabled(t, s) {
		t.Error("a logged-in sidecar left the managed provider disabled")
	}
}

// An empty roster is still an answer: the channel is reachable but has nothing
// to serve, so the provider stops advertising models. This one is logged-in but
// useless, and the card explains it rather than showing an empty picker.
func TestQoderEmptyRosterIsDefinitiveAndDisables(t *testing.T) {
	s := qoderSeedProvider(t)

	withQoderStub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	forceQoderProbe(t)
	st := probeQoderCached(true)
	if !st.Definitive {
		t.Error("an empty listing is the sidecar answering, want definitive")
	}
	if wired := s.ensureQoderProvider(st); wired {
		t.Error("providerWired = true with no models to serve")
	}
	if qoderEnabled(t, s) {
		t.Error("an empty roster left the provider enabled")
	}
}

// A /v1/models transport failure says nothing about the credential — the
// sidecar is up and answering /health, but the listing call timed out.
func TestQoderModelsTimeoutIsNotDefinitive(t *testing.T) {
	s := qoderSeedProvider(t)

	withQoderStub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/models":
			// A handler that never answers exhausts qoderProbeSlow.
			<-r.Context().Done()
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	forceQoderProbe(t)
	st := probeQoderCached(true)
	if st.Definitive {
		t.Error("a listing timeout claimed to be definitive")
	}
	if !qoderEnabled(t, s) || !s.ensureQoderProvider(st) {
		t.Error("a listing timeout disabled the managed provider")
	}
}
