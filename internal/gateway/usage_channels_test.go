package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zen-gate/internal/lane"
)

// The wide usage.csv keeps its one-row-per-day shape; the channel ledger is a
// separate long table so a spreadsheet can pivot on supplier without losing
// the existing model columns.
func TestUsageChannelsCSVExportsPerSupplierTokenLedger(t *testing.T) {
	s, _, _ := newBareServer(t)
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	at := time.Now().UnixMilli()
	s.Store.Record(lane.CallRecord{Model: "m", Channel: "zen", Ok: true,
		Input: 100, Output: 20, CacheRead: 80, At: at})
	s.Store.Record(lane.CallRecord{Model: "seed/m", Channel: "seed", Ok: false,
		Input: 5, Output: 0, At: at})

	resp, err := http.Get(ts.URL + "/admin/api/usage/channels.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	day := time.UnixMilli(at).Format("2006-01-02")
	for _, want := range []string{
		"date,channel,requests,failed,input,cacheRead,cacheWrite,output,total",
		day + ",seed,1,1,5,0,0,0,5",
		day + ",zen,1,0,100,80,0,20,120",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("CSV missing %q:\n%s", want, text)
		}
	}
	// Channels are sorted by id within a day, so the export is stable.
	if strings.Index(text, ",seed,") > strings.Index(text, ",zen,") {
		t.Errorf("channel rows not sorted:\n%s", text)
	}
}
