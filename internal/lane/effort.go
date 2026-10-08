package lane

import "fmt"

// Reasoning-effort policy, ported from effort.js. A live probe settled the
// design: the gateway accepts reasoning_effort and friends, then ignores them;
// the single enforced control is max_tokens. So a level here is a real,
// enforced output budget.

// Level is one rung of the effort ladder.
type Level struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	ZH      string `json:"zh"`
	Ceiling int    `json:"ceiling"` // 0 = model capacity ("deep")
}

// Levels is ordered for picker display.
var Levels = []Level{
	{ID: "light", Name: "Light", ZH: "精简", Ceiling: 2048},
	{ID: "balanced", Name: "Balanced", ZH: "均衡", Ceiling: 8192},
	{ID: "deep", Name: "Deep", ZH: "深思", Ceiling: 0},
}

// DefaultLevel is the default rung.
const DefaultLevel = "balanced"

// MinBudget is the floor no level may go below.
const MinBudget = 512

// AlwaysThinkingFactor widens every rung for models whose thinking cannot be
// switched off — thinking and the visible answer share one ceiling.
const AlwaysThinkingFactor = 2

// SupportsEffort: does this model expose an effort menu?
func SupportsEffort(m ModelInfo) bool { return m.Reasoning }

// ResolveLevel returns the rung in force for one call.
func ResolveLevel(level string, m ModelInfo) *Level {
	if !SupportsEffort(m) {
		return nil
	}
	for i := range Levels {
		if Levels[i].ID == level {
			return &Levels[i]
		}
	}
	for i := range Levels {
		if Levels[i].ID == DefaultLevel {
			return &Levels[i]
		}
	}
	return nil
}

func usableTokens(v int) int {
	if v > 0 {
		return v
	}
	return int(^uint(0) >> 1) // MaxInt → "no ceiling"
}

// BudgetFor resolves the generation ceiling for one level against one model.
// The level ceiling controls; model capacity and caller request can only
// lower it, never raise it.
func BudgetFor(level string, m ModelInfo, requested, fallback int) int {
	capacity := m.MaxOutput
	if m.MaxOutput <= 0 {
		capacity = 32768
	}
	if r := usableTokens(requested); r < capacity {
		capacity = r
	}
	if f := usableTokens(fallback); f < capacity {
		capacity = f
	}
	lvl := ResolveLevel(level, m)
	if lvl == nil || lvl.Ceiling == 0 {
		return max(MinBudget, capacity)
	}
	ceiling := lvl.Ceiling
	if !m.CanDisableThinking {
		ceiling *= AlwaysThinkingFactor
	}
	return max(MinBudget, min2(ceiling, capacity))
}

// BudgetLadder is the whole ladder as it applies to one model right now.
// defaultLevel marks the rung the user actually configured; an empty value
// falls back to the package default. Without it the ladder's asterisk stays
// on balanced even after the user picks deep — requests do follow the
// setting, only the marker lied.
func BudgetLadder(m ModelInfo, requested, fallback int, defaultLevel string) []LevelBudget {
	if defaultLevel == "" {
		defaultLevel = DefaultLevel
	}
	out := []LevelBudget{}
	for _, lvl := range Levels {
		out = append(out, LevelBudget{
			ID: lvl.ID, Name: lvl.Name, ZH: lvl.ZH,
			Tokens:    BudgetFor(lvl.ID, m, requested, fallback),
			IsDefault: lvl.ID == defaultLevel,
		})
	}
	return out
}

// LevelBudget is one rung with its resolved numbers.
type LevelBudget struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ZH        string `json:"zh"`
	Tokens    int    `json:"tokens"`
	IsDefault bool   `json:"isDefault"`
}

// EffortsFor is the declared effort list for one model, descriptions generated
// from the same BudgetFor call that will decide the request.
func EffortsFor(m ModelInfo, requested, fallback int, defaultLevel string) []LevelBudget {
	if !SupportsEffort(m) {
		return nil
	}
	return BudgetLadder(m, requested, fallback, defaultLevel)
}

// Kilos formats 16384 → "16K".
func Kilos(tokens int) string { return fmt.Sprintf("%dK", (tokens+512)/1024) }

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}
