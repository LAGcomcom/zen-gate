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
	Channel            string `json:"channel,omitempty"`   // "" | "kilo" — which free source serves this id
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

// Channel tags for the catalog's free sources. The empty channel is the
// OpenCode Zen lane itself; ChannelKilo is the keyless Kilo free pool.
const ChannelKilo = "kilo"

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
	return capability{vision: false, audio: false, file: false, reasoning: true, contextWindow: 131072, maxOutput: 32768, canDisableThinking: true}
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

// ParseListing accepts {"data":[{"id":…}]}, {"models":[…]} or a bare array.
func ParseListing(payload map[string]any) []string {
	var rows []any
	if d, ok := payload["data"].([]any); ok {
		rows = d
	} else if d, ok := payload["models"].([]any); ok {
		rows = d
	}
	ids := []string{}
	for _, row := range rows {
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
