package store

import (
	"encoding/json"
	"strings"
	"testing"
)

// "接更多网站" must stay a data change: the reset hour and the read-only
// balance endpoint are declarations in the node list, not a Go adapter.
func TestNodeBalanceContractIsData(t *testing.T) {
	text := `[{"name":"积分站","baseUrl":"https://credits.test/v1/","checkin":"daily",
	  "checkinResetHour":8,"balanceUrl":"/me/credits","balancePath":"data.credits",
	  "balanceKind":"积分余额","keys":["sk-must-not-leak"]}]`
	specs, problems := ParseNodeSpecs(text)
	if len(problems) != 0 || len(specs) != 1 {
		t.Fatalf("specs = %+v, problems = %+v", specs, problems)
	}
	s := specs[0]
	if s.CheckinResetHour != 8 || s.BalanceURL != "/me/credits" ||
		s.BalancePath != "data.credits" || s.BalanceKind != "积分余额" {
		t.Fatalf("balance contract lost on parse: %+v", s)
	}
	if len(s.Keys) != 1 || s.Keys[0] != "sk-must-not-leak" {
		t.Fatalf("import-only keys were dropped: %+v", s.Keys)
	}

	// Export carries the contract but never the credential, and never the
	// balance URL's query secrets either (a URL with userinfo is rejected).
	raw, err := json.Marshal(ExportNodeSpecs([]Provider{{
		ID: "credits", Name: "积分站", BaseURL: "https://credits.test/v1",
		APIKey: "sk-must-not-leak", Checkin: "daily", CheckinResetHour: 8,
		BalanceURL: "/me/credits", BalancePath: "data.credits", BalanceKind: "积分余额",
	}}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"checkinResetHour":8`, `"balanceUrl":"/me/credits"`, `"balancePath":"data.credits"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("export missing %s: %s", want, raw)
		}
	}
	if strings.Contains(string(raw), "sk-must-not-leak") {
		t.Fatalf("export leaked a key: %s", raw)
	}
}

// The store never guesses: a bad reset hour, an unlabelled endpoint, a URL
// with embedded credentials, or a path we cannot walk must be reported.
func TestNodeBalanceContractRefusesGuesses(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string
	}{
		{"reset hour out of range", `[{"baseUrl":"https://a.test/v1","checkinResetHour":24}]`, "checkinResetHour"},
		{"negative reset hour", `[{"baseUrl":"https://a.test/v1","checkinResetHour":-1}]`, "checkinResetHour"},
		{"url without a path", `[{"baseUrl":"https://a.test/v1","balanceUrl":"/credits"}]`, "balancePath"},
		{"userinfo in url", `[{"baseUrl":"https://a.test/v1","balanceUrl":"https://u:p@a.test/credits","balancePath":"credits"}]`, "内嵌账号密码"},
		{"bad scheme", `[{"baseUrl":"https://a.test/v1","balanceUrl":"file:///etc/passwd","balancePath":"credits"}]`, "http(s)"},
		{"path with brackets", `[{"baseUrl":"https://a.test/v1","balanceUrl":"/credits","balancePath":"data[0].x"}]`, "点分字段名"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			specs, problems := ParseNodeSpecs(c.json)
			if len(specs) != 0 || len(problems) != 1 || !strings.Contains(problems[0], c.want) {
				t.Fatalf("specs=%+v problems=%+v, want one complaint containing %q", specs, problems, c.want)
			}
		})
	}
}

func TestBalanceReadURL(t *testing.T) {
	cases := []struct{ base, decl, want string }{
		{"https://a.test/v1", "/credits", "https://a.test/v1/credits"},
		{"https://a.test/v1/", "me/credits", "https://a.test/v1/me/credits"},
		{"https://a.test/v1", "https://b.test/balance", "https://b.test/balance"},
	}
	for _, c := range cases {
		got, err := BalanceReadURL(c.base, c.decl)
		if err != nil || got != c.want {
			t.Errorf("BalanceReadURL(%q,%q) = %q, %v; want %q", c.base, c.decl, got, err, c.want)
		}
	}
	if _, err := BalanceReadURL("https://a.test", ""); err == nil {
		t.Error("an undeclared endpoint must not silently read a default URL")
	}
}

// A zero balance and an unreadable balance are different facts; the reader
// must distinguish numbers, numeric strings, missing fields and bad types.
func TestReadBalanceValue(t *testing.T) {
	body := []byte(`{"data":{"credits":12.5,"accounts":[{"total":"249.04"}],"name":"x"}}`)
	cases := []struct {
		path string
		want float64
	}{
		{"data.credits", 12.5},
		{"data.accounts.0.total", 249.04},
	}
	for _, c := range cases {
		got, err := ReadBalanceValue(body, c.path)
		if err != nil || got != c.want {
			t.Errorf("ReadBalanceValue(%q) = %v, %v; want %v", c.path, got, err, c.want)
		}
	}
	for _, bad := range []string{"data.missing", "data.accounts.9.total", "data.credits.oops", "data.name"} {
		if _, err := ReadBalanceValue(body, bad); err == nil {
			t.Errorf("path %q should fail instead of returning a guess", bad)
		}
	}
	if _, err := ReadBalanceValue([]byte(`not json`), "data.credits"); err == nil {
		t.Error("a non-JSON response must fail")
	}
}
