// Package tray runs the menu-bar / system-tray icon and menu.
//
// The package is split in two halves because the two platforms disagree about
// which thread owns UI: Windows is happy with the tray loop on its own locked
// goroutine while the WebView2 window owns the main thread; macOS requires
// *both* AppKit objects to be created on the main thread inside NSApp.run.
// Callers therefore Register the options and then hand the thread to Loop —
// window.Run does exactly that, differently per platform.
package tray

import (
	"sync"

	"github.com/getlantern/systray"
)

// Options configures the tray.
type Options struct {
	DashboardURL string
	OnQuit       func()
	OnReprobe    func() // tray menu: re-probe availability
	OnShow       func() // tray menu: focus the main window
	// OnReady runs on the UI thread right after the icon and menu exist. The
	// macOS window is created from here, so it must not block.
	OnReady func()
}

var (
	mu         sync.Mutex
	registered Options

	statusCh   chan string
	lastStatus string
)

// Register stores the tray configuration. It does no work on the UI thread, so
// it is safe to call from a goroutine that is about to hand the thread to Loop.
func Register(o Options) {
	mu.Lock()
	registered = o
	mu.Unlock()
}

// Loop creates the tray icon and menu, then runs the platform event loop until
// Quit. It must run on the thread Register was called from.
func Loop() {
	o := registered
	systray.Run(func() {
		setIcon()
		systray.SetTitle("Zen Gate")
		systray.SetTooltip("Zen Gate · Local free-model gateway")
		mu.Lock()
		statusCh = make(chan string, 16)
		pending := lastStatus
		mu.Unlock()
		if pending != "" {
			systray.SetTooltip(pending)
		}
		go func() {
			for t := range statusCh {
				systray.SetTooltip(t)
			}
		}()
		mOpen := systray.AddMenuItem("Open Dashboard", "Focus the main window")
		mReprobe := systray.AddMenuItem("Re-probe availability", "Re-probe model availability")
		mCopy := systray.AddMenuItem("Copy gateway URL", "Copy base URL")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Quit", "Quit zen-gate")
		mOpen.Enable()
		go func() {
			for {
				select {
				case <-mOpen.ClickedCh:
					if o.OnShow != nil {
						o.OnShow()
						continue
					}
					Open(o.DashboardURL)
				case <-mReprobe.ClickedCh:
					if o.OnReprobe != nil {
						o.OnReprobe()
					}
				case <-mCopy.ClickedCh:
					copyToClipboard(o.DashboardURL)
				case <-mQuit.ClickedCh:
					systray.Quit()
				}
			}
		}()
		if o.OnReady != nil {
			o.OnReady()
		}
	}, func() {
		if o.OnQuit != nil {
			o.OnQuit()
		}
	})
}

// Quit tears the tray down, which unblocks Loop.
func Quit() { systray.Quit() }

// SetStatus updates the tray tooltip with live state. Safe to call before
// Loop: statuses are queued and applied once the tray is ready.
func SetStatus(text string) {
	mu.Lock()
	defer mu.Unlock()
	if statusCh == nil {
		lastStatus = text
		return
	}
	select {
	case statusCh <- text:
	default:
	}
}

// Open opens url in the user's default handler (browser).
func Open(url string) { openURL(url) }
