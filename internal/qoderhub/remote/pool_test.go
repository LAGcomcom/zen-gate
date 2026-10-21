package remote

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestPool builds a pool over a temp dir with the IDE cache pointed at an
// empty directory, so the live-login fallback contributes nothing and the tests
// see only the rows they create.
func newTestPool(t *testing.T) *Pool {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("QODERCN_CACHE_DIR", filepath.Join(dir, "no-ide-cache"))
	pool := NewPool(filepath.Join(dir, "accounts"))
	if err := pool.Load(); err != nil {
		t.Fatalf("load empty pool: %v", err)
	}
	return pool
}

func testCredential(uid string) Credential {
	return Credential{
		CosyKey:         "cosy-" + uid,
		EncryptUserInfo: "enc-" + uid,
		UserID:          uid,
		MachineID:       "machine-" + uid,
	}
}

// Importing the same login twice must not create a second row: the upstream
// meters it as one account, so a duplicate row would make the scheduler think
// it has capacity it does not have.
func TestImportIsIdempotentPerAccount(t *testing.T) {
	pool := newTestPool(t)
	a, err := pool.Import(testCredential("uid-1"), "one")
	if err != nil {
		t.Fatal(err)
	}
	b, err := pool.Import(testCredential("uid-1"), "")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Fatalf("same uid got two ids: %q then %q", a.ID, b.ID)
	}
	if got := len(pool.Snapshot()); got != 1 {
		t.Fatalf("pool holds %d rows after importing one account twice, want 1", got)
	}
	if b.Label != "one" {
		t.Errorf("re-import with an empty label wiped it: %q", b.Label)
	}
}

// The index carries identity and order, never secrets.
func TestIndexHoldsNoCredentialMaterial(t *testing.T) {
	pool := newTestPool(t)
	if _, err := pool.Import(testCredential("uid-1"), "one"); err != nil {
		t.Fatal(err)
	}
	if err := pool.SaveIndex(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(pool.Dir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"cosy-", "enc-", "machine-"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("index.json carries credential material %q:\n%s", secret, raw)
		}
	}
	if !strings.Contains(string(raw), "uid-1") {
		t.Errorf("index.json lost the account identity:\n%s", raw)
	}
}

// A spent account must not be offered again, but a spent account for ONE model
// must stay usable for the others — that is the difference between a model
// window and an account-level ban.
func TestCandidatesSkipParkedAccountsPerScope(t *testing.T) {
	pool := newTestPool(t)
	a, err := pool.Import(testCredential("uid-1"), "one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Import(testCredential("uid-2"), "two"); err != nil {
		t.Fatal(err)
	}

	pool.Report(a.ID, "model-x", PoolQuota, "credit gone")
	ids := candidateIDs(pool.Candidates("model-x"))
	if contains(ids, a.ID) {
		t.Errorf("model-x candidates still name the spent account: %v", ids)
	}
	other := candidateIDs(pool.Candidates("model-y"))
	if !contains(other, a.ID) {
		t.Errorf("model-y candidates dropped an account parked only for model-x: %v", other)
	}

	pool.Report(a.ID, "", PoolQuota, "account credit gone")
	if all := candidateIDs(pool.Candidates("model-y")); contains(all, a.ID) {
		t.Errorf("an account-level park must hide the account from every model: %v", all)
	}
}

// A queue answer is model-level capacity shared by every account. Booking it
// against the asking account would shrink the pool and buy nothing.
func TestQueueOutcomeParksNothing(t *testing.T) {
	pool := newTestPool(t)
	a, err := pool.Import(testCredential("uid-1"), "one")
	if err != nil {
		t.Fatal(err)
	}
	pool.Report(a.ID, "model-x", PoolQueue, "10605 queued")
	if ids := candidateIDs(pool.Candidates("model-x")); !contains(ids, a.ID) {
		t.Errorf("a queue answer parked the account: %v", ids)
	}
	if st := pool.Snapshot()[0].State(time.Now()); st != "ok" {
		t.Errorf("state after a queue answer = %q, want ok", st)
	}
}

