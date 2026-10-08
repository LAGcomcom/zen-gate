package lane

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The Kilo free pool: an OpenAI-compatible gateway whose free slice answers
// with no credential at all — no key, no login, no fingerprint gate. That
// makes this the simplest source the gateway aggregates: a fixed base URL, a
// plain POST per turn, and the same streaming discipline as the Zen lane.
// The trade is stated on the gateway's own model cards and belongs in front
// of the user: free-pool prompts may be logged by the upstream provider and
// used to improve its services, so this pool is for throwaway work, not
// secrets.
//
// The roster is the listing's isFree slice; everything else on this gateway
// is a paid id that would answer 401 here. Failures classify through the
// shared codes, with the endpoint absent from every message — one vocabulary
// keeps the adapter's handling uniform across sources.

// KiloBase is the gateway root; override for tests.
var KiloBase = "https://api.kilo.ai/api/gateway"

const (
	kiloTurnTimeout = 5 * time.Minute
	kiloUserAgent   = "zen-gate/1.4"
)

// kiloMandatoryReasoning matches families whose endpoint refuses to run with
// thinking switched off — their menus simply omit the off rung rather than
// promise one that fails the turn.
var (
	kiloMandatoryReasoning = []*regexp.Regexp{regexp.MustCompile(`^stepfun/`), regexp.MustCompile(`^liquid/`), regexp.MustCompile(`^thinkingmachines/`)}
	kiloNoDisableIDs       = map[string]bool{"kilo-auto/free": true, "openrouter/free": true}
)

// FetchKiloListing pulls the gateway's model listing and returns the free
// rows, already normalized: non-free ids answer 401 keyless, and a picker
// full of guaranteed refusals is worse than a short roster.
func FetchKiloListing(ctx context.Context) ([]map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, KiloBase+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("user-agent", kiloUserAgent)
	client := &http.Client{Timeout: 15 * time.Second, Transport: laneHTTP.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, ClassifyFailure(resp.StatusCode, string(data), 0)
	}
	var payload struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	for _, row := range payload.Data {
		if row["isFree"] == true && str(row["id"]) != "" {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func str(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func numOr(v any, fallback int) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	}
	return fallback
}

// BuildKiloCatalog turns listing rows into catalog entries. Rows that repeat
// an id are dropped; the thinking menu follows the families the pool actually
// honours (verified live upstream): reasoning.effort rides the request for
// every family that accepts the parameter, and the off rung exists only where
// the endpoint tolerates disabling thinking.
func BuildKiloCatalog(rows []map[string]any) []ModelInfo {
	seen := map[string]bool{}
	entries := []ModelInfo{}
	for _, row := range rows {
		id := str(row["id"])
		if id == "" || row["isFree"] != true || seen[id] {
			continue
		}
		seen[id] = true
		context := numOr(row["context_length"], 0)
		if context <= 0 {
			if tp, ok := row["top_provider"].(map[string]any); ok {
				context = numOr(tp["context_length"], 0)
			}
		}
		if context <= 0 {
			context = 131072
		}
		output := 32768
		if tp, ok := row["top_provider"].(map[string]any); ok {
			if n := numOr(tp["max_completion_tokens"], 0); n > 0 {
				output = n
			}
		}
		vision := false
		if arch, ok := row["architecture"].(map[string]any); ok {
			if mods, ok := arch["input_modalities"].([]any); ok {
				for _, m := range mods {
					if str(m) == "image" {
						vision = true
					}
				}
			}
		}
		declaresReasoning := false
		if params, ok := row["supported_parameters"].([]any); ok {
			for _, p := range params {
				if str(p) == "reasoning" {
					declaresReasoning = true
				}
			}
		}
		canDisable := declaresReasoning
		if canDisable {
			for _, re := range kiloMandatoryReasoning {
				if re.MatchString(id) {
					canDisable = false
				}
			}
			if kiloNoDisableIDs[id] {
				canDisable = false
			}
		}
		entries = append(entries, ModelInfo{
			ID:                 id,
			Name:               KiloDisplayName(row),
			Blurb:              "Kilo 免费池模型，无需任何凭据；免费池提示词可能被上游记录，别交机密内容",
			Wire:               "chat",
			Channel:            ChannelKilo,
			Vision:             vision,
			Reasoning:          declaresReasoning,
			ContextWindow:      context,
			MaxOutput:          output,
			CanDisableThinking: canDisable,
		})
	}
	return entries
}

// kiloVendorPrefixRe strips the listing's "Vendor: " lead-in.
var kiloVendorPrefixRe = regexp.MustCompile(`^[^:]{1,40}:\s+`)

// KiloDisplayName renders the listing's own name with the vendor prefix and
// the "(free)" qualifier stripped — the org/ prefix is routing detail, never
// a name the picker shows.
func KiloDisplayName(row map[string]any) string {
	name := str(row["name"])
	id := str(row["id"])
	bare := name
	if bare == "" {
		bare = id
	}
	bare = kiloVendorPrefixRe.ReplaceAllString(bare, "")
	bare = strings.TrimSpace(strings.TrimSuffix(bare, "(free)"))
	if bare == "" {
		bare = id
	}
	return bare
}

// PostKiloStreamed posts one completion turn and delivers decoded `data:`
// payloads to onData. The body shape is sniffed before the Content-Type is
// believed, the head bytes are replayed so no token is buffered, and an idle
// stream dies at the idle deadline (every chunk resets it; the gateway sends
// `: KILO PROCESSING` comment lines while the model is being scheduled, which
// the SSE reader skips and which keep the idle cutoff honest without tripping
// it).
func PostKiloStreamed(ctx context.Context, body map[string]any, onData func(payload []byte) error) (*Usage, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, &UpstreamError{Code: CodeTransport, Message: err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, KiloBase+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, &UpstreamError{Code: CodeTransport, Message: err.Error()}
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("accept", "text/event-stream")
	req.Header.Set("user-agent", kiloUserAgent)
	resp, err := laneHTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &UpstreamError{Code: CodeAborted, Message: "cancelled"}
		}
		return nil, &UpstreamError{Code: CodeTransport, Message: err.Error()}
	}
	defer resp.Body.Close()

	retryAfter := 0
	if ra := resp.Header.Get("retry-after"); ra != "" {
		if n, perr := strconv.Atoi(ra); perr == nil && n > 0 && n < 3600 {
			retryAfter = n
		}
	}

	pump := newStreamReader(ctx, resp.Body, kiloTurnTimeout)
	defer pump.Close()
	head := readUpTo(pump, 4096, 20*time.Second)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rest, _ := io.ReadAll(io.LimitReader(pump, 1<<20))
		return nil, ClassifyFailure(resp.StatusCode, string(head)+string(rest), retryAfter)
	}

	switch sniffBody(head) {
	case "empty":
		return nil, &UpstreamError{Code: CodeEmpty, Message: "empty response body"}
	case "json", "unknown":
		rest, _ := io.ReadAll(io.LimitReader(pump, 8<<20))
		full := string(head) + string(rest)
		var parsed any
		if jerr := json.Unmarshal([]byte(full), &parsed); jerr != nil {
			return nil, &UpstreamError{Code: CodeServer, Message: truncate(full, 300)}
		}
		if m, ok := parsed.(map[string]any); ok {
			if _, has := m["error"]; has {
				return nil, ClassifyKiloFailure(resp.StatusCode, full, retryAfter)
			}
		}
		if uerr := onData([]byte(full)); uerr != nil {
			return nil, uerr
		}
		if m, ok := parsed.(map[string]any); ok {
			return usageFromBody(m), nil
		}
		return nil, nil
	}

	pump.Unread(head)
	usage, err := readSSE(pump, onData)
	if err != nil {
		if ctx.Err() != nil {
			return usage, &UpstreamError{Code: CodeAborted, Message: "cancelled"}
		}
		if ue, ok := err.(*UpstreamError); ok {
			return usage, ue
		}
		return usage, &UpstreamError{Code: CodeTransport, Message: err.Error()}
	}
	return usage, nil
}

