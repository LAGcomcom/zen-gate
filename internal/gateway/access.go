package gateway

import (
	"bytes"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"zen-gate/internal/lane"
	"zen-gate/internal/logx"
)

// Per-request logging. One logical client request gets one `access` line,
// written when its response is complete; every physical upstream attempt behind
// it (the free lane's failovers and the user-added providers) gets an `upstream`
// line, and the two bodies only get quoted at all when the user switches the
// `content` class on.

// turnAudit accumulates the attempts made for one client request.
type turnAudit struct {
	mu        sync.Mutex
	attempts  int
	served    string
	in        int
	out       int
	reasoning int
	cacheRead int
	errCode   string
	upRID     string
	recovered bool
}

// note folds one attempt into the running total and returns its sequence
// number for the upstream line.
func (a *turnAudit) note(rec lane.CallRecord) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.attempts++
	a.served = rec.Model
	a.in += rec.Input
	a.out += rec.Output
	a.reasoning += rec.Reasoning
	a.cacheRead += rec.CacheRead
	if rec.Recovered {
		a.recovered = true
	}
	// The access line reports what the client finally got, so a failover that
	// rescued the turn leaves no error behind.
	if rec.ErrCode == "" {
		a.errCode, a.upRID = "", ""
	} else {
		a.errCode, a.upRID = rec.ErrCode, rec.UpstreamRID
	}
	return a.attempts
}

// tally is a turnAudit flattened for one log line.
type tally struct {
	attempts  int
	served    string
	in        int
	out       int
	reasoning int
	cacheRead int
	errCode   string
	upRID     string
	recovered bool
}

func (a *turnAudit) snapshot() tally {
	a.mu.Lock()
	defer a.mu.Unlock()
	return tally{attempts: a.attempts, served: a.served, in: a.in, out: a.out,
		reasoning: a.reasoning, cacheRead: a.cacheRead, errCode: a.errCode,
		upRID: a.upRID, recovered: a.recovered}
}

// turnRecorder watches the response so the access line can carry the status and
// the byte count without the handlers knowing it is being measured.
type turnRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
	head   []byte
}

func (t *turnRecorder) WriteHeader(status int) {
	if t.status == 0 {
		t.status = status
	}
	t.ResponseWriter.WriteHeader(status)
}

func (t *turnRecorder) Write(p []byte) (int, error) {
	if t.status == 0 {
		t.status = http.StatusOK
	}
	if len(t.head) < bodyPeek {
		room := bodyPeek - len(t.head)
		if room > len(p) {
			room = len(p)
		}
		t.head = append(t.head, p[:room]...)
	}
	n, err := t.ResponseWriter.Write(p)
	t.bytes += n
	return n, err
}

// Flush keeps streaming working: the SSE writer type-asserts for it, and
// embedding the interface alone would not promote it.
func (t *turnRecorder) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

const bodyPeek = 4096

// serveTurn wraps a model endpoint: stamp the correlation id, register the
// audit the attempts report into, then write the request's log lines.
func (s *Server) serveTurn(w http.ResponseWriter, r *http.Request, wire string, next http.HandlerFunc) {
	start := time.Now()
	r = stampRequestID(w, r)
	rid := requestID(r)
	agent := s.agentOf(r)

	audit := &turnAudit{}
	s.trackAudit(rid, audit)
	defer s.forgetAudit(rid)

	head := peekBody(r)
	rec := &turnRecorder{ResponseWriter: w}
	if !s.authorized(rec, r) {
		s.logAccess(rid, wire, head, audit, rec, start, agent)
		return
	}
	next(rec, r)
	s.logAccess(rid, wire, head, audit, rec, start, agent)
}

// peekBody buffers the head of the request body and replays it for the handler,
// so the log can name the model (and quote the prompt when asked) without
// consuming it.
func peekBody(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	buf := make([]byte, bodyPeek)
	n, _ := io.ReadFull(r.Body, buf)
	head := buf[:n]
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(head), r.Body))
	return head
}

// requestedModel pulls the model out of the buffered request head; the JSON
// field order is client-chosen, so look in the head only and let the attempts
// supply the answer when it is not there.
func requestedModel(head []byte) string {
	m := modelRe.FindSubmatch(head)
	if m == nil {
		return ""
	}
	return string(m[1])
}

