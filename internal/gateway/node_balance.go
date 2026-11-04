package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"zen-gate/internal/store"
)

const nodeBalanceTimeout = 8 * time.Second

// nodeBalanceProbe builds a READ-ONLY balance reader from a node list
// declaration. It must never POST, never claim a daily reward, and never
// write anything back. This is the "接更多网站 / 更多算力" half of the check-in
// registry: a supplier that publishes a balance endpoint becomes watchable
// the moment its node list line is imported, with no Go adapter.
func nodeBalanceProbe(p store.Provider) func(context.Context) (float64, string, error) {
	return func(ctx context.Context) (float64, string, error) {
		kind := strings.TrimSpace(p.BalanceKind)
		if kind == "" {
			kind = "余额"
		}
		target, err := store.BalanceReadURL(p.BaseURL, p.BalanceURL)
		if err != nil {
			return 0, kind, err
		}
		if strings.TrimSpace(p.BalancePath) == "" {
			return 0, kind, fmt.Errorf("未声明 balancePath，不猜字段名")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return 0, kind, err
		}
		req.Header.Set("Accept", "application/json")
		// A node list is often pasted from somewhere public. Only send a
		// credential when the balance endpoint is on the SAME origin as the
		// API base URL — a declared third-party URL must never receive the
		// user's key. Same-origin absolute and relative URLs both qualify.
		if sameBalanceOrigin(p.BaseURL, target) {
			if keys := p.EffectiveKeys(); len(keys) > 0 && strings.TrimSpace(keys[0]) != "" {
				req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(keys[0]))
			}
		}
		client := &http.Client{Timeout: nodeBalanceTimeout}
		resp, err := client.Do(req)
		if err != nil {
			// Do not echo the target: a hand-written URL might carry a secret
			// in its query string, and errors end up in the admin log.
			return 0, kind, fmt.Errorf("余额读取失败: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return 0, kind, fmt.Errorf("余额端点返回 HTTP %d", resp.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return 0, kind, err
		}
		v, err := store.ReadBalanceValue(body, p.BalancePath)
		if err != nil {
			return 0, kind, err
		}
		return v, kind, nil
	}
}

func sameBalanceOrigin(baseURL, target string) bool {
	origin := func(raw string) string {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Host == "" {
			return ""
		}
		return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
	}
	a := origin(baseURL)
	return a != "" && a == origin(target)
}
