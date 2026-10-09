package agents

import (
	"sync/atomic"
	"testing"

	"zen-gate/internal/lane"
	"zen-gate/internal/store"
)

// countingAgent is a throwaway adapter that only records how many times it was
// written and reports enabled, to observe Registry.ResyncEnabled's fingerprint
// gate directly.
type countingAgent struct {
	enableCalls atomic.Int32
}

func (c *countingAgent) Meta() (string, string, string)   { return "count", "Count", "test" }
func (c *countingAgent) Detect() (bool, string, string)   { return true, "", "" }
func (c *countingAgent) IsEnabled() (bool, string, error) { return true, "", nil }
func (c *countingAgent) Enable(Options) error {
	c.enableCalls.Add(1)
	return nil
}
func (c *countingAgent) Disable() error { return nil }

func newCountingRegistry(t *testing.T, a *countingAgent) *Registry {
	t.Helper()
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(st)
	r.agents = []Agent{a}
	return r
}

// TestResyncEnabledGateIsTheInjectionFix pins issue #26's behaviour: main wires
// ResyncEnabled into OnChange, which fires on every probe round. The gate must
// write once at boot, then stay silent while the roster is unchanged — an idle
// probe round must not rewrite every agent (each rewrite stacks a timestamped
// backup that is never pruned). A roster change unlocks exactly one rewrite.
func TestResyncEnabledGateIsTheInjectionFix(t *testing.T) {
	a := &countingAgent{}
	r := newCountingRegistry(t, a)
	models := []lane.ModelInfo{{ID: "m1", ContextWindow: 1000, MaxOutput: 100}}
	r.SetEndpoints("http://127.0.0.1:1/v1", models)

	// Boot: nothing written yet, so the first pass injects.
	if r.ResyncEnabled() != 1 {
		t.Fatal("first sync must inject the enabled adapter")
	}
	// Idle probe rounds with an unchanged roster must not touch the file.
	for i := 0; i < 3; i++ {
		r.SetEndpoints("http://127.0.0.1:1/v1", models)
		if n := r.ResyncEnabled(); n != 0 {
			t.Fatalf("unchanged roster must be a no-op, pass %d injected %d", i, n)
		}
	}
	if a.enableCalls.Load() != 1 {
		t.Fatalf("adapter written %d times, want 1", a.enableCalls.Load())
	}

	// A new model unlocks one rewrite.
	r.SetEndpoints("http://127.0.0.1:1/v1", append(models, lane.ModelInfo{ID: "m2", ContextWindow: 1000, MaxOutput: 100}))
	if r.ResyncEnabled() != 1 {
		t.Fatal("a model joining the roster must re-inject")
	}

	// A capability change on an existing model (what #27/#28 fix on disk) also
	// counts, not just an id added or removed.
	r.SetEndpoints("http://127.0.0.1:1/v1", []lane.ModelInfo{
		{ID: "m1", ContextWindow: 1000, MaxOutput: 999999},
		{ID: "m2", ContextWindow: 1000, MaxOutput: 100},
	})
	if r.ResyncEnabled() != 1 {
		t.Fatal("a changed max-output must re-inject even with the same id set")
	}
}

// TestResyncGatePartialFailureDoesNotCommit proves the partial-pass guard: if
// one enabled adapter fails its write, the fingerprint must not advance, so the
// next round retries the whole list rather than stranding the failed agent on
// stale content forever behind the gate.
func TestResyncGatePartialFailureDoesNotCommit(t *testing.T) {
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	good := &countingAgent{}
	failing := &failingAgent{}
	r := NewRegistry(st)
	r.agents = []Agent{good, failing}
	r.SetEndpoints("http://127.0.0.1:1/v1", []lane.ModelInfo{{ID: "m1"}})

	// Two enabled adapters, one always fails: done(1) != attempted(2), so
	// nothing commits and both return 1 each round while it keeps failing.
	if n := r.ResyncEnabled(); n != 1 {
		t.Fatalf("partial pass should report the one success, got %d", n)
	}
	if n := r.ResyncEnabled(); n != 1 {
		t.Fatalf("second round must retry (gate uncommitted), got %d", n)
	}
	if good.enableCalls.Load() != 2 {
		t.Fatalf("an uncommitted gate must retry the whole list every round, got %d writes", good.enableCalls.Load())
	}
}

// failingAgent always refuses Enable, to exercise the partial-pass branch.
type failingAgent struct{}

func (failingAgent) Meta() (string, string, string)   { return "fail", "Fail", "test" }
func (failingAgent) Detect() (bool, string, string)   { return true, "", "" }
func (failingAgent) IsEnabled() (bool, string, error) { return true, "", nil }
func (failingAgent) Enable(Options) error             { return errNoWrite }
func (failingAgent) Disable() error                   { return nil }

var errNoWrite = &writeErr{}

type writeErr struct{}

func (*writeErr) Error() string { return "injected write failure" }