// A dead login parks the account until it is re-imported; a successful attempt
// clears both the park and the error note.
func TestAuthFailureAndRecovery(t *testing.T) {
	pool := newTestPool(t)
	a, err := pool.Import(testCredential("uid-1"), "one")
	if err != nil {
		t.Fatal(err)
	}
	pool.Report(a.ID, "", PoolAuth, "TOKEN_EXPIRE")
	if got := pool.Snapshot()[0].State(time.Now()); got != "auth" {
		t.Fatalf("state after an auth failure = %q, want auth", got)
	}
	if ids := candidateIDs(pool.Candidates("")); contains(ids, a.ID) {
		t.Errorf("a dead login is still a candidate: %v", ids)
	}

	if _, err := pool.Import(testCredential("uid-1"), ""); err != nil {
		t.Fatal(err)
	}
	if got := pool.Snapshot()[0].State(time.Now()); got != "ok" {
		t.Fatalf("re-importing the login left state = %q, want ok", got)
	}

	pool.Report(a.ID, "", PoolAuth, "TOKEN_EXPIRE")
	pool.Report(a.ID, "", PoolOK, "")
	if got := pool.Snapshot()[0].State(time.Now()); got != "ok" {
		t.Fatalf("a successful attempt did not clear the auth flag: %q", got)
	}
}

// Disabled accounts keep their row (so the card can show them and the toggle
// can bring them back) but never serve.
func TestDisabledAccountKeepsItsRow(t *testing.T) {
	pool := newTestPool(t)
	a, err := pool.Import(testCredential("uid-1"), "one")
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.SetEnabled(a.ID, false); err != nil {
		t.Fatal(err)
	}
	if got := len(pool.Snapshot()); got != 1 {
		t.Fatalf("disabling dropped the row: %d rows", got)
	}
	if ids := candidateIDs(pool.Candidates("")); contains(ids, a.ID) {
		t.Errorf("a disabled account is still a candidate: %v", ids)
	}
	if got := pool.Snapshot()[0].State(time.Now()); got != "disabled" {
		t.Errorf("state = %q, want disabled", got)
	}
}

// MoveFirst is how "优先使用" works: order is the only priority mechanism, so it
// must survive a save/load round trip.
func TestMoveFirstSurvivesSaveAndLoad(t *testing.T) {
	pool := newTestPool(t)
	if _, err := pool.Import(testCredential("uid-1"), "one"); err != nil {
		t.Fatal(err)
	}
	second, err := pool.Import(testCredential("uid-2"), "two")
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.MoveFirst(second.ID); err != nil {
		t.Fatal(err)
	}
	if err := pool.SaveIndex(); err != nil {
		t.Fatal(err)
	}

	reloaded := NewPool(pool.Dir())
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	rows := reloaded.Snapshot()
	if len(rows) != 2 || rows[0].ID != second.ID {
		t.Fatalf("reloaded order = %v, want %q first", rowIDs(rows), second.ID)
	}
}

// Delete removes the row and its credential file: leaving the file behind would
// leave a secret on disk that nothing manages any more.
func TestDeleteRemovesCredentialFile(t *testing.T) {
	pool := newTestPool(t)
	a, err := pool.Import(testCredential("uid-1"), "one")
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	if got := len(pool.Snapshot()); got != 0 {
		t.Fatalf("pool still holds %d rows after delete", got)
	}
	if _, err := os.ReadFile(pool.CredentialPath(a.ID)); err == nil {
		t.Error("credential file survived the delete")
	}
}

// An unreadable credential file is a dead account, not a fatal error: the pool
// notes it and keeps serving the others.
func TestUnreadableCredentialIsSkippedNotFatal(t *testing.T) {
	pool := newTestPool(t)
	broken, err := pool.Import(testCredential("uid-1"), "broken")
	if err != nil {
		t.Fatal(err)
	}
	good, err := pool.Import(testCredential("uid-2"), "good")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(pool.CredentialPath(broken.ID)); err != nil {
		t.Fatal(err)
	}
	ids := candidateIDs(pool.Candidates(""))
	if contains(ids, broken.ID) {
		t.Errorf("an unreadable account is still a candidate: %v", ids)
	}
	if !contains(ids, good.ID) {
		t.Errorf("a good account was dropped because a sibling was unreadable: %v", ids)
	}
	for _, row := range pool.Snapshot() {
		if row.ID == broken.ID && row.State(time.Now()) != "auth" {
			t.Errorf("broken account state = %q, want auth", row.State(time.Now()))
		}
	}
}

func candidateIDs(cands []PoolCandidate) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.ID)
	}
	return out
}

func rowIDs(rows []PoolAccount) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

func contains(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
