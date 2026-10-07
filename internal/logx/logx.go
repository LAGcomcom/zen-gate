// Package logx is zen-gate's logger: daily-sharded files with retention,
// plus an in-memory ring surfaced to the dashboard's log viewer.
//
// Two independent dimensions gate a line: its category (the user's per-class
// switches, authoritative — a disabled category records nothing at all, ring
// included) and its level (which only decides whether the line reaches the
// file, so the live viewer keeps seeing info lines even when the file is
// trimmed to warnings).
package logx

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Entry is one log line kept in the ring.
type Entry struct {
	At    time.Time `json:"at"`
	Level string    `json:"level"`
	Cat   string    `json:"cat,omitempty"`
	Msg   string    `json:"msg"`
}

// CatApp is the category of the bare Infof/Warnf/Errorf call sites — startup
// and anything not (yet) classified. It is always on.
const CatApp = "app"

// The classes the user can switch on and off from the dashboard.
const (
	// CatAccess is one line per client request: what was asked, what answered,
	// the status and the token bill.
	CatAccess = "access"
	// CatUpstream is one line per physical attempt on the free lane or a
	// user-added provider, including the attempts a failover hid from the
	// client.
	CatUpstream = "upstream"
	// CatContent is the only class that quotes request/response bodies, as a
	// short sanitized excerpt.
	CatContent = "content"
	// CatRouting records lane failover, fallback and strategy decisions.
	CatRouting = "routing"
	// CatAdmin records dashboard configuration changes.
	CatAdmin = "admin"
	// CatProbe records availability probing of models and providers.
	CatProbe = "probe"
	// CatLifecycle records startup, shutdown, updates and sidecar events.
	CatLifecycle = "lifecycle"
)

// DefaultCategories is the shipped switch set. The audit trail is always on;
// the two high-volume classes — every attempt of every failover, and body
// excerpts — stay off until the user asks for them.
func DefaultCategories() map[string]bool {
	return map[string]bool{
		CatAccess:    true,
		CatRouting:   true,
		CatAdmin:     true,
		CatProbe:     true,
		CatLifecycle: true,
		CatUpstream:  false,
		CatContent:   false,
	}
}

// EffectiveCategories merges a (possibly partial) persisted switch set onto
// the shipped defaults, so turning one class on never silently turns the rest
// on. Keys are lowercased to match SetCategories.
func EffectiveCategories(on map[string]bool) map[string]bool {
	out := DefaultCategories()
	for k, v := range on {
		out[strings.ToLower(k)] = v
	}
	return out
}

const (
	ringSize        = 4000
	DefaultKeepDays = 7
	// DefaultFileLevel is the unfiltered setting: the file only trims once the
	// user asks it to.
	DefaultFileLevel = "debug"
	timeLayout       = "2006-01-02 15:04:05.000"
	dayLayout        = "2006-01-02"
)

type Logger struct {
	dir  string
	mu   sync.Mutex
	file *os.File
	day  string
	ring []Entry
	mu2  sync.Mutex // guards ring
	min  string     // minimum level written to file, resolved by SetFileLevel
	// cats is nil until the user's switches are wired; a missing key is on,
	// so an unclassified category can never silently vanish.
	cats     map[string]bool
	keepDays int
	stdout   bool
}

// New creates the logger; dir = <data>/logs. Old files beyond the retention
// window are swept on start.
func New(dataDir string) *Logger {
	dir := filepath.Join(dataDir, "logs")
	_ = os.MkdirAll(dir, 0o700)
	l := &Logger{dir: dir, keepDays: DefaultKeepDays}
	l.sweep()
	return l
}

// SetFileLevel filters what reaches the file: "debug" (the default, i.e. no
// filter), "info", "warn", "error". The ring always keeps everything enabled.
func (l *Logger) SetFileLevel(level string) {
	if level == "" {
		level = DefaultFileLevel
	}
	l.mu.Lock()
	l.min = strings.ToLower(level)
	l.mu.Unlock()
}

