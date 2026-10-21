package remote

import (
	"errors"
	"strings"
	"testing"
)

// The classification is the whole reason the pool exists: it decides whether a
// failure is worth another account. These cases are the upstream answers that
// were actually observed, spelled out so a refactor cannot quietly reclassify
// one of them.
func TestClassifyQoderFailure(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantOut    PoolOutcome
		wantSwitch bool
	}{
		{
			name:       "queue envelope is model capacity, not the account",
			err:        &QuotaError{Status: 403, Kind: QuotaKindQueue, Body: `{"code":"10605"}`},
			wantOut:    PoolQueue,
			wantSwitch: true,
		},
		{
			name:       "402 credit is per account",
			err:        &QuotaError{Status: 402, Kind: QuotaKindCredit},
			wantOut:    PoolQuota,
			wantSwitch: true,
		},
		{
			name:       "429 rate is per account",
			err:        &QuotaError{Status: 429, Kind: QuotaKindRate},
			wantOut:    PoolQuota,
			wantSwitch: true,
		},
		{
			name:       "TOKEN_EXPIRE is a dead login",
			err:        &remoteStatusError{Status: 401, Body: `{"code":"TOKEN_EXPIRE"}`},
			wantOut:    PoolAuth,
			wantSwitch: true,
		},
		{
			name:       "offline user session is a dead login",
			err:        &remoteStatusError{Status: 403, Body: `Offline user session not found`},
			wantOut:    PoolAuth,
			wantSwitch: true,
		},
		{
			name:       "12153 is a dead login",
			err:        &remoteStatusError{Status: 403, Body: `{"code":12153}`},
			wantOut:    PoolAuth,
			wantSwitch: true,
		},
		{
			name: "a 403 without an auth marker is not a dead login",
			// The queue answer arrives as 403 too; treating every 403 as a
			// dead login would log the whole pool out during a busy hour.
			err:        &remoteStatusError{Status: 403, Body: `{"code":"FORBIDDEN_FOR_OTHER_REASON"}`},
			wantOut:    PoolOK,
			wantSwitch: false,
		},
		{
			name: "code 110 daily billing cap is per-account spend, not a dead login",
			// Observed live: Qoder answered the SSE stream with
			// {"code":"110","message":"Billing daily count exceeded"}. Filed as
			// auth it ended the turn with "invalid API key" while a second pool
			// account with a fresh daily count sat unused.
			err:        &remoteStatusError{Status: 403, Body: `{"code":"110","message":"Billing daily count exceeded"}`},
			wantOut:    PoolQuota,
			wantSwitch: true,
		},
		{
			name:       "a capped account whose body also says unauthorized stays spend",
			err:        &remoteStatusError{Status: 403, Body: `{"code":"110","message":"Billing daily count exceeded: unauthorized"}`},
			wantOut:    PoolQuota,
			wantSwitch: true,
		},
		{
			name:       "5xx is not worth another account",
			err:        &remoteStatusError{Status: 502, Body: "bad gateway"},
			wantOut:    PoolOK,
			wantSwitch: false,
		},
		{
			name:       "a transport error is not worth another account",
			err:        errors.New("dial tcp: connection refused"),
			wantOut:    PoolOK,
			wantSwitch: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, sw := classifyQoderFailure(tc.err)
			if out != tc.wantOut || sw != tc.wantSwitch {
				t.Errorf("classify = (%v, %v), want (%v, %v)", out, sw, tc.wantOut, tc.wantSwitch)
			}
		})
	}
}

// The upstream reports the queue as an HTTP error whose body names the reason.
// Both spellings were seen, plus the Chinese load message.
func TestQueueBodyDetection(t *testing.T) {
	for _, body := range []string{
		`{"code":"10605","message":"queued"}`,
		`{"error":"RESOURCE_LIMIT"}`,
		`{"isQueued":true}`,
		`{"message":"模型负载较高，请稍后再试"}`,
	} {
		if !isQoderQueueBody(body) {
			t.Errorf("%q was not recognized as a queue answer", body)
		}
	}
	for _, body := range []string{`{"code":12153}`, `{"error":"insufficient credit"}`, ``} {
		if isQoderQueueBody(body) {
			t.Errorf("%q was wrongly treated as a queue answer", body)
		}
	}
}

// A stated wait is surfaced so the caller can back off instead of retrying
// immediately against the same wall.
func TestRetryAfterIsReadFromTheEnvelope(t *testing.T) {
	if got := qoderRetryAfter(`{"retryAfterSeconds":42}`); got != 42 {
		t.Errorf("retryAfterSeconds = %d, want 42", got)
	}
	if got := qoderRetryAfter(`{"retry_after_seconds":"7"}`); got != 7 {
		t.Errorf("retry_after_seconds = %d, want 7", got)
	}
	if got := qoderRetryAfter(`{"message":"no wait stated"}`); got != 0 {
		t.Errorf("a body with no wait returned %d, want 0", got)
	}
}

// The quota error's message must name the kind, because the one thing a user
// needs to know is whether switching accounts can help.
func TestQuotaErrorMessageNamesTheKind(t *testing.T) {
	msg := (&QuotaError{Status: 403, Kind: QuotaKindQueue, Body: "queued"}).Error()
	if !strings.Contains(msg, "换账号无效") {
		t.Errorf("queue message does not say switching accounts is useless: %s", msg)
	}
	credit := (&QuotaError{Status: 402, Kind: QuotaKindCredit}).Error()
	if !strings.Contains(credit, "额度耗尽") {
		t.Errorf("credit message does not name the cause: %s", credit)
	}
}
