// Package subs turns proxy subscription URLs into a sing-box sidecar config
// and rotates the lane's egress across the resulting local socks inbounds.
//
// One subscription body is a (usually base64-encoded) list of node URIs —
// vless://, vmess://, ss://, trojan://, hy2://, tuic://, socks://. Each parsed
// node becomes one sing-box outbound behind one local inbound, so zen-gate can
// pick a different exit IP per request without talking any of those protocols
// itself (the sing-box binary stays a separate, unmodified program).
package subs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Node is one parsed proxy node: the sing-box outbound object is fully formed
// except for its tag, which the config builder assigns per inbound slot.
type Node struct {
	ID       string         // stable: "<proto>-<hash8(uri)>"
	Name     string         // display name (fragment or proto-host:port)
	Proto    string         // vmess | vless | trojan | ss | hy2 | tuic | socks
	Outbound map[string]any `json:"-"`
	Raw      string         // original URI (dedup + diagnostics)
}

// ParseBody decodes one subscription body (base64 or plain line list) into
// nodes. Unsupported or malformed lines are skipped; the returned error only
// fires when nothing at all could be parsed.
func ParseBody(body string) ([]Node, error) {
	lines := splitLines(body)
	if len(lines) == 0 {
		return nil, fmt.Errorf("订阅内容为空")
	}
	nodes := []Node{}
	for _, line := range lines {
		n, err := ParseURI(line)
		if err != nil {
			continue // unsupported scheme or malformed node — skip quietly
		}
		nodes = append(nodes, n)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("订阅中没有可识别的节点")
	}
	return dedupe(nodes), nil
}

// FetchSubscription downloads one subscription body via the given client
// (callers pass lane.Client() so the fetch follows the current proxy setting)
// and parses it into nodes.
func FetchSubscription(ctx context.Context, client HTTPDoer, rawURL string) ([]Node, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSpace(rawURL), nil)
	if err != nil {
		return nil, fmt.Errorf("订阅 URL 无效: %w", err)
	}
	req.Header.Set("user-agent", "clash-verge/1.6.6") // some panels serve different bodies per UA
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取订阅失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("拉取订阅失败: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("读取订阅失败: %w", err)
	}
	return ParseBody(string(data))
}

// splitLines tries base64 first (the common subscription encoding, in every
// padding/charset variant), falling back to a plain URI-per-line list.
func splitLines(body string) []string {
	body = strings.TrimSpace(body)
	if !strings.Contains(body, "://") {
		if dec, ok := tryBase64(body); ok {
			body = dec
		}
	}
	out := []string{}
	for _, line := range strings.FieldsFunc(body, func(r rune) bool { return r == '\n' || r == '\r' }) {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func tryBase64(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.Contains(s, "://") {
		return "", false
	}
	normalize := func(enc *base64.Encoding) string {
		return strings.NewReplacer("+", "-", "/", "_").Replace(strings.TrimRight(s, "="))
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		var candidate string
		switch enc {
		case base64.URLEncoding, base64.RawURLEncoding:
			candidate = normalize(enc)
			if enc == base64.URLEncoding && len(candidate)%4 != 0 {
				candidate += strings.Repeat("=", 4-len(candidate)%4)
			}
		default:
			candidate = s
		}
		if dec, err := enc.DecodeString(candidate); err == nil {
			if txt := strings.TrimSpace(string(dec)); strings.Contains(txt, "://") {
				return txt, true
			}
		}
	}
	return "", false
}

// --- URI → sing-box outbound -------------------------------------------------

// ParseURI converts one node URI into a Node with a ready sing-box outbound.
func ParseURI(raw string) (Node, error) {
	raw = strings.TrimSpace(raw)
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return Node{}, fmt.Errorf("不是节点 URI")
	}
	proto := strings.ToLower(scheme)
	switch proto {
	case "vmess":
		return parseVMESS(raw, rest)
	case "vless":
		return parseUserinfoURI(raw, proto)
	case "trojan":
		return parseUserinfoURI(raw, proto)
	case "anytls":
		return parseUserinfoURI(raw, proto)
	case "ss":
		return parseSS(raw)
	case "hy2", "hysteria2":
		return parseUserinfoURI(raw, "hy2")
	case "tuic":
		return parseUserinfoURI(raw, proto)
	case "socks", "socks5":
		return parseUserinfoURI(raw, "socks")
	default:
		return Node{}, fmt.Errorf("不支持的协议 %q", scheme)
	}
}

// parseUserinfoURI handles the URL-shaped schemes sharing
// `<proto>://userinfo@host:port?<query>#<name>`: vless, trojan, anytls, hy2,
// tuic, socks. Protocol-specific quirks are applied by the per-proto builders.
func parseUserinfoURI(raw, proto string) (Node, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Node{}, fmt.Errorf("URI 解析失败: %w", err)
	}
	host := u.Hostname()
	port, _ := strconv.Atoi(u.Port())
	if host == "" || port <= 0 {
		return Node{}, fmt.Errorf("缺少 host/port")
	}
	q := u.Query()
	ob := map[string]any{
		"tag":         "", // assigned by the config builder
		"server":      host,
		"server_port": port,
	}
	switch proto {
	case "vless":
		ob["type"] = "vless"
		ob["uuid"] = u.User.Username()
		if flow := q.Get("flow"); flow != "" {
			ob["flow"] = flow
		}
		ob["tls"] = tlsBlock(q, false)
		attachTransport(ob, q)
	case "trojan":
		ob["type"] = "trojan"
		if pw, ok := u.User.Password(); ok {
			ob["password"] = pw
		} else {
			ob["password"] = u.User.Username()
		}
		ob["tls"] = tlsBlock(q, true)
		attachTransport(ob, q)
	case "anytls":
		// Same userinfo shape as trojan, TLS always on. No transport object:
		// sing-box speaks AnyTLS over plain TCP only.
		ob["type"] = "anytls"
		if pw, ok := u.User.Password(); ok {
			ob["password"] = pw
		} else {
			ob["password"] = u.User.Username()
		}
		ob["tls"] = tlsBlock(q, true)
	case "hy2":
		ob["type"] = "hysteria2"
		if pw, ok := u.User.Password(); ok {
			ob["password"] = pw
		} else {
			ob["password"] = u.User.Username()
		}
		ob["tls"] = tlsBlock(q, true)
		if o := q.Get("obfs"); o != "" {
			ob["obfs"] = map[string]any{"type": o, "password": q.Get("obfs-password")}
		}
	case "tuic":
		ob["type"] = "tuic"
		ob["uuid"] = u.User.Username()
		ob["password"], _ = u.User.Password()
		if cc := q.Get("congestion_control"); cc != "" {
			ob["congestion_control"] = cc
		}
		ob["tls"] = tlsBlock(q, true)
		if alpn := q.Get("alpn"); alpn != "" {
			if tb, ok := ob["tls"].(map[string]any); ok {
				tb["alpn"] = strings.Split(alpn, ",")
			}
		}
	case "socks":
		ob["type"] = "socks"
		ob["version"] = "5"
		if usr := u.User.Username(); usr != "" {
			ob["username"] = usr
			if pw, ok := u.User.Password(); ok {
				ob["password"] = pw
			}
		}
	}
	name := fragmentName(u, proto, host, port)
	return Node{
		ID:       nodeID(raw),
		Name:     name,
		Proto:    proto,
		Outbound: ob,
		Raw:      raw,
	}, nil
}

