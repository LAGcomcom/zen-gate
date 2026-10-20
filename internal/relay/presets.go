package relay

import (
	"regexp"
	"strings"

	"zen-gate/internal/store"
)

// Preset is a curated free (or free-tier) upstream the dashboard offers as a
// one-click fill for the 自定义 API form. The user still supplies their own
// key — nothing here embeds credentials. Endpoints verified against the
// providers' docs and the community-maintained list
// github.com/cheahjs/free-llm-api-resources (2026-10).
type Preset struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	BaseURL  string `json:"baseUrl"`
	Protocol string `json:"protocol"`
	Note     string `json:"note"`   // free-tier shape, shown under the chip
	KeyURL   string `json:"keyUrl"` // where to apply for the key
}

// Presets is the catalog the dashboard renders. NVIDIA NIM leads (the user's
// ask); AMD is absent on purpose — AMD ships $100 Developer Cloud GPU credits
// rather than a hosted free API, so there is no endpoint to point at.
var Presets = []Preset{
	{ID: "nvidia-nim", Name: "NVIDIA NIM", BaseURL: "https://integrate.api.nvidia.com/v1",
		Protocol: store.ProtocolOpenAI,
		Note:     "注册即送免费额度（约 40 次/分钟），几百个开源模型：deepseek-ai/、qwen/、meta/、nvidia/ 前缀",
		KeyURL:   "https://build.nvidia.com"},
	{ID: "gemini", Name: "Google AI Studio (Gemini)", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
		Protocol: store.ProtocolOpenAI,
		Note:     "免费档，按模型限速；gemini-2.5-flash 等日常够用",
		KeyURL:   "https://aistudio.google.com/apikey"},
	{ID: "github-models", Name: "GitHub Models", BaseURL: "https://models.github.ai/inference",
		Protocol: store.ProtocolOpenAI,
		Note:     "GitHub PAT 就是 Key，免费按模型限速；模型 ID 形如 openai/gpt-4.1-mini",
		KeyURL:   "https://github.com/settings/tokens"},
	{ID: "groq", Name: "Groq", BaseURL: "https://api.groq.com/openai/v1",
		Protocol: store.ProtocolOpenAI,
		Note:     "免费档速度极快（LPU），llama-3.3-70b 等开源模型",
		KeyURL:   "https://console.groq.com/keys"},
	{ID: "cerebras", Name: "Cerebras", BaseURL: "https://api.cerebras.ai/v1",
		Protocol: store.ProtocolOpenAI,
		Note:     "免费档， waive-scale 推理速度，llama/qwen 开源模型",
		KeyURL:   "https://cloud.cerebras.ai"},
	{ID: "mistral", Name: "Mistral", BaseURL: "https://api.mistral.ai/v1",
		Protocol: store.ProtocolOpenAI,
		Note:     "免费实验档（1 req/s），mistral-large / codestral",
		KeyURL:   "https://console.mistral.ai/api-keys"},
	{ID: "openrouter", Name: "OpenRouter", BaseURL: "https://openrouter.ai/api/v1",
		Protocol: store.ProtocolOpenAI,
		Note:     "聚合网关，模型 ID 带 :free 后缀的免费（如 deepseek/deepseek-r1:free）",
		KeyURL:   "https://openrouter.ai/keys"},
	{ID: "huggingface", Name: "HuggingFace Router", BaseURL: "https://router.huggingface.co/v1",
		Protocol: store.ProtocolOpenAI,
		Note:     "每月赠送推理额度，路由到各家托管开源模型",
		KeyURL:   "https://huggingface.co/settings/tokens"},
	{ID: "siliconflow", Name: "硅基流动 SiliconFlow", BaseURL: "https://api.siliconflow.cn/v1",
		Protocol: store.ProtocolOpenAI,
		Note:     "国内直连，Qwen 小杯、GLM-4-9B 等常驻免费",
		KeyURL:   "https://cloud.siliconflow.cn/account/ak"},
	{ID: "modelscope", Name: "魔搭 ModelScope", BaseURL: "https://api-inference.modelscope.cn/v1",
		Protocol: store.ProtocolOpenAI,
		Note:     "国内直连，每日 2000 次免费推理，开源全家桶",
		KeyURL:   "https://modelscope.cn/my/myaccesstoken"},
	{ID: "intern-ai", Name: "书生·端砚 Intern", BaseURL: "https://discovery-api.intern-ai.org.cn/v1",
		Protocol: store.ProtocolOpenAI,
		Note:     "上海人工智能实验室开放平台，国内直连免费车道：DeepSeek-V4、GLM-5.3、Kimi-K2.6、书生-S2 等十来个开源旗舰，支持视觉与工具调用",
		KeyURL:   "https://discovery.intern-ai.org.cn"},
	{ID: "zhipu", Name: "智谱 BigModel", BaseURL: "https://open.bigmodel.cn/api/paas/v4",
		Protocol: store.ProtocolOpenAI,
		Note:     "国内直连，GLM-4.5-Flash 等 Flash 系免费",
		KeyURL:   "https://open.bigmodel.cn/usercenter/apikeys"},
	{ID: "longcat", Name: "LongCat（美团）", BaseURL: "https://api.longcat.chat/openai/v1",
		Protocol: store.ProtocolOpenAI,
		Note:     "注册即用，LongCat-Flash 系列，1M 上下文",
		KeyURL:   "https://longcat.chat"},
	{ID: "longcat-anthropic", Name: "LongCat（Anthropic 协议）", BaseURL: "https://api.longcat.chat/anthropic",
		Protocol: store.ProtocolAnthropic,
		Note:     "同一家，走 Anthropic /messages 协议，可喂给 Claude Code 类客户端",
		KeyURL:   "https://longcat.chat"},
	{ID: "anthropic", Name: "Anthropic 官方", BaseURL: "https://api.anthropic.com/v1",
		Protocol: store.ProtocolAnthropic,
		Note:     "付费（演示 Anthropic 协议接法），claude-opus-4-6 等",
		KeyURL:   "https://console.anthropic.com/settings/keys"},
	{ID: "opencode-go", Name: "OpenCode Go", BaseURL: "https://opencode.ai/inference/go/v1",
		Protocol: store.ProtocolOpenAI,
		Note:     "OpenCode 官方付费档（月度订阅，需在 OpenCode 控制台取 Key）；网关会自动补上它要求的会话头，Claude Code / Codex 等客户端可直连",
		KeyURL:   "https://opencode.ai/docs/go/"},
}

