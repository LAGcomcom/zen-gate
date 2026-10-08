package gateway

// Live capability probe (实测): send the smallest possible image / audio /
// PDF turn through the user's own provider key and record what the model
// actually accepts. A probe verdict outranks the AI tagger and the name
// heuristics — the project's rule is "what the upstream accepted", not what
// a model card claims.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	"zen-gate/internal/lane"
	"zen-gate/internal/logx"
	"zen-gate/internal/relay"
	"zen-gate/internal/store"
)

// probePNG is a 1×1 transparent PNG — the smallest legal image.
const probePNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// tinyWAV builds 0.1s of silent mono 8 kHz 16-bit PCM as a valid WAV.
func tinyWAV() []byte {
	const sampleRate = 8000
	const samples = 800
	dataLen := samples * 2
	buf := &bytes.Buffer{}
	buf.WriteString("RIFF")
	_ = binary.Write(buf, binary.LittleEndian, uint32(36+dataLen))
	buf.WriteString("WAVEfmt ")
	_ = binary.Write(buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(buf, binary.LittleEndian, uint16(1)) // PCM
	_ = binary.Write(buf, binary.LittleEndian, uint16(1)) // mono
	_ = binary.Write(buf, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(buf, binary.LittleEndian, uint32(sampleRate*2))
	_ = binary.Write(buf, binary.LittleEndian, uint16(2))  // block align
	_ = binary.Write(buf, binary.LittleEndian, uint16(16)) // bits
	buf.WriteString("data")
	_ = binary.Write(buf, binary.LittleEndian, uint32(dataLen))
	buf.Write(make([]byte, dataLen))
	return buf.Bytes()
}

// tinyPDF assembles a minimal one-page PDF (one word, real xref offsets).
func tinyPDF() []byte {
	content := "BT /F1 12 Tf 20 100 Td (OK) Tj ET"
	objs := []string{
		"<</Type/Catalog/Pages 2 0 R>>",
		"<</Type/Pages/Kids[3 0 R]/Count 1>>",
		"<</Type/Page/Parent 2 0 R/MediaBox[0 0 200 200]/Contents 4 0 R/Resources<</Font<</F1 5 0 R>>>>>>",
		fmt.Sprintf("<</Length %d>>\nstream\n%s\nendstream", len(content), content),
		"<</Type/Font/Subtype/Type1/BaseFont/Helvetica>>",
	}
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, body := range objs {
		offsets[i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}
	xrefAt := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(objs)+1)
	buf.WriteString("0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<</Size %d/Root 1 0 R>>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xrefAt)
	return buf.Bytes()
}

// probeVerdict is one modality's probe outcome.
const (
	verdictYes     = "yes"
	verdictNo      = "no"
	verdictUnknown = "unknown"
)

// probeOneModality sends one modality's minimal turn through the provider.
// "yes" = the upstream answered; "no" = it rejected the modality itself
// (400-class model error); "unknown" = anything else (quota, transport…) —
// an inconclusive probe must never be recorded as a capability verdict. The
// second return is what the upstream said, for the drawer.
func probeOneModality(ctx context.Context, p *store.Provider, model string, part lane.Part) (verdict, detail string) {
	msgs := []lane.Message{{Role: lane.RoleUser, Parts: []lane.Part{
		part, lane.TextPart{Text: "Reply with exactly: OK"}}}}
	var sb strings.Builder
	_, _, uerr := relay.Complete(ctx, relay.Turn{
		Provider: p, Model: model, Messages: msgs, MaxTokens: 16, Agent: "capsprobe",
	}, func(c lane.Chunk) {
		if c.Kind == lane.ChunkTextDelta {
			sb.WriteString(c.Delta)
		}
	})
	if uerr == nil {
		answer := strings.TrimSpace(sb.String())
		if len(answer) > 120 {
			answer = answer[:120] + "…"
		}
		return verdictYes, answer
	}
	if uerr.Code == lane.CodeServer && uerr.Unavailable {
		return verdictNo, uerr.Message
	}
	return verdictUnknown, uerr.Message
}

// probeCapabilities runs all three modality probes for one custom-provider
// model. The tag is only stored when every probe reached a verdict — a
// half-verdict would hard-filter modalities the model may well support.
// Each modality reports a start and an answer phase, so the card can show
// 图像 ✓ / 音频 … / 文件 ─ while the run is still going.
func (s *Server) probeCapabilities(ctx context.Context, p *store.Provider, model string) (verdicts map[string]string) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	verdicts = map[string]string{}
	steps := []struct {
		key   string
		label string
		part  lane.Part
	}{
		{"vision", "图像", lane.ImagePart{DataURL: "data:image/png;base64," + probePNG}},
		{"audio", "音频", lane.AudioPart{Data: base64.StdEncoding.EncodeToString(tinyWAV()), Format: "wav"}},
		{"file", "文件", lane.FilePart{Name: "probe.pdf", MediaType: "application/pdf", Data: base64.StdEncoding.EncodeToString(tinyPDF())}},
	}
	for _, step := range steps {
		lane.EmitProbeStep(ctx, step.label, lane.StepRunning, 0, "")
		start := time.Now()
		verdict, detail := probeOneModality(ctx, p, model, step.part)
		ms := time.Since(start).Milliseconds()
		if ms <= 0 {
			ms = 1
		}
		lane.EmitProbeStep(ctx, step.label, stepStatusForVerdict(verdict), ms, detail)
		verdicts[step.key] = verdict
		s.logCat(logx.CatProbe, "info", "能力实测 %s [%s] → %s", model, step.key, verdict)
	}
	for _, v := range verdicts {
		if v == verdictUnknown {
			return verdicts
		}
	}
	// The probe answers the modality questions only: whatever the previous
	// source said about 思考 has to survive, or a 能力实测 run would silently
	// downgrade the model to plain text.
	prev, _ := s.Store.ModelTagOf(model)
	s.Store.SetModelTag(model, store.ModelTag{
		Vision:    verdicts["vision"] == verdictYes,
		Audio:     verdicts["audio"] == verdictYes,
		File:      verdicts["file"] == verdictYes,
		Reasoning: prev.Reasoning,
		Source:    store.TagSourceProbe, At: time.Now().UnixMilli(),
	})
	return verdicts
}

// stepStatusForVerdict colours one modality's phase: a rejection is a real
// answer about the model, a quota or transport fault is not.
func stepStatusForVerdict(verdict string) string {
	switch verdict {
	case verdictYes:
		return lane.StepOK
	case verdictNo:
		return lane.StepFail
	default:
		return lane.StepUnknown
	}
}