// tlsBlock builds the sing-box TLS object from query params. defaultTLS is
// true for the always-TLS protocols (trojan/hy2/tuic) — security=none turns it
// off explicitly; for vless/vmess security=tls|reality turns it on.
func tlsBlock(q url.Values, defaultTLS bool) map[string]any {
	security := strings.ToLower(q.Get("security"))
	if security == "none" {
		return map[string]any{"enabled": false}
	}
	if !defaultTLS && security == "" {
		return map[string]any{"enabled": false}
	}
	tb := map[string]any{"enabled": true}
	if sni := q.Get("sni"); sni != "" {
		tb["server_name"] = sni
	}
	insecure := q.Get("allowInsecure")
	if insecure == "" {
		insecure = q.Get("insecure")
	}
	if insecure == "1" || strings.EqualFold(insecure, "true") {
		tb["insecure"] = true
	}
	if alpn := q.Get("alpn"); alpn != "" {
		tb["alpn"] = strings.Split(alpn, ",")
	}
	if fp := q.Get("fp"); fp != "" {
		tb["utls"] = map[string]any{"enabled": true, "fingerprint": fp}
	}
	if security == "reality" {
		tb["reality"] = map[string]any{
			"enabled":    true,
			"public_key": q.Get("pbk"),
			"short_id":   q.Get("sid"),
		}
	}
	return tb
}

// attachTransport fills ob["transport"] for ws/grpc/http carriers; plain tcp
// needs no transport object.
func attachTransport(ob map[string]any, q url.Values) {
	switch strings.ToLower(q.Get("type")) {
	case "ws":
		tr := map[string]any{"type": "ws"}
		if p := q.Get("path"); p != "" {
			tr["path"] = p
		}
		if h := q.Get("host"); h != "" {
			tr["headers"] = map[string]any{"Host": h}
		}
		ob["transport"] = tr
	case "grpc":
		tr := map[string]any{"type": "grpc"}
		if sn := q.Get("serviceName"); sn != "" {
			tr["service_name"] = sn
		}
		ob["transport"] = tr
	case "http", "h2":
		if strings.ToLower(q.Get("type")) == "http" && q.Get("headerType") == "http" {
			tr := map[string]any{"type": "http"}
			if h := q.Get("host"); h != "" {
				tr["host"] = strings.Split(h, ",")
			}
			if p := q.Get("path"); p != "" {
				tr["path"] = p
			}
			ob["transport"] = tr
		}
	}
}

