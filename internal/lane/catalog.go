package lane

import (
	"context"
	"regexp"
	"strings"
)

// ModelInfo is one catalog entry.
type ModelInfo struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Blurb              string `json:"blurb,omitempty"`     // 一行小介绍：社区评价 + 性能定位 + 推荐度
	Wire               string `json:"wire"`                // chat | responses | messages | systemone
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

var alwaysFree = map[string]bool{"union-alpha": true, "space-bunny-free": true}

var freeLaneRe = regexp.MustCompile(`(?:^|[-_])free(?:$|[-_.])`)

// IsFreeLane: is this id on the free lane? The gateway listing mixes paid and
// free ids; only these answer without a per-user key.
func IsFreeLane(modelID string) bool {
	base := BaseModelId(modelID)
	if alwaysFree[base] {
		return true
	}
	return freeLaneRe.MatchString(base)
}

type capability struct {
	match              *regexp.Regexp
	vision             bool
	audio              bool
	file               bool
	reasoning          bool
	contextWindow      int
	maxOutput          int
	canDisableThinking bool
}

// capabilities is the local baseline: contextWindow/maxOutput are the
// provider's published capacities (checked against official model pages and
// reviews, kept in sync with the blurbs below); vision is what the lane
// actually accepted under a direct probe, not what a model card claims.
// audio/file stay false until verified (a live probe or the AI tagger upgrades
// them) — an unverified modality must never receive that modality's traffic.
var capabilities = []capability{
	{regexp.MustCompile(`^mimo.*v2\.6`), true, false, false, true, 1048576, 131072, false},
	{regexp.MustCompile(`^mimo.*v2\.5`), true, false, false, true, 1048576, 131072, false},
	{regexp.MustCompile(`^mimo`), true, false, false, true, 262144, 131072, true},
	{regexp.MustCompile(`^muse.?spark`), true, false, false, true, 1048576, 131072, true},
	{regexp.MustCompile(`^nemotron`), false, false, false, true, 128000, 32768, true},
	{regexp.MustCompile(`^ling`), false, false, false, true, 262144, 32768, true},
	{regexp.MustCompile(`^space.?bunny`), true, false, false, true, 1048576, 65536, true},
	{regexp.MustCompile(`^union`), true, false, false, false, 262144, 131072, true},
	{regexp.MustCompile(`^deepseek`), false, false, false, true, 1048576, 65536, true},
	{regexp.MustCompile(`^longcat`), true, false, false, true, 1048576, 32768, true},
	{regexp.MustCompile(`^fledge`), false, false, false, true, 131072, 32768, true},
	{regexp.MustCompile(`^jev`), false, false, false, false, 32768, 4096, true},
}

var regionSensitiveRes = []*regexp.Regexp{regexp.MustCompile(`^muse.?spark`)}

var displayNames = map[string]string{
	"mimo-v2.6-flash-free":            "MiMo V2.6 Flash",
	"mimo-v2.5-free":                  "MiMo V2.5",
	"muse-spark-1.3-contributor-free": "Muse Spark 1.3",
	"muse-spark-1.2-contributor-free": "Muse Spark 1.2",
	"nemotron-3-ultra-free":           "Nemotron 3 Ultra",
	"nemotron-3.5-lightning-free":     "Nemotron 3.5 Lightning",
	"ling-3.0-flash-fin-free":         "Ling 3.0 Flash Fin",
	"space-bunny-free":                "Space Bunny",
	"union-alpha":                     "Union Alpha",
	"deepseek-v4-flash-free":          "DeepSeek V4 Flash",
	"jev-1.13-free":                   "Jev 1.13",
}

