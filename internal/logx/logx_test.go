package logx

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readLog returns today's daily file contents for this logger.
func readLog(t *testing.T, l *Logger) string {
	t.Helper()
	name := filepath.Join(l.dir, time.Now().Format("2006-01-02")+".log")
	b, err := os.ReadFile(name)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func mustLogf(t *testing.T, l *Logger, cat, level, format string, args ...any) {
	t.Helper()
	l.Logf(cat, level, format, args...)
}

func TestCategorySwitchIsAuthoritativeEverywhere(t *testing.T) {
	dir := t.TempDir()
	l := New(dir)
	defer l.Close()
	l.SetCategories(map[string]bool{"access": false, "routing": true})

	mustLogf(t, l, "access", "info", "should-not-appear")
	mustLogf(t, l, "routing", "info", "routing-line")

	if got := readLog(t, l); strings.Contains(got, "should-not-appear") {
		t.Errorf("disabled category still reached the file:\n%s", got)
	}
	if got := readLog(t, l); !strings.Contains(got, "routing-line") {
		t.Errorf("enabled category missing from the file:\n%s", got)
	}
	tail := l.Tail("", 0)
	for _, e := range tail {
		if e.Msg == "should-not-appear" {
			t.Errorf("disabled category still reached the ring: %+v", tail)
		}
	}
	if len(tail) != 1 || tail[0].Cat != "routing" {
		t.Errorf("tail = %+v, want exactly the routing entry", tail)
	}
}

func TestUnknownCategoryDefaultsToOn(t *testing.T) {
	l := New(t.TempDir())
	defer l.Close()
	mustLogf(t, l, "app", "info", "legacy-line")
	if !strings.Contains(readLog(t, l), "legacy-line") {
		t.Fatal("a category the config never mentioned should not silence the legacy call sites")
	}
}

func TestTurningCategoryOnLaterStartsRecording(t *testing.T) {
	l := New(t.TempDir())
	defer l.Close()
	l.SetCategories(map[string]bool{"upstream": false})
	mustLogf(t, l, "upstream", "warn", "before")
	l.SetCategories(map[string]bool{"upstream": true})
	mustLogf(t, l, "upstream", "warn", "after")

	got := readLog(t, l)
	if strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Fatalf("live switch did not take effect:\n%s", got)
	}
}

func TestLevelFilterAppliesToFileOnlyNotTheRing(t *testing.T) {
	l := New(t.TempDir())
	defer l.Close()
	l.SetFileLevel("warn")
	l.Infof("quiet-info")
	l.Warnf("loud-warn")

	got := readLog(t, l)
	if strings.Contains(got, "quiet-info") || !strings.Contains(got, "loud-warn") {
		t.Errorf("file level filter wrong:\n%s", got)
	}
	tail := l.Tail("", 0)
	if len(tail) != 2 {
		t.Fatalf("ring should hold both entries, got %+v", tail)
	}
}

func TestDebugfExistsAndLevelsAreTagged(t *testing.T) {
	l := New(t.TempDir())
	defer l.Close()
	l.Debugf("detail %d", 7)
	l.Errorf("boom")

	tail := l.Tail("", 0)
	if len(tail) != 2 {
		t.Fatalf("tail = %+v", tail)
	}
	if tail[0].Level != "DEBUG" || tail[0].Msg != "detail 7" {
		t.Errorf("Debugf = %+v", tail[0])
	}
	if tail[1].Level != "ERROR" {
		t.Errorf("Errorf = %+v", tail[1])
	}
}

func TestRingSurvivesAnHourOfAccessLogs(t *testing.T) {
	l := New(t.TempDir())
	defer l.Close()
	// The legacy ring capped at 1000, so a few minutes of always-on access
	// logging flushed every earlier warning out of the viewer. The interesting
	// line therefore has to be the OLDEST entry and still be present.
	l.Warnf("the one thing you wanted to still see")
	for i := 0; i < 3500; i++ {
		l.Logf("access", "info", "rid=n%d status=200", i)
	}

	tail := l.Tail("", 0)
	if len(tail) == 0 {
		t.Fatal("empty ring")
	}
	found := false
	for _, e := range tail {
		if e.Msg == "the one thing you wanted to still see" {
			found = true
		}
	}
	if !found {
		t.Fatalf("3500 access lines evicted the warning; ring kept %d entries", len(tail))
	}
	if len(tail) > ringSize {
		t.Errorf("ring grew past its cap: %d > %d", len(tail), ringSize)
	}
}

