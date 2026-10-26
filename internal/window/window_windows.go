//go:build windows

package window

import (
	"os"
	"path/filepath"

	"github.com/jchv/go-webview2"
	"github.com/jchv/go-webview2/webviewloader"

	"zen-gate/internal/notify"
	"zen-gate/internal/tray"
)

// Run creates the window, navigates to the URL and blocks until the window is
// closed. Must be called from the main thread: the WebView2 message pump owns
// it, while the tray runs on its own locked goroutine.
//
// Run is the last step of startup — the gateway is already serving by the time
// it is called. A missing WebView2 runtime (or an embedded window that fails
// to come up) must therefore not end the process: the app degrades to the
// Linux port's shape instead, tray resident and dashboard in the default
// browser, quitting only from the tray.
func Run(o Options) {
	setProcessDPIAwareness()

	// Pre-flight before NewWithOptions: on a machine without the runtime it
	// creates a bare Win32 window first and only then fails to embed, leaving
	// an orphan frame and surfacing the HRESULT through log.Printf, which
	// -H=windowsgui discards. An empty version with nil error is the loader's
	// "runtime not installed" answer (ERROR_FILE_NOT_FOUND maps to empty).
	version, verr := webviewloader.GetInstalledVersion()
	forced := os.Getenv("ZEN_GATE_NO_WEBVIEW2") != ""
	if ok, reason := embeddedDecision(version, verr, forced); !ok {
		runFallback(o, reason, false)
		return
	}

	// The tray must come up before the window so a close-to-tray app never
	// flashes a taskbar-less state.
	startTray(o)

	w := newWebviewSafely(o)
	if w == nil {
		// The runtime answered the pre-flight but environment creation still
		// failed; the library keeps the HRESULT to itself, so the reason stays
		// opaque here. Close the orphan frame and live on without the
		// embedded window.
		destroyOrphanWebview(o.Title)
		runFallback(o, "WebView2 环境创建失败（运行时在，窗口起不来）", true)
		return
	}
	defer w.Destroy()

	hwnd := uintptr(w.Window())
	if hwnd != 0 {
		themeTitleBar(hwnd)  // dark DWM border, no light 1px frame
		stripCaption(hwnd)   // the title bar is drawn by the UI itself
		setWindowIcons(hwnd) // taskbar + window icon (runtime WM_SETICON)
		if o.Bounds != nil && o.Bounds.W > 0 {
			w, h := o.Bounds.W, o.Bounds.H
			// A save that happened while minimized can persist the collapsed
			// titlebar size; fall back to the defaults rather than opening as
			// a tiny sliver.
			if w < 400 || h < 300 {
				w, h = o.Width, o.Height
			}
			ApplyBounds(hwnd, o.Bounds.X, o.Bounds.Y, w, h)
		}
		bindWindowControls(w, hwnd, o.OnCloseButton)
		if o.OnReady != nil {
			o.OnReady(hwnd)
		}
		if o.Bounds != nil && o.Bounds.Maximized {
			MaximizeWindow(hwnd)
		}
	}

	w.SetTitle(o.Title)
	w.Navigate(o.URL)
	w.Run()
}

// newWebviewSafely creates the embedded window, converting two failure modes
// that used to end the process into a nil return the caller degrades on.
//
//  1. A data path the WebView2 environment cannot use. On builds where the
//     profile directory exists but is not writable by this token (Roaming
//     profiles under AppData on some managed machines), environment creation
//     invokes the controller callback with a nil controller.
//  2. That nil controller. go-webview2 dereferences it unconditionally in
//     CreateCoreWebView2ControllerCompleted, so the failure surfaces as a nil
//     pointer dereference on the UI thread — no window, no tray, exit code 2.
//
// The fallback path is a throwaway profile under %TEMP%; the dashboard only
// serves the loopback UI, so a fresh profile each launch costs nothing beyond
// the cookie jar.
func newWebviewSafely(o Options) (w webview2.WebView) {
	path, ok := usableDataPath(o.DataPath)
	if !ok {
		dlogf("webview 数据目录不可写，改用临时 profile: %s", o.DataPath)
	}
	w, reason := createWebview(path, o)
	if w != nil {
		return w
	}
	if path == o.DataPath {
		// Retry once on a temp profile: the caller's directory may be fine at
		// stat time and poison the environment anyway.
		if alt, altOK := usableDataPath(""); altOK && alt != path {
			dlogf("webview 创建失败（%s），改用临时 profile 重试: %s", reason, alt)
			if w, _ := createWebview(alt, o); w != nil {
				return w
			}
		}
	}
	dlogf("webview 创建失败: %s", reason)
	return nil
}

