package gateway

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// esc() is the dashboard's only guard on values that land inside HTML
// attributes — a provider's baseUrl or a model id read out of its /v1/models is
// input from outside the gateway. Quotes were not escaped, so a `"` could close
// the attribute and run script in the admin page.
func TestDashboardEscNeutralisesAttributeDelimiters(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not available")
	}
	dir := t.TempDir()
	harness := filepath.Join(dir, "esc.mjs")
	body := escSource(t) + "\nprocess.stdout.write(esc(process.argv[2]));\n"
	if err := os.WriteFile(harness, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, harness, `a" onerror="alert(1) b'<c>&`).CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, out)
	}
	want := `a&#34; onerror=&#34;alert(1) b&#39;&lt;c&gt;&amp;`
	if got := string(out); got != want {
		t.Errorf("esc(%q) = %q, want %q", `a" onerror="alert(1) b'<c>&`, got, want)
	}
}

// escSource pulls the dashboard's own esc() out of the embedded page, so the
// test runs the shipped function rather than a copy of it.
func escSource(t *testing.T) string {
	t.Helper()
	return dashboardFunc(t, "function esc(")
}

// dashboardFunc returns the one-line function in the page whose definition
// starts at marker.
func dashboardFunc(t *testing.T, marker string) string {
	t.Helper()
	for _, line := range strings.Split(string(dashboardHTML), "\n") {
		if i := strings.Index(line, marker); i >= 0 {
			return strings.TrimSpace(line[i:])
		}
	}
	t.Fatalf("the dashboard no longer defines %s", marker)
	return ""
}

// 详情 links take their target from the announcement feed and the model
// catalog, and a feed is not ours — escaping the quotes still leaves
// `javascript:` as a working href.
func TestDashboardSafeHrefKeepsOnlyWebSchemes(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not available")
	}
	dir := t.TempDir()
	harness := filepath.Join(dir, "href.mjs")
	body := escSource(t) + "\n" + dashboardFunc(t, "function safeHref(") +
		"\nprocess.stdout.write(esc(safeHref(process.argv[2])));\n"
	if err := os.WriteFile(harness, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ in, want string }{
		{"https://example.com/a?b=1&c=2", "https://example.com/a?b=1&amp;c=2"},
		{"http://127.0.0.1:8787/v1", "http://127.0.0.1:8787/v1"},
		{"javascript:alert(1)", "#"},
		{"JavaScript:alert(1)", "#"},
		{"  javascript:alert(1)", "#"},
		{"data:text/html;base64,PHNjcmlwdD4=", "#"},
		{"", "#"},
		{"https:/example.com", "#"},
	} {
		out, err := exec.Command(node, harness, tc.in).CombinedOutput()
		if err != nil {
			t.Fatalf("node failed on %q: %v\n%s", tc.in, err, out)
		}
		if got := string(out); got != tc.want {
			t.Errorf("safeHref(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