// ClassifyKiloFailure reshapes one of this gateway's refusals onto the shared
// vocabulary: the envelope names its class twice — error.code inside the
// error object and error_type beside it — while ClassifyFailure reads
// error.type. A paid id requested keyless is the caller picking a model this
// pool does not serve (a request defect, not a broken credential store), and
// an explicit moderation block is the upstream's answer, not a transport
// fault; both are request faults, not retryable.
func ClassifyKiloFailure(status int, payload string, retryAfterSec int) *UpstreamError {
	var shaped map[string]any
	if json.Unmarshal([]byte(payload), &shaped) == nil {
		if errObj, ok := shaped["error"].(map[string]any); ok {
			typ := str(errObj["code"])
			if et, ok := shaped["error_type"].(string); ok && et != "" {
				typ = et
			}
			if typ != "" {
				if PaidModelRefusalRe.MatchString(typ) {
					return &UpstreamError{Code: CodeClient,
						Message: "this model is not on the free pool: it needs a paid account, so it is not served by this source"}
				}
				errObj["type"] = typ
				shaped["error"] = errObj
				payloadBytes, _ := json.Marshal(shaped)
				payload = string(payloadBytes)
			}
		} else if et, ok := shaped["error_type"].(string); ok && et != "" {
			if PaidModelRefusalRe.MatchString(et) {
				return &UpstreamError{Code: CodeClient,
					Message: "this model is not on the free pool: it needs a paid account, so it is not served by this source"}
			}
			payload = `{"error":{"type":` + jsonString(et) + `}}`
		}
	}
	failure := ClassifyFailure(status, payload, retryAfterSec)
	if moderationRe.MatchString(failure.Message) {
		return &UpstreamError{Code: CodeClient, Message: failure.Message}
	}
	return failure
}

// PaidModelRefusalRe matches the pool's own code for a paid id requested
// keyless.
var PaidModelRefusalRe = regexp.MustCompile(`(?i)PAID_MODEL_AUTH_REQUIRED`)

var moderationRe = regexp.MustCompile(`(?i)moderation|flagged|filtered`)

// kiloEffortRung maps the gateway's three-rung ladder onto the pool's own
// effort field.
func kiloEffortRung(effort string) string {
	switch effort {
	case "light":
		return "low"
	case "deep":
		return "high"
	default:
		return "medium"
	}
}
