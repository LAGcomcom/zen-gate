package agents

import (
	"testing"

	"zen-gate/internal/lane"
)

// ZCode overlays its own built-in metadata for every key the rule omits
// (ModelInputFormatConfig.overlay). For a model its catalogue has never heard
// of there is nothing to overlay, so an omitted inputFormat leaves the client
// believing the model is text-only — a pasted screenshot is silently dropped.
//
// Measured on a real install: muse-spark-1.3 and stepfun/step-5 both answer
// images through the gateway ("input_modalities":["text","image"]) yet arrived
// in ZCode without supportsImage, while models ZCode happens to know (mimo,
// space-bunny, longcat) got the field from its own catalogue. That asymmetry is
// the bug: whether images survive depended on ZCode's private model list.
func TestZCodeRuleStatesTheWholeInputFormat(t *testing.T) {
	models := []lane.ModelInfo{
		{ID: "vision-model", ContextWindow: 1000, Vision: true},
		{ID: "text-model", ContextWindow: 1000, Vision: false},
		{ID: "audio-model", ContextWindow: 1000, Vision: false, AudioInput: true},
		{ID: "pdf-model", ContextWindow: 1000, Vision: false, FileInput: true},
	}
	for _, m := range models {
		props := rulePropertiesFor(m)
		raw, ok := props["inputFormat"]
		if !ok {
			t.Fatalf("%s: inputFormat missing — ZCode would fill it from its own catalogue", m.ID)
		}
		fmtMap, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("%s: inputFormat is not an object", m.ID)
		}
		// ZCode's schema is .strict() over exactly these five keys.
		want := []string{"supportsText", "supportsImage", "supportsVideo", "supportsAudio", "supportsPdf"}
		for _, k := range want {
			if _, present := fmtMap[k]; !present {
				t.Errorf("%s: inputFormat.%s not stated", m.ID, k)
			}
		}
		if len(fmtMap) != len(want) {
			t.Errorf("%s: inputFormat carries %d keys, the schema allows exactly %d", m.ID, len(fmtMap), len(want))
		}
		if fmtMap["supportsText"] != true {
			t.Errorf("%s: supportsText should be true", m.ID)
		}
		if fmtMap["supportsImage"] != m.Vision {
			t.Errorf("%s: supportsImage = %v, want %v", m.ID, fmtMap["supportsImage"], m.Vision)
		}
		if fmtMap["supportsAudio"] != m.AudioInput {
			t.Errorf("%s: supportsAudio = %v, want %v", m.ID, fmtMap["supportsAudio"], m.AudioInput)
		}
		if fmtMap["supportsPdf"] != m.FileInput {
			t.Errorf("%s: supportsPdf = %v, want %v", m.ID, fmtMap["supportsPdf"], m.FileInput)
		}
		// Video input has no probe behind it, so it must never be advertised.
		if fmtMap["supportsVideo"] != false {
			t.Errorf("%s: supportsVideo = %v, want false (unverified)", m.ID, fmtMap["supportsVideo"])
		}
	}
}