// parseVMESS handles the v2rayN share format: vmess://base64(JSON).
func parseVMESS(raw, rest string) (Node, error) {
	payload, err := decodeAnyBase64(rest)
	if err != nil {
		return Node{}, fmt.Errorf("vmess base64 解码失败: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return Node{}, fmt.Errorf("vmess JSON 解析失败: %w", err)
	}
	get := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				s := strings.TrimSpace(fmt.Sprintf("%v", v))
				if s != "" && s != "0" {
					return s
				}
			}
		}
		return ""
	}
	host := get("add")
	port, _ := strconv.Atoi(get("port"))
	if host == "" || port <= 0 {
		return Node{}, fmt.Errorf("vmess 缺少 host/port")
	}
	uuid := get("id")
	if uuid == "" {
		return Node{}, fmt.Errorf("vmess 缺少 uuid")
	}
	ob := map[string]any{
		"tag":         "",
		"type":        "vmess",
		"server":      host,
		"server_port": port,
		"uuid":        uuid,
		"alter_id":    atoiOr(get("aid", "alterId"), 0),
		"security":    orDefault(get("scy", "security"), "auto"),
	}
	tlsOn := strings.EqualFold(get("tls"), "tls")
	sni := orDefault(get("sni"), get("host"))
	if tlsOn {
		tb := map[string]any{"enabled": true, "server_name": sni}
		if insecure := get("allowInsecure", "insecure"); insecure == "1" || strings.EqualFold(insecure, "true") {
			tb["insecure"] = true
		}
		if alpn := get("alpn"); alpn != "" {
			tb["alpn"] = strings.Split(alpn, ",")
		}
		ob["tls"] = tb
	}
	net := strings.ToLower(get("net"))
	q := url.Values{}
	q.Set("type", net)
	q.Set("path", get("path"))
	q.Set("host", get("host"))
	q.Set("serviceName", get("path"))
	if net == "grpc" {
		q.Set("serviceName", get("path"))
	}
	if net == "ws" || net == "grpc" || (net == "tcp" && strings.EqualFold(get("type"), "http")) {
		attachTransport(ob, q)
	}
	name := orDefault(get("ps", "remark"), fmt.Sprintf("vmess-%s:%d", host, port))
	return Node{
		ID:       nodeID(raw),
		Name:     name,
		Proto:    "vmess",
		Outbound: ob,
		Raw:      raw,
	}, nil
}

// parseSS handles shadowsocks in both SIP002 (`ss://b64(method:pass)@host:port#n`,
// `ss://method:pass@host:port#n`) and legacy (`ss://b64(method:pass@host:port)#n`)
// shapes.
func parseSS(raw string) (Node, error) {
	body := strings.TrimPrefix(raw, "ss://")
	frag := ""
	if i := strings.Index(body, "#"); i >= 0 {
		frag = unescape(body[i+1:])
		body = body[:i]
	}
	// Legacy: the whole thing before any @ is one base64 blob.
	if !strings.Contains(body, "@") {
		dec, err := decodeAnyBase64(body)
		if err != nil {
			return Node{}, fmt.Errorf("ss legacy base64 解码失败: %w", err)
		}
		body = string(dec)
	}
	userinfo, hostport, ok := strings.Cut(body, "@")
	if !ok {
		return Node{}, fmt.Errorf("ss 缺少 @")
	}
	// SIP002 userinfo is websafe base64 of "method:password"; some generators
	// emit it plain.
	method, password, ok := splitUserPass(userinfo)
	if !ok {
		dec, err := decodeAnyBase64(userinfo)
		if err != nil {
			return Node{}, fmt.Errorf("ss 凭据解码失败: %w", err)
		}
		method, password, ok = splitUserPass(string(dec))
		if !ok {
			return Node{}, fmt.Errorf("ss 凭据缺少 method:password")
		}
	}
	host, portStr, err := splitHostPort(hostport)
	if err != nil {
		return Node{}, err
	}
	port, _ := strconv.Atoi(portStr)
	ob := map[string]any{
		"tag":         "",
		"type":        "shadowsocks",
		"server":      host,
		"server_port": port,
		"method":      method,
		"password":    password,
	}
	name := orDefault(frag, fmt.Sprintf("ss-%s:%d", host, port))
	return Node{ID: nodeID(raw), Name: name, Proto: "ss", Outbound: ob, Raw: raw}, nil
}

// --- small helpers -----------------------------------------------------------

func nodeID(raw string) string {
	return "n-" + shortHash(raw)
}

func fragmentName(u *url.URL, proto, host string, port int) string {
	if u.Fragment != "" {
		return u.Fragment
	}
	if u.Opaque == "" && u.RawFragment != "" {
		if n := unescape(u.RawFragment); n != "" {
			return n
		}
	}
	return fmt.Sprintf("%s-%s:%d", proto, host, port)
}

func unescape(s string) string {
	if v, err := url.QueryUnescape(s); err == nil {
		return v
	}
	return s
}

func splitUserPass(s string) (string, string, bool) {
	method, pass, ok := strings.Cut(s, ":")
	if !ok || method == "" || pass == "" {
		return "", "", false
	}
	return method, pass, true
}

func dedupe(nodes []Node) []Node {
	seen := map[string]bool{}
	out := []Node{}
	for _, n := range nodes {
		if seen[n.ID] {
			continue
		}
		seen[n.ID] = true
		out = append(out, n)
	}
	return out
}