// recommendPatterns lists, per provider, the model-id patterns worth enabling
// by default — curated from hands-on testing (NVIDIA free lane, Oct 2026:
// DeepSeek V4.1-Flash / Nemotron 3 Super / Nemotron 3 Ultra answer correctly
// at usable speeds; Kimi K3 and GLM-5.3 answer after 3-4 minute queues and are
// left out) and the provider's own free tier (Zhipu: the 4.x flash family).
var recommendPatterns = map[string][]string{
	"nvidia-nim": {
		"deepseek-ai/deepseek-v4.1-flash",
		"nvidia/nemotron-3-super",
		"nvidia/nemotron-3-ultra",
	},
	"integrate.api.nvidia.com": {
		"deepseek-ai/deepseek-v4.1-flash",
		"nvidia/nemotron-3-super",
		"nvidia/nemotron-3-ultra",
	},
	"zhipu": {
		"glm-4-flash", "glm-4.5-flash", "glm-4v-flash", "glm-4.1v-thinking-flash",
	},
	"open.bigmodel.cn": {
		"glm-4-flash", "glm-4.5-flash", "glm-4v-flash", "glm-4.1v-thinking-flash",
	},
	// SiliconFlow's free tier (verified Oct 2026 against a zero-balance key):
	// the Qwen small family answers for free; GLM-4-9B-0414, Hunyuan, Ling and
	// everything Pro/ are paid.
	"siliconflow": {
		"Qwen/Qwen2.5-7B-Instruct", "Qwen/Qwen3-8B", "Qwen/Qwen3-14B", "Qwen/Qwen3-32B",
	},
	"api.siliconflow.cn": {
		"Qwen/Qwen2.5-7B-Instruct", "Qwen/Qwen3-8B", "Qwen/Qwen3-14B", "Qwen/Qwen3-32B",
	},
	// ModelScope free inference (verified Oct 2026): the flagship tier answers
	// fast — DeepSeek-V4-Pro 1.5s, GLM-5.2 1.2s, Qwen3.5-397B 3.5s. MiniMax-M3
	// / ERNIE / GLM-4.7-Flash / LongCat-Lite returned empty deployments.
	"modelscope": {
		"deepseek-ai/DeepSeek-V4-Pro", "deepseek-ai/DeepSeek-V4.1-Flash",
		"ZhipuAI/GLM-5.2", "Qwen/Qwen3.5-397B", "Qwen/Qwen3.5-122B",
		"Qwen/Qwen3.8-Flash-Next", "stepfun-ai/Step-3.7-Flash",
	},
	"api-inference.modelscope.cn": {
		"deepseek-ai/DeepSeek-V4-Pro", "deepseek-ai/DeepSeek-V4.1-Flash",
		"ZhipuAI/GLM-5.2", "Qwen/Qwen3.5-397B", "Qwen/Qwen3.5-122B",
		"Qwen/Qwen3.8-Flash-Next", "stepfun-ai/Step-3.7-Flash",
	},
	// Intern dispatch API (verified Oct 2026 against a live key): every model
	// answers in under 7s first-token — deepseek-v4-pro 0.9s, intern-s2 1.3s,
	// glm-5.3 1.9s, qwen3.8-27b 0.5s, deepseek-v4-flash-vision 0.4s.
	// kimi-k2.6 is the slowest (6.6s) but worth the default tick; the Atria
	// mystery tier and Agents-A1 stay opt-in.
	"intern-ai": {
		"deepseek-v4-pro", "deepseek-v4-flash", "glm-5.3", "intern-s2",
		"kimi-k2.6", "qwen3.8-27b", "minimax-m3",
	},
	"discovery-api.intern-ai.org.cn": {
		"deepseek-v4-pro", "deepseek-v4-flash", "glm-5.3", "intern-s2",
		"kimi-k2.6", "qwen3.8-27b", "minimax-m3",
	},
}

