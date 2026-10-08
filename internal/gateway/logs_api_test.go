package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zen-gate/internal/logx"
)

// logLines decodes just the entries array so a filter test can assert on order.
func logEntries(t *testing.T, base, query string) []logx.Entry {
	t.Helper()
	resp, err := http.Get(base + "/admin/api/logs" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("logs%s = %d: %s", query, resp.StatusCode, body)
	}
	var out struct {
		Entries []logx.Entry `json:"entries"`
		Days    []string     `json:"days"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return out.Entries
}

// The 排障 path is: open the 日志 page, switch the class on, and narrow to the
// one request by id — so the endpoint has to filter by category and message.
func TestLogsAPIFiltersByCategoryAndKeyword(t *testing.T) {
	up := fakeUpstream(t, []string{"[DONE]"})
	defer up.Close()
	s := newTestServer(t, up)
	l := wireLog(t, s)
	cats := logx.DefaultCategories()
	cats[logx.CatUpstream] = true
	l.SetCategories(cats)
	s.logCat(logx.CatAccess, "info", "rid=aaa chat status=200")
	s.logCat(logx.CatUpstream, "info", "rid=aaa lane model=x")
	s.logCat(logx.CatRouting, "warn", "rid=bbb 免费车道耗尽")

	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	acc := logEntries(t, ts.URL, "?cat=access")
	if len(acc) != 1 || acc[0].Cat != logx.CatAccess {
		t.Fatalf("cat=access = %+v, want the one access line", acc)
	}
	pair := logEntries(t, ts.URL, "?cat=access,upstream")
	if len(pair) != 2 {
		t.Fatalf("cat=access,upstream = %+v, want 2 lines", pair)
	}
	for _, e := range pair {
		if e.Cat != logx.CatAccess && e.Cat != logx.CatUpstream {
			t.Errorf("filtered result leaked cat %q: %+v", e.Cat, e)
		}
	}
	kw := logEntries(t, ts.URL, "?q=%E5%85%8D%E8%B4%B9%E8%BD%A6%E9%81%93")
	if len(kw) != 1 || !strings.Contains(kw[0].Msg, "免费车道耗尽") {
		t.Fatalf("keyword search = %+v, want the routing line", kw)
	}
	lvl := logEntries(t, ts.URL, "?level=warn")
	if len(lvl) != 1 || lvl[0].Level != "WARN" {
		t.Fatalf("level=warn = %+v, want only the warning", lvl)
	}
	if n := len(logEntries(t, ts.URL, "?limit=1")); n != 1 {
		t.Errorf("limit=1 returned %d lines", n)
	}
}

// The ring only holds the last few thousand lines; the daily file is the audit
// record. ?day= must read that file — a line the ring never saw has to come
// back — and the response has to list the files the picker can offer.
func TestLogsAPIReadsADayFileTheRingNeverSaw(t *testing.T) {
	up := fakeUpstream(t, []string{"[DONE]"})
	defer up.Close()
	s := newTestServer(t, up)
	dir := t.TempDir()
	l := logx.New(dir)
	defer l.Close()
	s.SetLogger(l)

	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	line := "[" + yesterday + " 09:00:00.000] [INFO] {access} rid=yyy chat status=200\n"
	if err := os.WriteFile(filepath.Join(dir, "logs", yesterday+".log"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	s.logCat(logx.CatAccess, "info", "rid=now chat status=200")

	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/admin/api/logs?day=" + yesterday)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Entries []logx.Entry `json:"entries"`
		Days    []string     `json:"days"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if len(out.Entries) != 1 || !strings.Contains(out.Entries[0].Msg, "rid=yyy") {
		t.Errorf("day=%s entries = %+v, want yesterday's file line only", yesterday, out.Entries)
	}
	if len(out.Days) != 2 || out.Days[0] != time.Now().Format("2006-01-02") || out.Days[1] != yesterday {
		t.Errorf("days = %+v, want the retained files newest first for the history picker", out.Days)
	}
	// A day that is not a date must not reach the filesystem: it falls back to
	// the live ring instead of resolving a path.
	bad := logEntries(t, ts.URL, "?day=..%2F..%2Fetc%2Fpasswd&cat=access")
	if len(bad) != 1 || !strings.Contains(bad[0].Msg, "rid=now") {
		t.Errorf("day=../../etc/passwd entries = %+v, want the live ring's access line", bad)
	}
}

// An audit trail you cannot hand to someone is not an audit trail: the same
// filters, as plain text, as a download.
func TestLogsDownloadIsPlainTextWithFilters(t *testing.T) {
	up := fakeUpstream(t, []string{"[DONE]"})
	defer up.Close()
	s := newTestServer(t, up)
	l := wireLog(t, s)
	cats := logx.DefaultCategories()
	cats[logx.CatUpstream] = true
	l.SetCategories(cats)
	s.logCat(logx.CatAccess, "info", "rid=ddd chat status=200")
	s.logCat(logx.CatUpstream, "info", "rid=ddd lane model=x")

	ts := httptest.NewServer(s.mux)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/admin/api/logs/download?cat=access")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	if !strings.Contains(text, "rid=ddd") || strings.Contains(text, "lane model=x") {
		t.Errorf("download body = %q, want only the access lines", text)
	}
	if ct := resp.Header.Get("content-type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain", ct)
	}
	if cd := resp.Header.Get("content-disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("content-disposition = %q, want an attachment", cd)
	}
}