func TestKeepDaysIsConfigurableAndSweeps(t *testing.T) {
	dir := t.TempDir()
	logs := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(daysAgo int) string {
		day := time.Now().AddDate(0, 0, -daysAgo).Format("2006-01-02")
		name := filepath.Join(logs, day+".log")
		if err := os.WriteFile(name, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return name
	}

	l := New(dir)
	// A longer window must spare a file the hardcoded 7-day default would kill.
	nine := write(9)
	l.SetKeepDays(30)
	l.sweep()
	if _, err := os.Stat(nine); err != nil {
		t.Fatalf("30-day retention swept a 9-day file: %v", err)
	}

	old := write(9)
	edge := write(3)
	l.SetKeepDays(5)
	l.sweep()
	l.Close()

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("9-day-old file survived a 5-day retention")
	}
	if _, err := os.Stat(edge); err != nil {
		t.Errorf("3-day-old file was swept by a 5-day retention: %v", err)
	}
}

func TestStdoutMirrorOffByDefault(t *testing.T) {
	dir := t.TempDir()
	l := New(dir)
	defer l.Close()
	if l.writesStdout() {
		t.Fatal("a GUI build must not write to stdout unless asked")
	}
	l.SetStdout(true)
	if !l.writesStdout() {
		t.Fatal("SetStdout(true) did not take")
	}
}