// SetCategories installs the per-class switches. A category present and false
// is silenced completely; absent means on.
func (l *Logger) SetCategories(on map[string]bool) {
	next := make(map[string]bool, len(on))
	for k, v := range on {
		next[strings.ToLower(k)] = v
	}
	l.mu.Lock()
	l.cats = next
	l.mu.Unlock()
}

// SetKeepDays changes how many daily files are retained.
func (l *Logger) SetKeepDays(days int) {
	if days <= 0 {
		days = DefaultKeepDays
	}
	l.mu.Lock()
	l.keepDays = days
	l.mu.Unlock()
}

// SetStdout mirrors lines to stdout, which is what a -no-tray run needs to be
// debuggable without opening the file.
func (l *Logger) SetStdout(on bool) {
	l.mu.Lock()
	l.stdout = on
	l.mu.Unlock()
}

func (l *Logger) writesStdout() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stdout
}

// Enabled reports whether a class currently records, so an expensive line
// (body excerpts, for instance) can be skipped instead of formatted and
// dropped.
func (l *Logger) Enabled(cat string) bool { return l.enabled(cat) }

func (l *Logger) enabled(cat string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cats == nil {
		return true
	}
	on, ok := l.cats[strings.ToLower(cat)]
	return !ok || on
}

func levelRank(level string) int {
	switch strings.ToLower(level) {
	case "debug":
		return 0
	case "info":
		return 1
	case "warn":
		return 2
	case "error":
		return 3
	}
	return 1
}

// Logf writes one line under an explicit category.
func (l *Logger) Logf(cat, level, format string, args ...any) {
	if !l.enabled(cat) {
		return
	}
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	e := Entry{At: time.Now(), Level: strings.ToUpper(level), Cat: cat, Msg: msg}

	l.mu2.Lock()
	l.ring = append(l.ring, e)
	if len(l.ring) > ringSize {
		l.ring = l.ring[len(l.ring)-ringSize:]
	}
	l.mu2.Unlock()

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stdout {
		fmt.Fprintf(os.Stdout, "[%s] [%s] {%s} %s\n", e.At.Format(timeLayout), e.Level, cat, msg)
	}
	if rank := levelRank(level); rank < levelRank(l.min) {
		return
	}
	day := e.At.Format(dayLayout)
	if l.file == nil || l.day != day {
		if l.file != nil {
			_ = l.file.Close()
		}
		f, err := os.OpenFile(filepath.Join(l.dir, day+".log"),
			os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		l.file, l.day = f, day
	}
	fmt.Fprintf(l.file, "[%s] [%s] {%s} %s\n", e.At.Format(timeLayout), e.Level, cat, msg)
}

func (l *Logger) Debugf(format string, args ...any) { l.Logf(CatApp, "debug", format, args...) }
func (l *Logger) Infof(format string, args ...any)  { l.Logf(CatApp, "info", format, args...) }
func (l *Logger) Warnf(format string, args ...any)  { l.Logf(CatApp, "warn", format, args...) }
func (l *Logger) Errorf(format string, args ...any) { l.Logf(CatApp, "error", format, args...) }

// Tail returns the newest entries (optionally filtered by minimum level).
func (l *Logger) Tail(minLevel string, limit int) []Entry {
	return l.Query(Query{MinLevel: minLevel, Limit: limit})
}

// Query selects log lines.
type Query struct {
	Day      string
	Cats     []string
	MinLevel string
	Contains string
	Limit    int
}

func (q Query) wants(cat string) bool {
	if len(q.Cats) == 0 {
		return true
	}
	for _, c := range q.Cats {
		if strings.EqualFold(c, cat) {
			return true
		}
	}
	return false
}

func (q Query) wantsLevel(level string) bool {
	if q.MinLevel == "" {
		return true
	}
	return levelRank(strings.ToLower(level)) >= levelRank(strings.ToLower(q.MinLevel))
}

func (q Query) wantsMsg(msg string) bool {
	return q.Contains == "" || strings.Contains(strings.ToLower(msg), strings.ToLower(q.Contains))
}

// Query returns the newest matches, oldest first. An empty Day reads the live
// ring; a "2006-01-02" Day reads that day's file, which is how the viewer
// reaches history that already aged out of the ring.
func (l *Logger) Query(q Query) []Entry {
	var pool []Entry
	if q.Day == "" {
		l.mu2.Lock()
		pool = append(pool, l.ring...)
		l.mu2.Unlock()
	} else {
		pool = l.readFile(q.Day)
	}
	out := []Entry{}
	for _, e := range pool {
		if q.wants(e.Cat) && q.wantsLevel(e.Level) && q.wantsMsg(e.Msg) {
			out = append(out, e)
		}
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[len(out)-q.Limit:]
	}
	return out
}

// readFile parses one daily file, newest chunk first so a huge file cannot
// balloon memory. Lines written before categories existed simply come back
// with an empty Cat.
func (l *Logger) readFile(day string) []Entry {
	if _, err := time.Parse(dayLayout, day); err != nil {
		return nil
	}
	name := filepath.Join(l.dir, day+".log")
	info, err := os.Stat(name)
	if err != nil {
		return nil
	}
	const window = 4 << 20 // 4 MiB tail is plenty for one day of lines
	size := info.Size()
	offset := int64(0)
	if size > window {
		offset = size - window
	}
	f, err := os.Open(name)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, size-offset)
	if _, err := f.ReadAt(buf, offset); err != nil && err.Error() != "EOF" {
		return nil
	}
	out := make([]Entry, 0, 4096)
	for _, line := range strings.Split(string(buf), "\n") {
		if e, ok := parseLine(line); ok {
			out = append(out, e)
		}
	}
	return out
}

