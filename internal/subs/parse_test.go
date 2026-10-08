package subs

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

const testUUID = "b831381d-6324-4d53-ad4f-8cda48b30811"

func TestParseVLESSWSTLS(t *testing.T) {
	n, err := ParseURI("vless://" + testUUID + "@example.com:443?encryption=none&security=tls&sni=cdn.example.com&fp=chrome&type=ws&host=cdn.example.com&path=%2Fws#JP%E8%8A%82%E7%82%B9")
	if err != nil {
		t.Fatal(err)
	}
	if n.Proto != "vless" || n.Name != "JP节点" {
		t.Fatalf("proto/name = %q/%q", n.Proto, n.Name)
	}
	ob := n.Outbound
	if ob["type"] != "vless" || ob["server"] != "example.com" || ob["uuid"] != testUUID {
		t.Fatalf("outbound 基础字段错误: %v", ob)
	}
	if ob["server_port"] != 443 {
		t.Fatalf("port = %v", ob["server_port"])
	}
	tls := ob["tls"].(map[string]any)
	if tls["enabled"] != true || tls["server_name"] != "cdn.example.com" {
		t.Fatalf("tls 错误: %v", tls)
	}
	if ut := tls["utls"].(map[string]any); ut["fingerprint"] != "chrome" {
		t.Fatalf("utls 错误: %v", ut)
	}
	tr := ob["transport"].(map[string]any)
	if tr["type"] != "ws" || tr["path"] != "/ws" {
		t.Fatalf("transport 错误: %v", tr)
	}
}

func TestParseVLESSReality(t *testing.T) {
	n, err := ParseURI("vless://" + testUUID + "@r.example.com:8443?security=reality&sni=www.apple.com&fp=chrome&pbk=PUBKEY123&sid=abcd&type=tcp#R")
	if err != nil {
		t.Fatal(err)
	}
	tls := n.Outbound["tls"].(map[string]any)
	re := tls["reality"].(map[string]any)
	if re["enabled"] != true || re["public_key"] != "PUBKEY123" || re["short_id"] != "abcd" {
		t.Fatalf("reality 错误: %v", re)
	}
}

func TestParseVMESS(t *testing.T) {
	raw := map[string]any{
		"v": "2", "ps": "VM-01", "add": "1.2.3.4", "port": "8443",
		"id": testUUID, "aid": "0", "scy": "auto", "net": "ws",
		"host": "cdn.example.com", "path": "/vm", "tls": "tls", "sni": "cdn.example.com",
	}
	j, _ := json.Marshal(raw)
	n, err := ParseURI("vmess://" + base64.StdEncoding.EncodeToString(j))
	if err != nil {
		t.Fatal(err)
	}
	if n.Proto != "vmess" || n.Name != "VM-01" {
		t.Fatalf("proto/name = %q/%q", n.Proto, n.Name)
	}
	ob := n.Outbound
	if ob["type"] != "vmess" || ob["server"] != "1.2.3.4" || ob["server_port"] != 8443 || ob["security"] != "auto" {
		t.Fatalf("outbound 错误: %v", ob)
	}
	if tls := ob["tls"].(map[string]any); tls["enabled"] != true {
		t.Fatalf("tls 未启用: %v", ob)
	}
	if tr := ob["transport"].(map[string]any); tr["type"] != "ws" || tr["path"] != "/vm" {
		t.Fatalf("transport 错误: %v", tr)
	}
}

func TestParseTrojanSSHy2Tuic(t *testing.T) {
	n, err := ParseURI("trojan://Password123@jp.example.com:443?sni=jp.example.com&type=tcp#TR-JP")
	if err != nil {
		t.Fatal(err)
	}
	if n.Outbound["type"] != "trojan" || n.Outbound["password"] != "Password123" {
		t.Fatalf("trojan 错误: %v", n.Outbound)
	}
	if tls := n.Outbound["tls"].(map[string]any); tls["server_name"] != "jp.example.com" {
		t.Fatalf("trojan tls 错误: %v", tls)
	}

	n, err = ParseURI("ss://YWVzLTI1Ni1nY206cGFzc3dvcmQ=@hk.example.com:8388#HK-01")
	if err != nil {
		t.Fatal(err)
	}
	if n.Outbound["type"] != "shadowsocks" || n.Outbound["method"] != "aes-256-gcm" || n.Outbound["password"] != "password" {
		t.Fatalf("ss 错误: %v", n.Outbound)
	}

	n, err = ParseURI("hy2://letmein@us.example.com:8443?sni=us.example.com&insecure=1&obfs=salamander&obfs-password=obfsPass#US-01")
	if err != nil {
		t.Fatal(err)
	}
	if n.Outbound["type"] != "hysteria2" || n.Outbound["password"] != "letmein" {
		t.Fatalf("hy2 错误: %v", n.Outbound)
	}
	if tls := n.Outbound["tls"].(map[string]any); tls["insecure"] != true {
		t.Fatalf("hy2 insecure 未生效: %v", tls)
	}
	if obf := n.Outbound["obfs"].(map[string]any); obf["password"] != "obfsPass" {
		t.Fatalf("hy2 obfs 错误: %v", obf)
	}

	n, err = ParseURI("tuic://" + testUUID + ":tuicpass@sg.example.com:8443?congestion_control=bbr&alpn=h3&sni=sg.example.com#SG-01")
	if err != nil {
		t.Fatal(err)
	}
	if n.Outbound["type"] != "tuic" || n.Outbound["congestion_control"] != "bbr" {
		t.Fatalf("tuic 错误: %v", n.Outbound)
	}
}

func TestParseBodySkipsUnknownAndBase64Decodes(t *testing.T) {
	vless := "vless://" + testUUID + "@a.example.com:443?type=tcp#A"
	trojan := "trojan://pw@b.example.com:443#B"
	body := base64.StdEncoding.EncodeToString([]byte(strings.Join([]string{vless, "foo://bar", "not a uri", trojan}, "\n")))
	nodes, err := ParseBody(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("应解析出 2 个节点，得到 %d", len(nodes))
	}
	if nodes[0].Proto != "vless" || nodes[1].Proto != "trojan" {
		t.Fatalf("顺序/协议错误: %s, %s", nodes[0].Proto, nodes[1].Proto)
	}
}

func TestParseBodyRejectsGarbage(t *testing.T) {
	if _, err := ParseBody("hello world"); err == nil {
		t.Fatal("无节点的正文应报错")
	}
	if _, err := ParseBody(""); err == nil {
		t.Fatal("空正文应报错")
	}
}