// blurbs are curated one-liners (community reviews + benchmarks, GPT series as
// the reference point) shown on the model page. Sources: Artificial Analysis
// 智能指数, Code Arena, SemiAnalysis, r/opencode, opencode.ai 用量榜.
var blurbs = map[string]string{
	"mimo-v2.6-flash-free":            "小米开源王牌：开源权重智能指数第一、Code Arena 前十，整体逼近 GPT-5 级主力，1M 上下文。综合推荐 ★★★★★（默认主力）",
	"mimo-v2.5-free":                  "小米上一代旗舰，1M 上下文、agent 稳；能力在 GPT-4.5~5 之间，已被 V2.6 全面接替。推荐 ★★★（备用）",
	"muse-spark-1.3-contributor-free": "Meta 编程特化模型，官方对标 GPT-5.6 Sol，编码上限高；但 CN 出口实测被地区门拦。推荐 ★★★★（有海外出口再开）",
	"muse-spark-1.2-contributor-free": "Muse Spark 上一代编程模型，同为 GPT-5 级定位；CN 出口地区受限。推荐 ★★★",
	"nemotron-3-ultra-free":           "NVIDIA 开源旗舰，口碑平平（SemiAnalysis 认为逊于中国开源第一梯队），约 GPT-4.5 水平；国内直连最稳。推荐 ★★★（稳定备选）",
	"nemotron-3.5-lightning-free":     "Nemotron 3.5 轻快版，适合快问快答与短任务，约 GPT-4.5 级。推荐 ★★★",
	"ling-3.0-flash-fin-free":         "蚂蚁 Ling 3.0 Flash：256K 上下文、开源权重，轻快可靠，能力约 GPT-4.5~5 之间。推荐 ★★★★",
	"ling-3.1-flash-free":             "蚂蚁 Ling 3.1 迭代版，同系轻快路线，日常任务顺滑。推荐 ★★★★",
	"space-bunny-free":                "匿名 1M 上下文模型，OpenCode 用量榜第一（周 63T tokens）、社区口碑好，来头未公开。推荐 ★★★★☆",
	"union-alpha":                     "r/opencode 社区常推的免费模型，综合约 GPT-4.5+ 水平。推荐 ★★★★",
	"deepseek-v4-flash-free":          "DeepSeek V4 Flash：速度优先，首字快 40-60%、输出 ~214 tok/s，1M 上下文，官方称体验全面超越自家 Pro，综合对位 GPT-5 级。推荐 ★★★★★（速度之王）",
	"longcat-2.5-preview-free":        "美团 LongCat 2.5 预览：万亿级动态稀疏 MoE、百万上下文，国产算力全流程训练。推荐 ★★★★",
	"fledge-alpha-free":               "新上架的神秘面孔，社区评价还少；先按 GPT-4.5 级试水。推荐 ★★★（尝鲜）",
	"jev-1.13-free":                   "TypeSafe AI 的 System One 判定模型：只答 choice/score/noul 三类结构化决策，不做内容生成，给 Agent 当意图/情绪判定层。推荐 ★★（别拿来聊天）",
}

// DisplayModelName turns a bare upstream id into something a picker can show.
func DisplayModelName(modelID string) string {
	base := BaseModelId(modelID)
	if n, ok := displayNames[base]; ok {
		return n
	}
	words := strings.FieldsFunc(base, func(r rune) bool {
		return r == '-' || r == '_' || r == '.'
	})
	for i, w := range words {
		if len(w) > 0 && !(w[0] >= '0' && w[0] <= '9') {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

func capabilitiesFor(base string) capability {
	for _, c := range capabilities {
		if c.match.MatchString(base) {
			return c
		}
	}
	// Nothing is known about this model, so nothing is claimed: a guessed
	// reasoning:true showed up on the model page as a verified 思考 badge.
	return capability{vision: false, audio: false, file: false, reasoning: false, contextWindow: 131072, maxOutput: 32768, canDisableThinking: true}
}

// CapabilityMatched reports whether the local table states anything about a
// model — false means capabilitiesFor() answered with its defaults.
func CapabilityMatched(base string) bool {
	for _, c := range capabilities {
		if c.match.MatchString(base) {
			return true
		}
	}
	return false
}

// BuildCatalog merges the upstream listing with the local capability table,
// keeping listing order and deduplicating by base id.
func BuildCatalog(ids []string) []ModelInfo {
	seen := map[string]bool{}
	entries := []ModelInfo{}
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" || !IsFreeLane(id) {
			continue
		}
		base := BaseModelId(id)
		if seen[base] {
			continue
		}
		seen[base] = true
		caps := capabilitiesFor(base)
		wire := "chat"
		if WireFor(base) != "chat" {
			wire = WireFor(base)
		}
		regionSensitive := false
		for _, re := range regionSensitiveRes {
			if re.MatchString(base) {
				regionSensitive = true
			}
		}
		entries = append(entries, ModelInfo{
			ID:                 base,
			Name:               DisplayModelName(base),
			Blurb:              blurbs[base],
			Wire:               wire,
			SystemOne:          wire == "systemone",
			Vision:             caps.vision,
			AudioInput:         caps.audio,
			FileInput:          caps.file,
			Reasoning:          caps.reasoning,
			ContextWindow:      caps.contextWindow,
			MaxOutput:          caps.maxOutput,
			CanDisableThinking: caps.canDisableThinking,
			RegionSensitive:    regionSensitive,
		})
	}
	return entries
}

// FallbackCatalogIDs covers a cold start with no network.
var FallbackCatalogIDs = []string{
	"mimo-v2.6-flash-free", "mimo-v2.5-free", "ling-3.0-flash-fin-free",
	"nemotron-3-ultra-free", "nemotron-3.5-lightning-free", "space-bunny-free",
	"muse-spark-1.3-contributor-free", "muse-spark-1.2-contributor-free",
}

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

// FetchListing pulls the authoritative id set from the gateway.
func FetchListing(ctx context.Context) ([]string, error) {
	var payload map[string]any
	err := GetJSON(ctx, "/zen/v1/models", SessionForConversation("catalog:zen-gate"), MintRequestId(0), &payload)
	if err != nil {
		return nil, err
	}
	return ParseListing(payload), nil
}