// parseLine reverses "[ts] [LEVEL] {cat} msg", and the older
// "[ts] [LEVEL] msg" shape. The category sits in braces precisely so a message
// that opens with a bracketed tag — "[LAN] 已开启" — is never eaten as one.
func parseLine(line string) (Entry, bool) {
	line = strings.TrimSuffix(line, "\r")
	ts, rest, ok := bracket(line)
	if !ok {
		return Entry{}, false
	}
	level, rest, ok := bracket(rest)
	if !ok {
		return Entry{}, false
	}
	at, err := time.ParseInLocation(timeLayout, ts, time.Local)
	if err != nil {
		return Entry{}, false
	}
	cat := ""
	rest = strings.TrimPrefix(rest, " ")
	if strings.HasPrefix(rest, "{") {
		end := strings.Index(rest, "}")
		if end < 0 {
			return Entry{}, false
		}
		cat, rest = rest[1:end], strings.TrimPrefix(rest[end+1:], " ")
	}
	return Entry{At: at, Level: strings.ToUpper(level), Cat: cat, Msg: rest}, true
}

// bracket peels one leading "[...]" group.
func bracket(s string) (string, string, bool) {
	s = strings.TrimPrefix(s, " ")
	if !strings.HasPrefix(s, "[") {
		return "", s, false
	}
	end := strings.Index(s, "]")
	if end < 0 {
		return "", s, false
	}
	return s[1:end], s[end+1:], true
}

// Days lists the retained daily files, newest first — what the viewer offers
// as its history choices.
func (l *Logger) Days() []string {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return nil
	}
	days := []string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		day := strings.TrimSuffix(e.Name(), ".log")
		if _, err := time.Parse(dayLayout, day); err == nil {
			days = append(days, day)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(days)))
	return days
}

// sweep removes log files beyond the retention window.
func (l *Logger) sweep() {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return
	}
	l.mu.Lock()
	keep := l.keepDays
	l.mu.Unlock()
	if keep <= 0 {
		keep = DefaultKeepDays
	}
	cutoff := time.Now().AddDate(0, 0, -keep)
	names := []string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		day := strings.TrimSuffix(name, ".log")
		t, err := time.ParseInLocation(dayLayout, day, time.Local)
		if err != nil {
			continue
		}
		if t.Before(cutoff) {
			_ = os.Remove(filepath.Join(l.dir, name))
		}
	}
}

// Close flushes the current file.
func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
}
