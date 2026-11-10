package gateway

import (
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"zen-gate/internal/lane"
)

// defaultTokenSaverMaxChars is the per-part ceiling used when the toggle is on
// but the user has not set a size of their own.
const defaultTokenSaverMaxChars = 6000

// tokenSaverLimit resolves the effective per-part character ceiling for one
// request. Zero or a negative value means "let everything through".
func (s *Server) tokenSaverLimit(r *http.Request) int {
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-ZG-Token-Saver")), "off") {
		return 0
	}
	cfg := s.Store.Config()
	if !cfg.TokenSaver {
		return 0
	}
	if cfg.TokenSaverMaxChars > 0 {
		return cfg.TokenSaverMaxChars
	}
	return defaultTokenSaverMaxChars
}

// applyTokenSaver trims oversized tool output before the turn is forwarded.
// The last two messages are left alone: they are the freshest tool result and
// the follow-up the model is about to answer, where truncation hurts most.
func (s *Server) applyTokenSaver(r *http.Request, msgs []lane.Message) []lane.Message {
	limit := s.tokenSaverLimit(r)
	if limit <= 0 {
		return msgs
	}
	return shrinkToolResults(msgs, limit)
}

// shrinkToolResults returns a copy of msgs whose tool payloads are cut down to
// at most max characters each, keeping the head and the tail of long output.
// Parts are value types, so the caller's slice is never mutated.
func shrinkToolResults(msgs []lane.Message, max int) []lane.Message {
	if max <= 0 || len(msgs) <= 2 {
		return msgs
	}
	out := make([]lane.Message, len(msgs))
	copy(out, msgs)
	for i := 0; i < len(out)-2; i++ {
		parts := out[i].Parts
		if len(parts) == 0 {
			continue
		}
		changed := false
		next := make([]lane.Part, len(parts))
		copy(next, parts)
		for j, p := range next {
			switch v := p.(type) {
			case lane.TextPart:
				if trimmed, ok := truncateMiddle(v.Text, max); ok {
					v.Text = trimmed
					next[j] = v
					changed = true
				}
			case lane.ToolResultPart:
				if trimmed, ok := truncateMiddle(v.Text, max); ok {
					v.Text = trimmed
					next[j] = v
					changed = true
				}
			}
		}
		if changed {
			out[i].Parts = next
		}
	}
	return out
}

// truncateMiddle keeps the first 60% and last 25% of a rune count ceiling and
// marks the cut, so the model still sees how output starts and ends. ok is
// false when s already fits, letting callers skip a copy.
func truncateMiddle(s string, max int) (string, bool) {
	if utf8.RuneCountInString(s) <= max {
		return s, false
	}
	r := []rune(s)
	head := max * 6 / 10
	tail := max * 25 / 100
	if head+tail >= len(r) {
		return s, false
	}
	cut := len(r) - head - tail
	return string(r[:head]) + fmt.Sprintf("\n…[truncated %d chars]\n", cut) + string(r[len(r)-tail:]), true
}
