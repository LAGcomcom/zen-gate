package remote

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The Qoder account pool: several IDE login snapshots behind one channel.
//
// Two facts shape it, both measured upstream and neither obvious:
//
//   - A "queue" answer (10605 / RESOURCE_LIMIT) is MODEL-level capacity, shared
//     by every account. Rotating accounts on it achieves nothing except
//     dragging several accounts into cooldown at once, so the pool never
//     switches on it — it surfaces the wait and lets the caller pick another
//     model.
//   - What an account really buys is quota and concurrency. So the pool
//     switches on the things that are actually per-account: a dead login
//     (401/403/TOKEN_EXPIRE/12153) and exhausted credit (402).
//
// Storage is one file per account plus an index. Each account keeps the whole
// credential the IDE minted, including its machine id, so switching accounts
// never re-uses another account's device identity.

const (
	// qoderMaxAccountTries bounds how many accounts one turn may spend.
	qoderMaxAccountTries = 4
	// poolQuotaCooldown parks an account whose credit is gone. Long, because
	// credit does not come back within a turn.
	poolQuotaCooldown = 30 * time.Minute
	// poolAuthCooldown parks an account whose login died: it will not fix
	// itself, but the next turn may still find the IDE has re-minted it.
	poolAuthCooldown = 10 * time.Minute
	// poolIDEAccountID names the live IDE cache in the candidate list. It is
	// always last, and always present when the pool is empty, so a
	// single-account install behaves exactly as it did before pooling.
	poolIDEAccountID = "ide"
)

// PoolOutcome is what one attempt taught us about one account.
type PoolOutcome int

const (
	// PoolOK clears the failure bookkeeping.
	PoolOK PoolOutcome = iota
	// PoolQuota: this account's credit is gone.
	PoolQuota
	// PoolAuth: this account's login is dead.
	PoolAuth
	// PoolQueue: model-level capacity, nothing to do with the account.
	PoolQueue
)

