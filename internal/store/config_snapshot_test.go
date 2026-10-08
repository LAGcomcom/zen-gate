package store

import "testing"

// The gateway reads the config from every request goroutine while an admin
// handler writes it, and until now both touched the same object: a handler that
// set a field rewrote the config a request was mid-way through reading, and
// Save marshalled whatever a concurrent writer had left half-updated. Mutations
// go through Mutate, which publishes a copy, so a config already handed out is
// frozen for its holder.
func TestMutateLeavesAConfigItAlreadyHandedOutAlone(t *testing.T) {
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	held := st.Config()
	original := held.MainKey

	st.Mutate(func(c *Config) { c.MainKey = "sk-rotated" })
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	if got := st.Config().MainKey; got != "sk-rotated" {
		t.Errorf("Config().MainKey = %q, want the mutated value", got)
	}
	if held.MainKey != original {
		t.Errorf("Mutate rewrote the config another goroutine was holding: %q", held.MainKey)
	}
	reopened, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Config().MainKey != "sk-rotated" {
		t.Errorf("Mutate did not persist: %q", reopened.Config().MainKey)
	}
}

// Copying only the top-level struct would still let a writer reach through a
// shared slice backing array or map and change what a reader sees.
func TestMutateCopiesNestedValues(t *testing.T) {
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	st.Mutate(func(c *Config) {
		c.Providers = []Provider{{ID: "nim", APIKey: "old", Models: []string{"glm-4.5"}}}
		c.AgentKeys = map[string]string{"claude": "old"}
		c.EnabledAgents = map[string]bool{"claude": true}
		c.HiddenModels = []string{"m-1"}
	})
	held := st.Config()
	st.Mutate(func(c *Config) {
		c.Providers[0].APIKey = "new"
		c.Providers[0].Models[0] = "renamed"
		c.AgentKeys["claude"] = "new"
		c.EnabledAgents["claude"] = false
		c.HiddenModels[0] = "m-2"
	})

	if held.Providers[0].APIKey != "old" || held.Providers[0].Models[0] != "glm-4.5" {
		t.Errorf("nested provider values were shared: %+v", held.Providers[0])
	}
	if held.AgentKeys["claude"] != "old" {
		t.Errorf("the agentKey map was shared: %v", held.AgentKeys)
	}
	if !held.EnabledAgents["claude"] {
		t.Errorf("the enabledAgents map was shared: %v", held.EnabledAgents)
	}
	if held.HiddenModels[0] != "m-1" {
		t.Errorf("the hiddenModels slice was shared: %v", held.HiddenModels)
	}
}

// A Mutate that produces no change must not blank a collection the JSON round
// trip would otherwise drop, and an absent collection stays absent rather than
// turning into an empty one.
func TestMutateKeepsAbsentCollectionsAbsent(t *testing.T) {
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	st.Mutate(func(c *Config) { c.Port = 8788 })
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Config().Providers != nil {
		t.Errorf("Providers = %v, want it to stay absent", reopened.Config().Providers)
	}
	if reopened.Config().Port != 8788 {
		t.Errorf("Port = %d, want 8788", reopened.Config().Port)
	}
}