func TestEntryCarriesCategoryIntoJSON(t *testing.T) {
	e := Entry{At: time.Unix(1, 0), Level: "INFO", Cat: "access", Msg: "m"}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"cat":"access"`) {
		t.Errorf("marshal = %s", b)
	}
}

func TestQueryReadsADayFileAfterTheRingIsGone(t *testing.T) {
	l := New(t.TempDir())
	l.SetCategories(map[string]bool{"access": true})
	l.Logf("access", "info", "rid=abc status=200")
	l.Logf("routing", "info", "picked free lane")
	l.Close()

	// The viewer must reach history once it has aged out of the ring.
	l.mu2.Lock()
	l.ring = nil
	l.mu2.Unlock()

	got := l.Query(Query{Day: time.Now().Format("2006-01-02"), Limit: 10})
	if len(got) != 2 {
		t.Fatalf("day query = %+v, want both lines back from disk", got)
	}
	if got[0].Cat != "access" || got[0].Msg != "rid=abc status=200" {
		t.Errorf("first = %+v", got[0])
	}
	if !strings.Contains(got[1].Msg, "picked free lane") || got[1].Level != "INFO" {
		t.Errorf("second = %+v", got[1])
	}
}

func TestQueryNarrowsByCategoryAndKeyword(t *testing.T) {
	day := time.Now().Format("2006-01-02")
	l := New(t.TempDir())
	l.SetCategories(map[string]bool{"access": true, "upstream": true})
	l.Logf("access", "info", "rid=one status=200")
	l.Logf("access", "info", "rid=two status=500")
	l.Logf("upstream", "warn", "rid=two attempt=2 http=429")
	defer l.Close()

	if got := l.Query(Query{Day: day, Cats: []string{"upstream"}}); len(got) != 1 {
		t.Fatalf("cat filter = %+v", got)
	}
	if got := l.Query(Query{Day: day, Contains: "status=500"}); len(got) != 1 ||
		!strings.Contains(got[0].Msg, "rid=two") {
		t.Fatalf("keyword filter = %+v", got)
	}
	// The keyword match must not leak the other category in.
	if got := l.Query(Query{Day: day, Cats: []string{"access"}, Contains: "rid=two"}); len(got) != 1 {
		t.Fatalf("combined filter = %+v", got)
	}
}

func TestQueryReadsLegacyLinesWithoutACategory(t *testing.T) {
	// Files written by older builds have no [cat] segment; they should still be
	// readable rather than dropped.
	dir := t.TempDir()
	logs := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	day := time.Now().Format("2006-01-02")
	line := "[" + time.Now().Format("2006-01-02 15:04:05.000") + "] [ERROR] legacy boom\n"
	if err := os.WriteFile(filepath.Join(logs, day+".log"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	l := New(dir)
	defer l.Close()

	got := l.Query(Query{Day: day})
	if len(got) != 1 || got[0].Cat != "" || got[0].Msg != "legacy boom" || got[0].Level != "ERROR" {
		t.Fatalf("legacy line parsed as %+v", got)
	}
}

// A legacy message that itself opens with a bracketed tag must not be eaten as
// a category — that is what separates "[access]" from "[LAN] 已开启".
func TestParseLineKeepsBracketedLegacyMessage(t *testing.T) {
	ts := time.Now().Format(timeLayout)
	e, ok := parseLine("[" + ts + "] [INFO] [LAN] 已开启")
	if !ok {
		t.Fatal("legacy line rejected")
	}
	if e.Cat != "" || e.Msg != "[LAN] 已开启" {
		t.Errorf("parsed as %+v, want the cat empty and the message kept whole", e)
	}

	e, ok = parseLine("[" + ts + "] [INFO] {access} rid=one")
	if !ok || e.Cat != "access" || e.Msg != "rid=one" {
		t.Errorf("new-format line parsed as %+v (ok=%v)", e, ok)
	}
}

func TestQueryRejectsADayThatIsNotADate(t *testing.T) {
	l := New(t.TempDir())
	defer l.Close()
	l.Infof("seed")
	for _, bad := range []string{"../config", "2026-13-45", "config.json", ""} {
		if bad == "" {
			continue // empty Day legitimately means "read the ring"
		}
		if got := l.Query(Query{Day: bad}); len(got) != 0 {
			t.Errorf("Day=%q returned %d entries, want 0", bad, len(got))
		}
	}
}

func TestDaysListsRetainedFilesNewestFirst(t *testing.T) {
	dir := t.TempDir()
	logs := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	today := time.Now().Format("2006-01-02")
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	for _, name := range []string{today + ".log", yesterday + ".log", "notes.txt", "keepme.log"} {
		if err := os.WriteFile(filepath.Join(logs, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	l := New(dir)
	defer l.Close()

	got := l.Days()
	if len(got) != 2 || got[0] != today || got[1] != yesterday {
		t.Fatalf("Days() = %v, want the two dated files newest first", got)
	}
}

// The shipped switch set is what a fresh install runs with: the client-facing
// audit trail on, the two high-volume classes off until the user asks.
func TestDefaultCategoriesSilenceOnlyTheHighVolumeClasses(t *testing.T) {
	l := New(t.TempDir())
	defer l.Close()
	def := DefaultCategories()
	for _, cat := range []string{CatAccess, CatRouting, CatAdmin, CatProbe, CatLifecycle} {
		if !def[cat] {
			t.Errorf("default %s = off, want on", cat)
		}
	}
	for _, cat := range []string{CatUpstream, CatContent} {
		if def[cat] {
			t.Errorf("default %s = on, want off", cat)
		}
	}

	l.SetCategories(def)
	l.Infof("bare app line")
	l.Logf(CatAccess, "info", "a%d", 1)
	l.Logf(CatUpstream, "info", "u%d", 1)
	l.Logf(CatContent, "info", "c%d", 1)
	if n := len(l.Query(Query{Cats: []string{CatAccess}})); n != 1 {
		t.Errorf("access lines = %d, want 1", n)
	}
	if n := len(l.Query(Query{Cats: []string{CatApp}})); n != 1 {
		t.Errorf("app lines = %d, want 1", n)
	}
	if n := len(l.Query(Query{})); n != 2 {
		t.Errorf("total lines = %d, want only access+app: %+v", n, l.Query(Query{}))
	}
}

// A persisted switch set can be partial (or missing entirely on an upgraded
// config), so the merge has to fill every omitted class back in from the
// shipped defaults — otherwise "turn content on" would switch everything on.
func TestEffectiveCategoriesMergeWithTheShippedDefaults(t *testing.T) {
	if got := EffectiveCategories(nil); !equalCats(got, DefaultCategories()) {
		t.Errorf("EffectiveCategories(nil) = %+v, want the shipped defaults", got)
	}
	got := EffectiveCategories(map[string]bool{"Content": true, "routing": false})
	if !got[CatContent] {
		t.Errorf("content = off, want the requested on (and the key lowercased)")
	}
	if got[CatRouting] {
		t.Errorf("routing = on, want the requested off")
	}
	if !got[CatAccess] {
		t.Errorf("access = off, want the untouched default on")
	}
	if got[CatUpstream] {
		t.Errorf("upstream = on, want the untouched default off")
	}
}

func equalCats(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
