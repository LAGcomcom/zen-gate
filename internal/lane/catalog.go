package lane

import "strings"

// ModelInfo is one catalog entry.
type ModelInfo struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Blurb              string `json:"blurb,omitempty"`     // 一行小介绍：社区评价 + 性能定位 + 推荐度
	Wire               string `json:"wire"`                // chat | responses | messages | systemone
	Channel            string `json:"channel,omitempty"`   // "kilo" | "workbuddy" | "codearts" — which source serves this id
	SystemOne          bool   `json:"systemOne,omitempty"` // decision model, not chat-capable
	Vision             bool   `json:"vision"`
	AudioInput         bool   `json:"audioInput,omitempty"`
	FileInput          bool   `json:"fileInput,omitempty"`
	Reasoning          bool   `json:"reasoning"`
	ContextWindow      int    `json:"contextWindow"`
	MaxOutput          int    `json:"maxOutput"`
	CanDisableThinking bool   `json:"canDisableThinking"`
	RegionSensitive    bool   `json:"regionSensitive"`
}

// Channel tags for the catalog's serving sources. There is no empty-channel
// source any more: the OpenCode Zen lane that used to own it answers
// FreeTierError to every unauthenticated call and is retired. ChannelKilo is
// the keyless Kilo free pool; ChannelWorkBuddy is the WorkBuddy 账号渠道
// (intl); ChannelCodeArts is the Huawei Cloud CodeArts 账号渠道.
const (
	ChannelKilo      = "kilo"
	ChannelWorkBuddy = "workbuddy"
	ChannelCodeArts  = "codearts"
)

// ListingMeta is one upstream model row: its id plus whatever the provider
// itself declares about it. Zero bools mean "not stated" unless the matching
// *Declared flag says otherwise.
type ListingMeta struct {
	ID             string `json:"id"`
	Name           string `json:"name,omitempty"` // the provider's own display name
	ContextWindow  int    `json:"contextWindow,omitempty"`
	MaxOutput      int    `json:"maxOutput,omitempty"`
	Vision         bool   `json:"vision,omitempty"`
	Audio          bool   `json:"audio,omitempty"`
	File           bool   `json:"file,omitempty"`
	Reasoning      bool   `json:"reasoning,omitempty"`
	InputDeclared  bool   `json:"inputDeclared,omitempty"`  // input_modalities were stated
	OutputDeclared bool   `json:"outputDeclared,omitempty"` // output_modalities were stated

	// Efforts are the thinking rungs the provider publishes for this model, in
	// the provider's own naming, lightest first. EffortDefault is whichever of
	// them the provider turns on when the caller states nothing. Both empty
	// means the listing said nothing about thinking levels — the gateway then
	// falls back to its own budget ladder instead of claiming rungs the
	// upstream has never heard of.
	Efforts       []string `json:"efforts,omitempty"`
	EffortDefault string   `json:"effortDefault,omitempty"`
}

// dig walks a decoded-JSON path: a string step is an object key, an int step
// indexes an array. Any missing or mis-typed step yields nil.
func dig(v any, path ...any) any {
	cur := v
	for _, step := range path {
		switch s := step.(type) {
		case string:
			obj, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = obj[s]
		case int:
			list, ok := cur.([]any)
			if !ok || s >= len(list) {
				return nil
			}
			cur = list[s]
		}
		if cur == nil {
			return nil
		}
	}
	return cur
}

func numberAt(v any, path ...any) int {
	if n, ok := dig(v, path...).(float64); ok && n > 0 {
		return int(n)
	}
	return 0
}

func stringAt(v any, path ...any) string {
	s, _ := dig(v, path...).(string)
	return s
}

// firstString returns the first key that holds a non-empty string.
func firstString(row map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := row[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// applyModalitySchema reads the per-modality declaration shape some providers
// publish (schema_version 2.4 style): input_modalities carries the context
// window and the accepted media types, output_modalities carries the output cap
// and whether a reasoning switch exists. Absent the block, nothing is claimed —
// an unlisted modality is then "unknown", not "refused".
func applyModalitySchema(row map[string]any, meta *ListingMeta) {
	if inputs, ok := row["input_modalities"].([]any); ok {
		meta.InputDeclared = true
		for _, item := range inputs {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch strings.ToLower(stringAt(m, "type")) {
			case "image":
				meta.Vision = true
			case "audio":
				meta.Audio = true
			case "file", "document", "pdf":
				meta.File = true
			}
			if n := numberAt(m, "supported_inputs", "max_context_length", "value"); n > 0 && meta.ContextWindow == 0 {
				meta.ContextWindow = n
			}
		}
	}
	outputs, ok := row["output_modalities"].([]any)
	if !ok {
		return
	}
	meta.OutputDeclared = true
	for _, item := range outputs {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if dig(m, "supported_parameters", "reasoning") != nil {
			meta.Reasoning = true
		}
		n := numberAt(m, "max_length", "value")
		if n == 0 {
			n = numberAt(m, "supported_parameters", "max_tokens", "max")
		}
		if n > 0 && meta.MaxOutput == 0 {
			meta.MaxOutput = n
		}
	}
}

// Declared-capacity field names seen in the wild: NVIDIA NIM splits input and
// output caps, OpenRouter publishes one total window plus a completion cap.
// Order is a preference — an input cap is the tighter, more honest bound for
// routing than a viewer-wide token allowance.
var (
	declaredContextKeys = []string{"max_input_tokens", "context_length", "viewer_total_token_limit", "context_window"}
	declaredOutputKeys  = []string{"max_output_tokens", "max_completion_tokens"}
)

func declaredInt(row map[string]any, keys ...string) int {
	for _, k := range keys {
		// encoding/json numbers everything as float64.
		if n, ok := row[k].(float64); ok && n > 0 {
			return int(n)
		}
	}
	return 0
}

// listingRows returns the model rows of a listing payload, or nil when the
// payload carries no recognisable list.
func listingRows(payload map[string]any) []any {
	if d, ok := payload["data"].([]any); ok {
		return d
	}
	if d, ok := payload["models"].([]any); ok {
		return d
	}
	return nil
}

// ParseListingMeta reads the listing keeping each row's declared capacities.
// Rows without an id are dropped; rows without numbers are kept, so a caller
// can tell "the provider published nothing" apart from "the provider was
// unreachable".
func ParseListingMeta(payload map[string]any) []ListingMeta {
	rows := []ListingMeta{}
	for _, row := range listingRows(payload) {
		switch t := row.(type) {
		case string:
			if id := strings.TrimSpace(t); id != "" {
				rows = append(rows, ListingMeta{ID: id})
			}
		case map[string]any:
			id, _ := t["id"].(string)
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			rows = append(rows, ListingMeta{
				ID:            id,
				Name:          firstString(t, "name", "display_name"),
				ContextWindow: declaredInt(t, declaredContextKeys...),
				MaxOutput:     declaredInt(t, declaredOutputKeys...),
			})
			applyModalitySchema(t, &rows[len(rows)-1])
		}
	}
	return rows
}

// ParseListing accepts {"data":[{"id":…}]}, {"models":[…]} or a bare array.
// It stays because provider listings (and the /v1/codex-catalog sidecar) are
// still parsed through it; the Zen roster that first used it is retired.
func ParseListing(payload map[string]any) []string {
	ids := []string{}
	for _, row := range listingRows(payload) {
		switch t := row.(type) {
		case string:
			ids = append(ids, t)
		case map[string]any:
			if id, ok := t["id"].(string); ok && id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}
