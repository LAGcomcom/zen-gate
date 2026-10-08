package store

import "testing"

func TestDeclaredMetaCarriesTheListingSchema(t *testing.T) {
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	st.Config().Providers = []Provider{{ID: "nim", Name: "N", Enabled: true,
		Models: []string{"glm-5.3"},
		ModelMeta: map[string]ModelMeta{"glm-5.3": {
			Name: "GLM-5.3", ContextWindow: 1048576, MaxOutput: 1048576,
			Reasoning: true, InputDeclared: true, OutputDeclared: true,
		}}}}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.DeclaredMeta("glm-5.3")
	if !ok {
		t.Fatal("declared meta lost")
	}
	if got.Name != "GLM-5.3" || !got.Reasoning || !got.InputDeclared || !got.OutputDeclared {
		t.Errorf("listing schema fields lost across restart: %+v", got)
	}
}

func TestDeclaredMetaServesListingNumbers(t *testing.T) {
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	st.Config().Providers = []Provider{
		{ID: "nim", Name: "NVIDIA NIM", Enabled: true, Models: []string{"nvidia/m1", "nvidia/m2"},
			ModelMeta: map[string]ModelMeta{
				"nvidia/m1": {ContextWindow: 131072, MaxOutput: 4096},
				"nvidia/m2": {}, // the listing carried no numbers for this one
			}},
		{ID: "off", Name: "Disabled", Enabled: false, Models: []string{"ghost/m3"},
			ModelMeta: map[string]ModelMeta{"ghost/m3": {ContextWindow: 8192}}},
	}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	got, ok := st.DeclaredMeta("nvidia/m1")
	if !ok || got.ContextWindow != 131072 || got.MaxOutput != 4096 {
		t.Errorf("DeclaredMeta(nvidia/m1) = %+v ok=%v, want 131072/4096", got, ok)
	}
	if _, ok := st.DeclaredMeta("nvidia/m2"); ok {
		t.Errorf("an empty declaration must read as \"nothing declared\", not as a zero-size model")
	}
	if _, ok := st.DeclaredMeta("ghost/m3"); ok {
		t.Errorf("a disabled provider's listing must not feed routing or display")
	}
	if _, ok := st.DeclaredMeta("never-seen"); ok {
		t.Errorf("unknown model reported as declared")
	}

	// The numbers have to survive a restart — they come from a network fetch.
	reopened, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reopened.DeclaredMeta("nvidia/m1"); !ok || got.ContextWindow != 131072 {
		t.Errorf("after reopen: %+v ok=%v", got, ok)
	}
}

// intern-ai publishes output_modalities.max_length equal to max_context_length
// for all ten of its models. A completion cap the size of the whole window is
// the provider echoing its context length, and showing it as 输出 told the user
// nothing but that the model can produce a million tokens.
func TestDeclaredMetaDropsTheEchoedOutputCap(t *testing.T) {
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	st.Config().Providers = []Provider{{ID: "tp", Name: "T", Enabled: true,
		Models: []string{"echo", "stated"},
		ModelMeta: map[string]ModelMeta{
			"echo":   {ContextWindow: 1048576, MaxOutput: 1048576, InputDeclared: true, OutputDeclared: true},
			"stated": {ContextWindow: 1048576, MaxOutput: 65536, InputDeclared: true, OutputDeclared: true},
		}}}

	if got, ok := st.DeclaredMeta("echo"); !ok || got.MaxOutput != 0 {
		t.Errorf("echoed window survived: %+v ok=%v", got, ok)
	} else if got.ContextWindow != 1048576 {
		t.Errorf("context window lost with it: %+v", got)
	}
	if got, _ := st.DeclaredMeta("stated"); got.MaxOutput != 65536 {
		t.Errorf("a real completion cap was dropped: %+v", got)
	}
}
