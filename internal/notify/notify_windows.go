//go:build windows

// Windows toast notifications, delivered through the PowerShell WinRT channel
// with the subprocess console hidden.
package notify

import (
	"encoding/base64"
	"os/exec"
	"strings"
	"syscall"
)

// Toast shows a Windows toast with app name "Zen Gate".
// Best-effort: PowerShell delivery failures are silently ignored.
func Toast(title, body string) {
	title, body, ok := want(title, body)
	if !ok {
		return
	}
	ps := buildScript(title, body)
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", ps)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	_ = cmd.Start()
}

// payload encodes one string for the script: XML-escaped, then base64. A toast
// title can come from the release feed, and the text used to be pasted into a
// double-quoted here-string — where PowerShell expands $(…) — so a payload
// carrying `"@` could end the string and run code, and a `</text>` could
// rewrite the toast document. base64 is the one alphabet that cannot do either.
func payload(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return base64.StdEncoding.EncodeToString([]byte(r.Replace(s)))
}

func buildScript(title, body string) string {
	decode := func(s string) string {
		return `$([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String("` + payload(s) + `")))`
	}
	// The here-string header must end its line: PowerShell 5.1 refuses
	// `$xml = @"<toast>…`, which is what the script used to be, so every toast
	// silently failed to parse. Keep the document on its own line.
	return `
[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] | Out-Null;
[Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime] | Out-Null;
$xml = @"
<toast><visual><binding template="ToastGeneric"><text>` + decode(title) + `</text><text>` + decode(body) + `</text></binding></visual></toast>
"@;
$doc = New-Object Windows.Data.Xml.Dom.XmlDocument;
$doc.LoadXml($xml);
$t = [Windows.UI.Notifications.ToastNotification]::new($doc);
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier("Zen Gate").Show($t);`
}