// junkRe filters non-chat entries (embedders, safety nets, parsers, …) that
// clutter big catalogs like NVIDIA NIM's.
var junkRe = regexp.MustCompile(`(?i)embed|rerank|retriev|guard|reward|parse|whisper|image|diffusion|ocr|safety|topic-control|moderation|tts|speech|transcri|clip`)

// smallRe filters sub-33B dense models and small MoE (太落后太低端的不要).
// Anchors keep "120b-a12b" and "a12b" from matching the 12b alternative.
var smallRe = regexp.MustCompile(`(?i)(^|[^a-z0-9])(1|2|3|4|6\.7|7|8|9|11|12|14|20|22|24|30|31|32)b([^a-z0-9]|$)`)

// oldRe filters 2023-2024 vintage families.
var oldRe = regexp.MustCompile(`(?i)llama-?2|codellama|chatqa|mixtral|codestral|mistral-7b|mistral-nemo|minitron|recurrentgemma|deepseek-coder|llama-3\.[12]|nemotron-4-340b|gemma-2\b`)

// RecommendedModels picks the model ids a provider should enable by default:
// a curated list for known providers (matched by preset id or base-URL host,
// with word-boundary matching so glm-4-flash never drags in paid glm-4-flashx),
// falling back to a junk/small/vintage heuristic for unknown catalogs. The
// result never contains ids outside the fetched list.
func RecommendedModels(presetID, baseURL string, models []string) []string {
	pats := recommendPatterns[strings.ToLower(strings.TrimSpace(presetID))]
	if pats == nil {
		if host := hostOf(baseURL); host != "" {
			pats = recommendPatterns[strings.ToLower(host)]
		}
	}
	if pats != nil {
		var out []string
		for _, m := range models {
			for _, pat := range pats {
				if matchPattern(m, pat) {
					out = append(out, m)
					break
				}
			}
		}
		if len(out) > 0 {
			return out
		}
		// curated list matched nothing (catalog rotated) → heuristic below
	}
	out := []string{}
	for _, m := range models {
		if junkRe.MatchString(m) || smallRe.MatchString(m) || oldRe.MatchString(m) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// matchPattern reports whether id equals pat or extends it on a word boundary
// ("glm-4-flash" matches "glm-4-flash-250414" but not "glm-4-flashx").
func matchPattern(id, pat string) bool {
	id, pat = strings.ToLower(strings.TrimSpace(id)), strings.ToLower(strings.TrimSpace(pat))
	if id == pat {
		return true
	}
	if !strings.HasPrefix(id, pat) || len(id) == len(pat) {
		return false
	}
	switch id[len(pat)] {
	case '-', '_', '.', ':', '@', '/':
		return true
	}
	return false
}

func hostOf(baseURL string) string {
	u := NormalizeBaseURL(baseURL)
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.Index(u, "/"); i >= 0 {
		u = u[:i]
	}
	return strings.ToLower(u)
}
