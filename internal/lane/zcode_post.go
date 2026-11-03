package lane

import (
	"context"
	"strings"
	"time"
)

// Rotation over the ZCode pool.
//
// A turn spends one account end to end. It switches only on refusals that are
// actually per-account — a dead login, exhausted credit, or a risk-control park —
// because switching on anything else pays the latency twice for the same answer.
//
// Once a byte has reached the client the turn is committed: replaying it on
// another account would duplicate the visible answer, so the switch happens only
// before the first emitted frame.

const zcodeMaxAccountTries = 4

// zcodePost walks the pool for one turn.
func (l *Lane) zcodePost(ctx context.Context, body map[string]any, onData func([]byte) error) (*Usage, error) {
	accts := l.zcodeSpendable()
	if len(accts) == 0 {
		return nil, &UpstreamError{Code: CodeCredential,
			Message: "还没有登录任何 ZCode 账号，请先在渠道页登录"}
	}
	emitted := false
	tracked := func(p []byte) error {
		emitted = true
		return onData(p)
	}

	var lastErr error
	sawQuota := false
	for i, acct := range accts {
		if i >= zcodeMaxAccountTries {
			break
		}
		usage, err := l.zcodeChat(ctx, body, acct, tracked)
		if err == nil {
			l.zcodeMarkOK(acct.ID)
			return usage, nil
		}
		lastErr = err
		ue := asUpstream(err)
		if ue == nil {
			return usage, err
		}
		switch ue.Code {
		case CodeQuota:
			sawQuota = true
			l.zcodeCool(acct.ID, ue.RetryAfter)
			l.logf("ZCode 账号 %s 额度受限：%s", acct.Label, truncate(ue.Message, 120))
		case CodeCredential:
			l.zcodeMarkAuth(acct.ID, ue.Message)
			l.logf("ZCode 账号 %s 登录态问题：%s", acct.Label, truncate(ue.Message, 120))
		default:
			// A request-shaped defect answers identically on every account;
			// switching would only pay the latency twice.
			return usage, err
		}
		if emitted {
			return usage, err
		}
	}
	if lastErr != nil {
		if sawQuota {
			return nil, lastErr
		}
		return nil, lastErr
	}
	return nil, &UpstreamError{Code: CodeQuota, Message: "ZCode 账号都不可用"}
}

// zcodeCoolNote records a park. Kept in a package-level map rather than in the
// account struct so a restart forgets it: a cooldown is a fact about right now.
var zcodePark = map[string]int64{} // account id -> unix-milli until usable

func (l *Lane) zcodeCool(id string, seconds int) {
	if seconds <= 0 {
		seconds = 600
	}
	zcodePark[id] = time.Now().UnixMilli() + int64(seconds)*1000
}

func (l *Lane) zcodeMarkOK(id string) {
	delete(zcodePark, id)
}

// zcodeMarkAuth benches an account whose login died. It will not fix itself, but
// the user can log in again, so this only parks it rather than deleting it.
func (l *Lane) zcodeMarkAuth(id, reason string) {
	l.logf("ZCode 账号 %s 被停用一轮：%s", id, truncate(strings.TrimSpace(reason), 120))
	zcodePark[id] = time.Now().UnixMilli() + 10*60*1000
}

// zcodeParked reports whether an account is currently parked.
func zcodeParked(id string) bool {
	until, ok := zcodePark[id]
	if !ok {
		return false
	}
	if time.Now().UnixMilli() >= until {
		delete(zcodePark, id)
		return false
	}
	return true
}

// ZCodeAccountParked is the exported form the admin card reads.
func ZCodeAccountParked(id string) bool { return zcodeParked(id) }