var modelRe = regexp.MustCompile(`"model"\s*:\s*"([^"]+)"`)

func (s *Server) logAccess(rid, wire string, head []byte, audit *turnAudit, rec *turnRecorder, start time.Time, agent string) {
	if s.logger == nil {
		return
	}
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	parts := []string{"rid=" + rid, wire, "status=" + strconv.Itoa(status),
		"ms=" + strconv.FormatInt(time.Since(start).Milliseconds(), 10)}
	if model := requestedModel(head); model != "" {
		parts = append(parts, "model="+model)
	}
	snap := audit.snapshot()
	if snap.served != "" {
		parts = append(parts, "served="+snap.served)
	}
	parts = append(parts, "attempts="+strconv.Itoa(snap.attempts))
	if snap.in > 0 || snap.out > 0 {
		parts = append(parts, "in="+strconv.Itoa(snap.in), "out="+strconv.Itoa(snap.out))
	}
	if snap.reasoning > 0 {
		parts = append(parts, "reasoning="+strconv.Itoa(snap.reasoning))
	}
	if snap.cacheRead > 0 {
		parts = append(parts, "cache_read="+strconv.Itoa(snap.cacheRead))
	}
	if agent != "" {
		parts = append(parts, "agent="+agent)
	}
	if snap.recovered {
		parts = append(parts, "recovered")
	}
	if snap.errCode != "" {
		parts = append(parts, "err="+snap.errCode)
		if snap.upRID != "" {
			parts = append(parts, "upstream_rid="+snap.upRID)
		}
	}
	level := "info"
	if status >= 400 {
		level = "warn"
	}
	s.logger.Logf(logx.CatAccess, level, "%s", strings.Join(parts, " "))
	if s.logger.Enabled(logx.CatContent) {
		s.logger.Logf(logx.CatContent, level, "rid=%s in=%s out=%s", rid,
			excerpt(head), excerpt(rec.head))
	}
}

// NoteCall observes one physical upstream attempt: it feeds the request's audit
// and writes the attempt's own line when the upstream class is switched on.
// Calls that belong to no client request (probes, background work) are ignored —
// the probe class already covers them.
func (s *Server) NoteCall(rec lane.CallRecord) {
	if s.logger == nil || rec.Trace == "" {
		return
	}
	audit := s.auditFor(rec.Trace)
	if audit == nil {
		return
	}
	n := audit.note(rec)
	kind := "lane"
	if strings.Contains(rec.Model, "/") {
		kind = "provider"
	}
	parts := []string{"rid=" + rec.Trace, "#" + strconv.Itoa(n), kind, "model=" + rec.Model}
	level := "info"
	if rec.ErrCode != "" {
		level = "warn"
		parts = append(parts, "ERR="+rec.ErrCode)
		if rec.UpstreamRID != "" {
			parts = append(parts, "upstream_rid="+rec.UpstreamRID)
		}
	} else {
		parts = append(parts, "ok")
	}
	if rec.Effort != "" {
		parts = append(parts, "effort="+rec.Effort)
	}
	if rec.TTFTMs > 0 {
		parts = append(parts, "ttft="+strconv.FormatInt(rec.TTFTMs, 10)+"ms")
	}
	if rec.Input > 0 || rec.Output > 0 {
		parts = append(parts, "in="+strconv.Itoa(rec.Input), "out="+strconv.Itoa(rec.Output))
	}
	if rec.Recovered {
		parts = append(parts, "recovered")
	}
	if rec.Truncated {
		parts = append(parts, "truncated")
	}
	s.logger.Logf(logx.CatUpstream, level, "%s", strings.Join(parts, " "))
}

var blobRe = regexp.MustCompile(`[A-Za-z0-9+/_=-]{48,}`)

// excerpt quotes at most 120 runes of a body for the content class, collapsing
// whitespace and replacing long base64-ish runs so an embedded image never
// reaches the log file.
func excerpt(b []byte) string {
	if len(b) == 0 {
		return `""`
	}
	s := blobRe.ReplaceAllString(string(b), "«blob»")
	var out strings.Builder
	runes := 0
	for _, r := range s {
		if runes >= 120 {
			break
		}
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			r = ' '
		case !unicode.IsPrint(r):
			continue
		}
		out.WriteRune(r)
		runes++
	}
	return strconv.Quote(strings.Join(strings.Fields(out.String()), " "))
}