// createWebview runs NewWithOptions with panic containment. It returns a nil
// WebView for both the documented nil return and a recovered panic, which is
// the signal the caller already branches on.
func createWebview(dataPath string, o Options) (v webview2.WebView, reason string) {
	defer func() {
		if p := recover(); p != nil {
			reason = "内嵌窗口初始化异常（WebView2 控制器为空）"
			dlogf("webview panic: %v", p)
			v = nil
		}
	}()
	v = webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		DataPath:  dataPath,
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  o.Title,
			Width:  uint(o.Width),
			Height: uint(o.Height),
			Center: true,
			IconId: 2,
		},
	})
	if v == nil {
		return nil, "WebView2 环境创建失败"
	}
	return v, ""
}

// usableDataPath reports whether a WebView2 profile directory can actually be
// written by this process, creating it first when needed. An empty path is the
// caller asking for the %TEMP% fallback. A write probe is the only reliable
// test: ACLs can grant the directory while denying child creation, and the
// WebView2 environment needs both.
func usableDataPath(dir string) (string, bool) {
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "zen-gate-webview")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return dir, false
	}
	probe, err := os.CreateTemp(dir, ".write-probe-*")
	if err != nil {
		return dir, false
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return dir, true
}

// bindWindowControls exposes the custom title bar's actions to JS. The HTML
// title bar calls these: drag on mousedown, buttons for min/max/close.
func bindWindowControls(w webview2.WebView, hwnd uintptr, onCloseButton func() bool) {
	_ = w.Bind("zengateDrag", func() error {
		startDrag(hwnd)
		return nil
	})
	_ = w.Bind("zengateMinimize", func() error {
		minimizeWindow(hwnd)
		return nil
	})
	_ = w.Bind("zengateToggleMax", func() error {
		toggleMaximize(hwnd)
		return nil
	})
	_ = w.Bind("zengateClose", func() error {
		if onCloseButton != nil && !onCloseButton() {
			return nil // close-to-tray: the window stays, just hidden by caller
		}
		closeWindow(hwnd)
		return nil
	})
}

// embeddedDecision reads a GetInstalledVersion pre-flight. A failed query
// (verr != nil) leaves the verdict open: attempting the real creation cannot
// make things worse than degrading on a bogus pre-flight.
func embeddedDecision(version string, verr error, forced bool) (bool, string) {
	if forced {
		return false, "ZEN_GATE_NO_WEBVIEW2=1（手动跳过内嵌窗口）"
	}
	if verr != nil {
		return true, ""
	}
	if version == "" {
		return false, "WebView2 Runtime 未安装"
	}
	return true, ""
}

// startTray hosts the tray on its own locked goroutine — the caller's thread
// stays with the window, or parks forever in fallback mode.
func startTray(o Options) {
	if o.Tray == nil {
		return
	}
	t := *o.Tray
	go func() {
		tray.Register(tray.Options{
			DashboardURL: t.DashboardURL,
			OnQuit:       t.OnQuit,
			OnReprobe:    t.OnReprobe,
			OnShow:       t.OnShow,
		})
		tray.Loop()
	}()
}

// runFallback keeps the app alive without the embedded window: the dashboard
// opens in the default browser and the tray stays resident. Blocks forever —
// the tray's 退出 path (OnQuit → os.Exit(0)) is the only way out, and it
// already runs the full gateway teardown.
func runFallback(o Options, reason string, trayStarted bool) {
	if o.Tray == nil {
		return // nothing hosts a quit path; let the caller tear down as before
	}
	dlogf("内嵌窗口不可用，改用默认浏览器打开管理页: %s", reason)
	if !trayStarted {
		startTray(o)
	}
	notify.Toast("内嵌窗口不可用", "管理页已在默认浏览器打开 · "+reason)
	if o.URL != "" {
		tray.Open(o.URL)
	}
	<-make(chan struct{}) // nothing closes this channel; the tray quit path exits the process
}

// destroyOrphanWebview closes the bare frame NewWithOptions leaves behind when
// the embed fails after the window was created and shown. Matched by class
// plus exact title so it can never hit another app's webview-class window.
func destroyOrphanWebview(title string) {
	hwnd, _, _ := procFindWindow.Call(toUTF16("webview"), toUTF16(title))
	if hwnd != 0 {
		procDestroyWindow.Call(hwnd)
	}
}
