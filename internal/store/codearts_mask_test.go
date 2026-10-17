package store

import (
	"encoding/json"
	"testing"

	"zen-gate/internal/lane"
)

// A shared config export must carry no CodeArts secret: the STS triple, the
// refresh binding (token + verifier) and the DPoP key all mask, while the
// identity fields stay so the file still says whose account it was.
func TestMaskedHidesEveryCodeArtsSecret(t *testing.T) {
	t.Setenv("ZEN_GATE_HOME", t.TempDir())
	st, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	st.Mutate(func(c *Config) {
		c.CodeArts = lane.CAAccount{
			UserID: "u-9", UserName: "someone",
			SecurityToken: "sts", AccessKeyID: "ak", SecretAccessKey: "sk",
			RefreshToken: "rt", CodeVerifier: "cv",
			DPoPKey: map[string]string{"d": "private-d"},
		}
	})
	cfg := st.Masked()
	b, _ := json.Marshal(cfg.CodeArts)
	for _, secret := range []string{"sts", "\"ak\"", "\"sk\"", "rt", "cv", "private-d"} {
		if contains(string(b), secret) {
			t.Errorf("masked export still carries %s: %s", secret, b)
		}
	}
	if cfg.CodeArts.UserID != "u-9" || cfg.CodeArts.UserName != "someone" {
		t.Errorf("identity should stay visible: %+v", cfg.CodeArts)
	}
	// Masking must not mutate the live config.
	if got := st.Config().CodeArts.SecurityToken; got != "sts" {
		t.Errorf("live config was mutated by Masked(): %q", got)
	}
}

// clone must not alias the DPoP map: a later refresh writes a new key on one
// snapshot and the previously handed-out snapshot must keep the old one.
func TestCloneCopiesCodeArtsDPoPMap(t *testing.T) {
	c := &Config{CodeArts: lane.CAAccount{DPoPKey: map[string]string{"d": "v1"}}}
	cp := c.clone()
	cp.CodeArts.DPoPKey["d"] = "v2"
	if c.CodeArts.DPoPKey["d"] != "v1" {
		t.Fatal("clone aliased the DPoP key map")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
