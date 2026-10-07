package store

import (
	"encoding/json"
	"testing"
	"time"

	"zen-gate/internal/lane"
)

// A day bucket written by an older build has no "agents"/"models" object, so
// unmarshalling leaves those maps nil. Record must still be able to fold a
// call in: it used to panic with "assignment to entry in nil map" and take the
// whole process down as soon as any caller passed an agent label (autotag does).
func TestRecordFoldsIntoLegacyDayBucketWithNilMaps(t *testing.T) {
	legacy := `{"version":1,"days":{"2026-10-07":{"requests":3,"failed":0,"input":10,"output":20}},"recent":[]}`
	var st Stats
	if err := json.Unmarshal([]byte(legacy), &st); err != nil {
		t.Fatalf("legacy stats.json must parse: %v", err)
	}
	day := st.Days["2026-10-07"]
	if day == nil {
		t.Fatal("fixture lost the day bucket")
	}
	if day.Agents != nil || day.Models != nil {
		t.Fatal("fixture must reproduce the nil maps a pre-agents build leaves behind")
	}

	s := &Store{stats: &st}
	at := time.Date(2026, 10, 7, 12, 0, 0, 0, time.Local).UnixMilli()
	s.Record(lane.CallRecord{Model: "intern-s2", Agent: "autotag", Ok: true, Input: 5, Output: 7, At: at})

	if day.Agents["autotag"] != 7 {
		t.Fatalf("agents bucket not accumulated: %#v", day.Agents)
	}
	if day.Models["intern-s2"] != 7 {
		t.Fatalf("models bucket not accumulated: %#v", day.Models)
	}
	if day.ModelReqs["intern-s2"] != 1 {
		t.Fatalf("modelReqs not accumulated: %#v", day.ModelReqs)
	}
	if day.Requests != 4 {
		t.Fatalf("requests = %d, want 4", day.Requests)
	}
}
