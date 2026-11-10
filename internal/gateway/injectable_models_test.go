package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// TestInjectableModelsMergesCustomProviders: the adapter-facing model set must
// include enabled custom-provider models as "<providerID>/<model>" (this is
// what puts 自定义 API models into agent pickers), respect the unchecked ids,
// and skip disabled providers. Regression for the NVIDIA-NIM-in-agents gap.
func TestInjectableModelsMergesCustomProviders(t *testing.T) {
	up := fakeUpstream(t, []string{`{"choices":[{"delta":{"content":"x"}}]}`, `data: [DONE]`})
	defer up.Close()
	s := newTestServer(t, up)

	cfg := s.Store.Config()
	cfg.Providers = append(cfg.Providers,
		store.Provider{
			ID: "nvidia-nim", Name: "NVIDIA NIM", BaseURL: "https://integrate.api.nvidia.com/v1",
			Protocol: "openai", Enabled: true,
			// 第一条与免费车道同名（同一份权重，另一个门），第二条撞上被勾掉
			// 的模型，第三条才是真正的新模型。
			Models: []string{"deepseek-ai/deepseek-v4.1-flash", "nvidia/nemotron-3-ultra-550b-a55b", "mistralai/mistral-large-3"},
		},
		store.Provider{
			ID: "sleepy", Name: "Disabled", BaseURL: "https://x.example/v1",
			Protocol: "openai", Enabled: false,
			Models: []string{"hidden-model"},
		},
	)
	cfg.HiddenModels = []string{"nvidia-nim/nvidia/nemotron-3-ultra-550b-a55b"}

	ids := map[string]bool{}
	fams := map[string]int{}
	for _, m := range s.InjectableModels() {
		if ids[m.ID] {
			t.Fatalf("重复模型 %q", m.ID)
		}
		ids[m.ID] = true
		fams[lane.ModelFamily(m.ID)]++
	}
	for fam, n := range fams {
		if n > 1 {
			t.Fatalf("家族 %q 在注入清单里出现 %d 次——同名模型必须合成一条", fam, n)
		}
	}
	if !ids["nvidia-nim/mistralai/mistral-large-3"] {
		t.Fatal("启用供应商的独有模型应进入注入清单")
	}
	// 同一个模型两个门时留下免费车道那条：它会做跨模型故障转移，
	// 供应商那条只是用户自己的 Key。车道这一版的 id 带 -free，家族键把它去掉
	// 了，所以这里判的是「留下的是不是车道那条」，而不是写死某个 id。
	if ids["nvidia-nim/deepseek-ai/deepseek-v4.1-flash"] {
		t.Fatal("同一份权重被列了两次：供应商那条应并入车道那条")
	}
	laneKept := false
	for id := range ids {
		if !strings.Contains(id, "/") && lane.ModelFamily(id) == "deepseek-v4.1-flash" {
			laneKept = true
		}
	}
	if !laneKept {
		t.Fatal("同名时应保留免费车道那条，而不是被供应商条目顶掉")
	}
	if ids["nvidia-nim/nvidia/nemotron-3-ultra-550b-a55b"] {
		t.Fatal("被勾掉的自定义模型不应进入注入清单")
	}
	if ids["sleepy/hidden-model"] {
		t.Fatal("未启用供应商的模型不应进入注入清单")
	}
}

// TestCodexCatalogIncludesCustomModels: the picker's live-refresh endpoint
// must not drown custom-provider models back out.
func TestCodexCatalogIncludesCustomModels(t *testing.T) {
	up := fakeUpstream(t, []string{`{"choices":[{"delta":{"content":"x"}}]}`, `data: [DONE]`})
	defer up.Close()
	s := newTestServer(t, up)

	cfg := s.Store.Config()
	cfg.Providers = append(cfg.Providers, store.Provider{
		ID: "nvidia-nim", Name: "NVIDIA NIM", BaseURL: "https://integrate.api.nvidia.com/v1",
		Protocol: "openai", Enabled: true,
		Models: []string{"mistralai/mistral-large-3"},
	})

	rec := httptest.NewRecorder()
	s.handleCodexCatalog(rec, httptest.NewRequest(http.MethodGet, "/v1/codex-catalog", nil))
	if rec.Code != 200 {
		t.Fatalf("codex catalog status = %d", rec.Code)
	}
	var out struct {
		Models []struct {
			Slug        string `json:"slug"`
			Description string `json:"description"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range out.Models {
		if m.Slug == "nvidia-nim/mistralai/mistral-large-3" {
			found = true
			if !strings.Contains(m.Description, "NVIDIA NIM") {
				t.Fatalf("自定义模型描述应带供应商名，得到 %q", m.Description)
			}
		}
	}
	if !found {
		t.Fatal("codex 在线目录应包含自定义供应商模型")
	}
}
