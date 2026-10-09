package qoderhub

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"zen-gate/internal/qoderhub/httpapi"
	"zen-gate/internal/qoderhub/service"
)

// The embedded QoderCN gateway: the vendored qodercn-gateway (MIT, by
// Tiancheng Lu) runs inside zen-gate's process on 127.0.0.1:8095 and serves
// the OpenAI/Anthropic surface the Qoder 账号渠道 provider points at. It
// harvests the QoderCN IDE's cached login, so "logged in" follows the IDE.
//
// Start is non-fatal by design: when the port is already taken (an external
// qodercn-gateway the user runs themselves, or a second zen-gate), the hub
// stays down and the status probe simply reports whatever answers on the port.

// HubHost/HubPort is the fixed loopback bind the managed provider points at.
const (
	HubHost = "127.0.0.1"
	HubPort = 8095
)

// BaseURL is what the managed provider's BaseURL is built from.
const BaseURL = "http://127.0.0.1:8095/v1"

// Hub is one embedded gateway instance.
type Hub struct {
	svc    *service.Service
	server *httpapi.Server
	addr   string
}

// Start brings the hub up. Warmup happens asynchronously — a slow or absent
// IDE login must never delay zen-gate's own startup.
func Start() (*Hub, error) {
	addr := net.JoinHostPort(HubHost, fmt.Sprintf("%d", HubPort))
	// Fail fast on a busy port so the caller can fall back to whatever
	// already answers there instead of half-starting.
	if ln, err := net.Listen("tcp", addr); err != nil {
		return nil, fmt.Errorf("port %d occupied: %w", HubPort, err)
	} else {
		_ = ln.Close()
	}

	cfg := service.Config{
		Host:        HubHost,
		Port:        HubPort,
		Model:       "kmodel",
		SessionMode: "auto",
	}
	svc := service.New(cfg)
	h := &Hub{svc: svc, addr: addr}
	h.server = httpapi.NewServer(addr, svc)

	go func() {
		warmCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := svc.Warmup(warmCtx); err != nil {
			// Not fatal: the credential may arrive later (IDE login) and the
			// client re-reads the cache per request.
			_ = err
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		if err := h.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	select {
	case err := <-errCh:
		return nil, err
	case <-time.After(500 * time.Millisecond):
	}
	return h, nil
}

// Stop drains the hub.
func (h *Hub) Stop() {
	if h == nil || h.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = h.server.Shutdown(ctx)
	_ = h.svc.Close()
}
