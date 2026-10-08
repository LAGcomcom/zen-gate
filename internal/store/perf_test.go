package store

import "testing"

func openPerfStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// A free-lane model that queues for two minutes once in a while is still a
// 1.3s model. The number shown next to 首字 (and ranked by the latency
// strategy) must not move because of that one sample.
func TestTTFTStatsReportsTheMedianSample(t *testing.T) {
	st := openPerfStore(t)
	for _, ms := range []int64{1214, 1305, 1290, 1302, 966, 26587, 117959, 79337, 985, 1064} {
		st.AddTTFTSample("busy-model", ms)
	}
	med, count := st.TTFTStats("busy-model")
	if count != 10 {
		t.Fatalf("count = %d, want 10", count)
	}
	if want := int64(1296); med != want {
		t.Errorf("median = %d ms, want %d ms (the mean over these samples is 23200)", med, want)
	}
}

// A 170s queue is a real measurement, not a broken stopwatch. The old 120s
// cut-off kept 118s and dropped 170s, so 首字 and the ring statistic described
// two different populations and could never be reconciled.
func TestAddTTFTSampleKeepsSlowProbes(t *testing.T) {
	st := openPerfStore(t)
	st.AddTTFTSample("queued-model", 169766)
	st.AddTTFTSample("queued-model", 120001)
	if _, count := st.TTFTStats("queued-model"); count != 2 {
		t.Fatalf("count = %d, want 2 (slow probes belong in the ring)", count)
	}
	st.AddTTFTSample("queued-model", 0)
	st.AddTTFTSample("queued-model", -5)
	st.AddTTFTSample("queued-model", 24*3600*1000)
	if _, count := st.TTFTStats("queued-model"); count != 2 {
		t.Fatalf("count = %d after non-samples, want 2", count)
	}
}
