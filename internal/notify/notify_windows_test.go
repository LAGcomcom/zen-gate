//go:build windows

package notify

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The title and body are not ours: the 有新版本 toast carries a version string
// read out of the release feed. Doubling backticks does not stop PowerShell
// from expanding a $(…) subexpression inside a double-quoted here-string, and
// nothing stopped a payload from closing the toast's <text> node either.
func TestBuildScriptKeepsRemotePayloadOutOfThePowerShellSource(t *testing.T) {
	title := `1.0"@ ; $(Get-Content my-secret) </text><text>注入`
	ps := buildScript(title, "body")
	if strings.Contains(ps, "Get-Content") {
		t.Errorf("the payload reached the PowerShell source, so its $(…) would run:\n%s", ps)
	}
	if strings.Contains(ps, "</text><text>注入") {
		t.Errorf("the payload could rewrite the toast XML:\n%s", ps)
	}
}

// Out of the script source, but still into the notification: what travels must
// decode back to XML-escaped text, so the reader sees the version string
// instead of a document the payload restructured.
func TestBuildScriptCarriesThePayloadEncoded(t *testing.T) {
	title := `a & b <c>`
	ps := buildScript(title, "body")
	encoded := base64.StdEncoding.EncodeToString([]byte(`a &amp; b &lt;c&gt;`))
	if !strings.Contains(ps, encoded) {
		t.Errorf("the script must carry the XML-escaped title base64-encoded (%s):\n%s", encoded, ps)
	}
}

// The toast used to be built by a script PowerShell could not even parse
// (`$xml = @"<toast>…` — a here-string header may not have content after it),
// and because delivery is best-effort the failure was invisible. Parse the real
// script without running it, so no notification pops up over the desktop.
func TestBuildScriptIsParsablePowerShell(t *testing.T) {
	if _, err := exec.LookPath("powershell"); err != nil {
		t.Skip("powershell is not available")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "toast.ps1")
	if err := os.WriteFile(script, []byte(buildScript(`标题 "@ $(inject) & <x>`, "body")), 0o600); err != nil {
		t.Fatal(err)
	}
	checker := filepath.Join(dir, "parse.ps1")
	code := `$ErrorActionPreference='Stop';` + "\n" +
		`$code = Get-Content -Raw -LiteralPath $env:ZEN_TOAST_SCRIPT;` + "\n" +
		`$errs = $null;` + "\n" +
		`[System.Management.Automation.Language.Parser]::ParseInput($code, [ref]$null, [ref]$errs) | Out-Null;` + "\n" +
		`$errs | ForEach-Object { $_.Message };` + "\n" +
		`if ($errs.Count) { exit 1 };` + "\n" +
		`"parsed"` + "\n"
	if err := os.WriteFile(checker, []byte(code), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", checker)
	cmd.Env = append(os.Environ(), "ZEN_TOAST_SCRIPT="+script)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "parsed") {
		t.Fatalf("PowerShell cannot parse the toast script: %v\n%s", err, out)
	}
}
