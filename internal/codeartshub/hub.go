// Package codeartshub embeds the CodeArts2API gateway (MIT, by HITZY2002 —
// https://github.com/HITZY2002/codearts2api) inside zen-gate, the same way
// qoderhub embeds qodercn-gateway.
//
// CodeArts2API turns a Huawei Cloud CodeArts Agent account into an
// OpenAI-compatible endpoint: it owns the OAuth login, refreshes the token,
// rotates across accounts, and proxies /v1/chat/completions. zen-gate does not
// reimplement any of that — it runs the vendored server on a fixed loopback
// port and points a managed provider at it, so the models appear in every
// picker exactly like the Qoder channel's.
//
// The one thing the caller owns is where state lives: accounts, the pool state
// file and the conversation map all live under zen-gate's data directory, so
// an upgrade or a reinstall never loses a login.
package codeartshub

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"zen-gate/internal/codeartshub/auth"
	"zen-gate/internal/codeartshub/pool"
	"zen-gate/internal/codeartshub/scheduler"
	"zen-gate/internal/codeartshub/server"
	"zen-gate/internal/codeartshub/upstream"
)

// Fixed loopback bind the managed provider points at. A port of its own, not
// 7866 (the upstream default) or 8095 (Qoder): both channels must be able to
// run side by side.
const (
	HubHost = "127.0.0.1"
	HubPort = 8096
)

// BaseURL is what the managed provider's BaseURL is built from.
const BaseURL = "http://127.0.0.1:8096/v1"

// APIKey is the key the embedded server requires from its own provider. It
// never leaves the process: the provider is created by zen-gate with the same
// value, and the port is loopback-only.
const APIKey = "zen-gate-codearts-local"

// Hub is one embedded CodeArts gateway instance.
type Hub struct {
	handler *server.Handler
	server  *http.Server
	pool    *pool.Pool
	cancel  context.CancelFunc
	addr    string
}

// Start brings the hub up under home (zen-gate's data directory) and returns
// it. Non-fatal by design, like the Qoder hub: when the port is taken (an
// external codearts2api the user runs themselves, or a second zen-gate) this
// returns an error and the caller reports whatever already answers there.
func Start(home string) (*Hub, error) {
	addr := net.JoinHostPort(HubHost, fmt.Sprintf("%d", HubPort))
	if ln, err := net.Listen("tcp", addr); err != nil {
		return nil, fmt.Errorf("port %d occupied: %w", HubPort, err)
	} else {
		_ = ln.Close()
	}

	root := filepath.Join(home, "codearts")
	authDir := filepath.Join(root, "auths")
	stateFile := filepath.Join(root, "state.json")
	for _, d := range []string{authDir, filepath.Dir(stateFile)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", d, err)
		}
	}

	auths, err := auth.LoadDir(authDir)
	if err != nil {
		return nil, fmt.Errorf("load auths: %w", err)
	}
	p, err := pool.New(auths, pool.Config{
		MaxConcurrent:   5,
		SoftCooldown:    60 * time.Second,
		ErrThreshold:    3,
		ErrCooldown:     10 * time.Minute,
		RefreshSkew:     30 * time.Minute,
		KeepaliveWindow: 10 * time.Minute,
	}, stateFile)
	if err != nil {
		return nil, fmt.Errorf("build pool: %w", err)
	}

	up := upstream.New(120 * time.Second)
	up.SetBenefitAutoClaim(true)

	login := upstream.DefaultLoginConfig()
	for _, a := range auths {
		if id := a.ClientIDOr(""); id != "" {
			login.ClientID = id
			break
		}
	}

	h := server.NewHandler(server.Config{
		Pool:             p,
		Upstream:         up,
		APIKey:           APIKey,
		MaxRotate:        3,
		SoftCooldown:     60 * time.Second,
		ErrThreshold:     3,
		ErrCooldown:      10 * time.Minute,
		DefaultModel:     "snap-chat",
		ConvStateFile:    stateFile + ".chats.json",
		AuthDir:          authDir,
		Listen:           addr,
		OAuthClient:      upstream.New(15 * time.Second),
		LoginConfig:      login,
		QueueRetryDelay:  10 * time.Second,
		QueueMaxAttempts: 30,
		WatchInfo: map[string]any{
			"enabled":            true,
			"credits_auto_claim": true,
			"poll_minutes":       30,
			"max_concurrent":     5,
			"keepalive_window":   "10m",
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	sch := scheduler.New(scheduler.Config{
		Pool:              p,
		Enabled:           true,
		CreditsAutoClaim:  true,
		PollInterval:      30 * time.Minute,
		RefreshSkew:       30 * time.Minute,
		KeepaliveInterval: 15 * time.Minute,
	})
	go sch.Run(ctx)
	h.StartAvailabilityProber(ctx)

	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 30 * time.Second}
	hub := &Hub{handler: h, server: srv, pool: p, cancel: cancel, addr: addr}
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	select {
	case err := <-errCh:
		cancel()
		return nil, err
	case <-time.After(500 * time.Millisecond):
	}
	return hub, nil
}

// Stop drains the hub and its background scheduler.
func (h *Hub) Stop() {
	if h == nil {
		return
	}
	if h.cancel != nil {
		h.cancel()
	}
	if h.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.server.Shutdown(ctx)
	}
}