// PoolAccount is one pool member: the durable identity from the index plus the
// memory-only runtime state. Runtime state is deliberately not persisted — a
// cooldown is a fact about right now, and a restart should re-try.
type PoolAccount struct {
	ID        string    `json:"id"`
	Label     string    `json:"label,omitempty"`
	UID       string    `json:"uid,omitempty"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
	// ExpiresAt is when the account's token dies (zero when unknown). It is
	// index data, not a secret, and the card needs it to warn before an account
	// quietly stops answering.
	ExpiresAt time.Time `json:"expiresAt,omitempty"`

	cooledUntil time.Time
	modelCool   map[string]time.Time
	authBad     bool
	lastError   string
}

// poolIndex is the on-disk index: order plus labels, never secrets.
type poolIndex struct {
	Version  int            `json:"version"`
	Accounts []*PoolAccount `json:"accounts"`
}

// Pool owns the account directory.
type Pool struct {
	mu       sync.Mutex
	dir      string
	accounts []*PoolAccount
}

// DefaultAccountsDir is where the pool lives: beside the hub's own settings,
// under the user's config dir.
func DefaultAccountsDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "qodercn-gateway", "accounts"), nil
}

// NewPool builds a pool rooted at dir. It does not touch the disk; call Load.
func NewPool(dir string) *Pool { return &Pool{dir: dir} }

// Dir reports the pool's directory.
func (p *Pool) Dir() string { return p.dir }

// Load reads the index. A missing index is an empty pool, not an error: that
// is exactly the state of a first run.
func (p *Pool) Load() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	data, err := os.ReadFile(filepath.Join(p.dir, "index.json"))
	if err != nil {
		if os.IsNotExist(err) {
			p.accounts = nil
			return nil
		}
		return err
	}
	var idx poolIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return fmt.Errorf("read qoder account index: %w", err)
	}
	p.accounts = idx.Accounts
	for _, a := range p.accounts {
		if a.modelCool == nil {
			a.modelCool = map[string]time.Time{}
		}
	}
	return nil
}

// SaveIndex writes the index atomically. Runtime state is dropped on the way
// out: the file carries identity and order only.
func (p *Pool) SaveIndex() error {
	p.mu.Lock()
	accounts := make([]*PoolAccount, len(p.accounts))
	copy(accounts, p.accounts)
	p.mu.Unlock()

	clean := make([]*PoolAccount, 0, len(accounts))
	for _, a := range accounts {
		clean = append(clean, &PoolAccount{
			ID: a.ID, Label: a.Label, UID: a.UID,
			Enabled: a.Enabled, CreatedAt: a.CreatedAt, ExpiresAt: a.ExpiresAt,
		})
	}
	idx := poolIndex{Version: 1, Accounts: clean}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(p.dir, "index.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Snapshot returns a copy of the pool for display. It carries no credential
// material of any kind — the whole point of keeping the secrets in per-account
// files is that a status response can be shown to anyone.
func (p *Pool) Snapshot() []PoolAccount {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]PoolAccount, 0, len(p.accounts))
	for _, a := range p.accounts {
		out = append(out, PoolAccount{
			ID: a.ID, Label: a.Label, UID: a.UID,
			Enabled: a.Enabled, CreatedAt: a.CreatedAt, ExpiresAt: a.ExpiresAt,
			cooledUntil: a.cooledUntil, authBad: a.authBad, lastError: a.lastError,
		})
	}
	return out
}

// State names one account's condition for the UI.
func (a PoolAccount) State(now time.Time) string {
	switch {
	case a.authBad:
		return "auth"
	case !a.Enabled:
		return "disabled"
	case a.cooledUntil.After(now):
		return "cooldown"
	}
	return "ok"
}

// CooldownUntil exposes the park deadline (zero when healthy).
func (a PoolAccount) CooldownUntil() time.Time { return a.cooledUntil }

// LastError exposes the last failure note.
func (a PoolAccount) LastError() string { return a.lastError }

// sanitizeID keeps an account id safe as a file name.
func sanitizeID(v string) string {
	var b strings.Builder
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		}
		if b.Len() >= 40 {
			break
		}
	}
	return b.String()
}

// CredentialPath is where one account's credential file lives.
func (p *Pool) CredentialPath(id string) string {
	return filepath.Join(p.dir, id+".json")
}

// Import stores one credential as an account, or updates the account that
// already owns this UID. Importing the same login twice must not create a
// second row: the upstream would still meter it as one account, so a duplicate
// row only misleads the scheduler.
func (p *Pool) Import(cred Credential, label string) (PoolAccount, error) {
	if err := validateCredential(cred); err != nil {
		return PoolAccount{}, err
	}
	id := "acc-" + sanitizeID(cred.UserID)
	if sanitizeID(cred.UserID) == "" {
		id = "acc-" + sanitizeID(fmt.Sprintf("%x", time.Now().UnixNano()))
	}
	if err := SaveCredentialFile(cred, p.CredentialPath(id)); err != nil {
		return PoolAccount{}, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	expiry := liveExpiry(cred)
	for _, a := range p.accounts {
		if a.ID == id || (a.UID != "" && a.UID == cred.UserID) {
			a.UID = cred.UserID
			if label != "" {
				a.Label = label
			}
			a.ExpiresAt = expiry
			a.authBad = false
			a.cooledUntil = time.Time{}
			a.modelCool = map[string]time.Time{}
			a.lastError = ""
			return *a, nil
		}
	}
	a := &PoolAccount{
		ID: id, Label: label, UID: cred.UserID,
		Enabled: true, CreatedAt: now, ExpiresAt: expiry,
		modelCool: map[string]time.Time{},
	}
	p.accounts = append(p.accounts, a)
	return *a, nil
}

// liveExpiry is the account's token deadline as a time, or zero when the
// credential does not carry one (the IDE cache does not state it).
func liveExpiry(cred Credential) time.Time {
	if cred.TokenExpireTime <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(cred.TokenExpireTime)
}

// ImportCurrent snapshots the IDE's live login into the pool.
func (p *Pool) ImportCurrent(label string) (PoolAccount, error) {
	cred, err := LoadCredential("")
	if err != nil {
		return PoolAccount{}, fmt.Errorf("未找到可用的 QoderCN/Lingma 登录缓存：%w", err)
	}
	if strings.TrimSpace(label) == "" {
		label = "账号 " + shortUID(cred.UserID)
	}
	return p.Import(cred, label)
}

// shortUID renders the tail of a uid for a default label.
func shortUID(uid string) string {
	if len(uid) <= 6 {
		return uid
	}
	return uid[len(uid)-6:]
}

// Delete drops one account and its credential file.
func (p *Pool) Delete(id string) error {
	p.mu.Lock()
	keep := p.accounts[:0]
	found := false
	for _, a := range p.accounts {
		if a.ID == id {
			found = true
			continue
		}
		keep = append(keep, a)
	}
	p.accounts = keep
	p.mu.Unlock()
	if !found {
		return errors.New("账号不存在")
	}
	_ = os.Remove(p.CredentialPath(id))
	return nil
}

// SetEnabled flips one account's participation.
func (p *Pool) SetEnabled(id string, enabled bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.ID == id {
			a.Enabled = enabled
			return nil
		}
	}
	return errors.New("账号不存在")
}

// MoveFirst promotes one account to the front of the pool.
func (p *Pool) MoveFirst(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx := -1
	for i, a := range p.accounts {
		if a.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return errors.New("账号不存在")
	}
	if idx == 0 {
		return nil
	}
	moved := p.accounts[idx]
	rest := make([]*PoolAccount, 0, len(p.accounts))
	rest = append(rest, moved)
	rest = append(rest, p.accounts[:idx]...)
	rest = append(rest, p.accounts[idx+1:]...)
	p.accounts = rest
	return nil
}

// Report books one attempt's outcome against one account.
//
// PoolQueue deliberately does nothing: a model-level queue is shared by every
// account, so parking the account that happened to ask would only shrink the
// pool without buying capacity.
func (p *Pool) Report(id, model string, out PoolOutcome, msg string) {
	if id == "" || id == poolIDEAccountID {
		// The live IDE cache is not a pool row; nothing to book.
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.ID != id {
			continue
		}
		now := time.Now()
		switch out {
		case PoolOK:
			a.authBad = false
			a.lastError = ""
		case PoolQuota:
			// A quota refusal that names a model parks that model only. The
			// account-wide deadline is reserved for a refusal with no model to
			// blame (spent balance), because parking everything on a
			// per-model window would drop capacity the account still has.
			if model == "" {
				if until := now.Add(poolQuotaCooldown); until.After(a.cooledUntil) {
					a.cooledUntil = until
				}
			} else {
				if a.modelCool == nil {
					a.modelCool = map[string]time.Time{}
				}
				a.modelCool[model] = now.Add(poolQuotaCooldown)
			}
			a.lastError = truncateNote(msg)
		case PoolAuth:
			a.authBad = true
			a.lastError = truncateNote(msg)
		}
		return
	}
}

func truncateNote(msg string) string {
	msg = strings.TrimSpace(msg)
	if len(msg) > 200 {
		return msg[:200]
	}
	return msg
}

// PoolCandidate is one attempt target: an id (for reporting) and the
// credential to present.
type PoolCandidate struct {
	ID   string
	Cred Credential
}

// Candidates lists the accounts to try for one model, in order, with the live
// IDE login last. When the pool is empty the result is just that live login —
// which is exactly what the single-account code path did.
func (p *Pool) Candidates(model string) []PoolCandidate {
	now := time.Now()
	p.mu.Lock()
	rows := make([]*PoolAccount, len(p.accounts))
	copy(rows, p.accounts)
	p.mu.Unlock()

	out := make([]PoolCandidate, 0, len(rows)+1)
	seen := map[string]bool{}
	for _, a := range rows {
		if !a.Enabled || a.authBad {
			continue
		}
		if a.cooledUntil.After(now) {
			continue
		}
		if model != "" {
			if until, ok := a.modelCool[model]; ok && until.After(now) {
				continue
			}
		}
		cred, err := loadCredentialFile(p.CredentialPath(a.ID))
		if err != nil {
			// A file that cannot be read is a dead account, not a fatal error:
			// note it and move on.
			p.mu.Lock()
			a.authBad = true
			a.lastError = "凭证文件不可读：" + err.Error()
			p.mu.Unlock()
			continue
		}
		if a.UID != "" {
			seen[a.UID] = true
		}
		out = append(out, PoolCandidate{ID: a.ID, Cred: cred})
	}

	// The live IDE login is always a candidate, last, unless the pool already
	// holds that same account (then it is the same credential twice).
	if cred, err := LoadCredential(""); err == nil {
		if cred.UserID == "" || !seen[cred.UserID] {
			out = append(out, PoolCandidate{ID: poolIDEAccountID, Cred: cred})
		}
	}
	return out
}

// IDELoggedIn reports whether the IDE cache currently holds a usable login.
func IDELoggedIn() bool {
	cred, err := LoadCredential("")
	return err == nil && strings.TrimSpace(cred.CosyKey) != ""
}

// sortAccountsByCreated keeps a stable, explainable order when nothing else
// decides (used by tests and by the index writer's determinism).
func sortAccountsByCreated(accts []*PoolAccount) {
	sort.SliceStable(accts, func(i, j int) bool { return accts[i].CreatedAt.Before(accts[j].CreatedAt) })
}
