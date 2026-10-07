package subs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"zen-gate/internal/store"
)

// PortBase is the first local socks inbound port; node i listens on
// PortBase+i. The range sits well above ephemeral ports and below the
// dynamic RPC block on Windows.
const PortBase = 21001

// MaxNodes caps one rotation pool — enough nodes for any sane subscription,
// few enough to keep the port range and the probe loop bounded.
const MaxNodes = 64

// BuildConfig renders the sing-box config for nodes: one socks inbound per
// node, one outbound per node, and route rules pinning each inbound to its
// outbound so dialing 127.0.0.1:PortBase+i always exits through node i.
func BuildConfig(nodes []Node) ([]byte, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("没有节点")
	}
	if len(nodes) > MaxNodes {
		nodes = nodes[:MaxNodes]
	}
	inbounds := []any{}
	outbounds := []any{}
	rules := []any{}
	for i, n := range nodes {
		tag := outboundTag(i)
		ob := n.Outbound
		if ob == nil {
			return nil, fmt.Errorf("节点 %s 缺少 outbound", n.ID)
		}
		ob["tag"] = tag
		inbounds = append(inbounds, map[string]any{
			"type":        "socks",
			"tag":         inboundTag(i),
			"listen":      "127.0.0.1",
			"listen_port": PortBase + i,
		})
		outbounds = append(outbounds, ob)
		rules = append(rules, map[string]any{
			"inbound":  []string{inboundTag(i)},
			"outbound": tag,
		})
	}
	cfg := map[string]any{
		"log":       map[string]any{"level": "warn", "timestamp": true},
		"inbounds":  inbounds,
		"outbounds": outbounds,
		"route":     map[string]any{"rules": rules},
	}
	return json.MarshalIndent(cfg, "", "  ")
}

func inboundTag(i int) string  { return fmt.Sprintf("in-%02d", i) }
func outboundTag(i int) string { return fmt.Sprintf("out-%02d", i) }

// WriteConfig atomically writes the generated config under
// <zen-gate home>/subs/sing-box.json and returns its path.
func WriteConfig(nodes []Node) (string, error) {
	data, err := BuildConfig(nodes)
	if err != nil {
		return "", err
	}
	dir := store.SubsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "sing-box.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}
