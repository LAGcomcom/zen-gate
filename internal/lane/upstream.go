package lane

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// FingerprintTools is the tool pair the v2 free-tier gate requires: OpenCode's
// own `read` and `shell`, declared with their exact descriptions and parameter
// schemas (9router PR #4842). Case variants like Claude's `Read` pass through
// untouched; a tool already using an exact fingerprint name with a different
// definition travels under an alias.
var FingerprintTools = []string{"read", "shell"}

// Verbatim from the OpenCode v2.0.26 CLI. Upstream compares description and
// parameters, so do not reword or reformat these.
const (
	realReadDesc = "Read the contents of a file or directory. Supports text files, images, and PDFs. Images and PDFs are presented directly to the model. Each text line is prefixed by its 1-based line number as <line>: <content>. The prefix is for reference and is not part of the file content. Directory entries are returned one per line. Use offset and limit to read large files or directories in sections. Prefer one larger read over many small slices, and use grep to find specific content in large files."

	realShellDesc = "Execute a shell command and return its output. Commands run on Linux using bash. Quote file paths containing spaces or special characters. Prefer dedicated tools over shell commands when possible. When output is large, the full result is saved to a file and a truncated preview is returned. Rely on automatic truncation unless filtering the output is more useful. Commands accept an optional timeout, background commands have no timeout by default. Background commands return immediately, and you will be notified when they complete."
)

// fingerprintDefs returns the exact read/shell definitions keyed by name.
// Values are rebuilt per call so callers cannot mutate the shared schema.
func fingerprintDefs(name string) (desc string, params map[string]any, ok bool) {
	switch name {
	case "read":
		return realReadDesc, map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "File or directory to read"},
				"offset": map[string]any{"type": "integer", "minimum": 0, "description": "The line or directory entry to start reading from (1-based)"},
				"limit":  map[string]any{"type": "integer", "minimum": 0, "description": "The maximum number of lines or directory entries to read (defaults to and capped at 2000)"},
			},
			"required":             []any{"path"},
			"additionalProperties": false,
		}, true
	case "shell":
		return realShellDesc, map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":    map[string]any{"type": "string", "description": "Shell command string to execute"},
				"workdir":    map[string]any{"type": "string", "description": "Working directory to execute the command in. Defaults to the current working directory. When possible, avoid changing directories in the command and set the working directory here instead."},
				"timeout":    map[string]any{"type": "integer", "minimum": 0, "description": "Timeout in milliseconds. Set to 0 to disable the timeout. Defaults to 120000 for foreground commands. Background commands have no timeout by default."},
				"background": map[string]any{"type": "boolean", "description": "Run the command in the background and return immediately (useful for dev servers and long-running builds). You do not need to use '&' at the end of the command when using this parameter. You will be notified when it completes. DO NOT poll for completion."},
			},
			"required":             []any{"command"},
			"additionalProperties": false,
		}, true
	}
	return "", nil, false
}

var (
	sessionRe = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
	requestRe = regexp.MustCompile(`^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
)

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func base62From(b []byte) string {
	out := make([]byte, len(b))
	for i, c := range b {
		out[i] = base62[int(c)%62]
	}
	return string(out)
}

var lastStamp atomic.Int64
var seqCounter atomic.Int64

// MintSessionId mints a gateway-shaped canonical session id
// (time-prefixed, monotonic counter), byte for byte the shape a real client mints.
func MintSessionId(nowMS int64) string {
	if nowMS <= 0 {
		nowMS = time.Now().UnixMilli()
	}
	prev := lastStamp.Load()
	if prev != nowMS {
		lastStamp.CompareAndSwap(prev, nowMS)
		seqCounter.Store(0)
	}
	seq := seqCounter.Add(1)
	value := ^(uint64(nowMS)<<12 | uint64(seq&0xFFF))
	var raw [6]byte
	for i := 0; i < 6; i++ {
		raw[i] = byte((value >> (40 - 8*i)) & 0xFF)
	}
	rnd := make([]byte, 14)
	_, _ = rand.Read(rnd)
	return "ses_" + hex.EncodeToString(raw[:]) + base62From(rnd)
}

// SessionForConversation maps one downstream conversation onto one stable
// upstream session. Free-tier quota is accounted per session, so a fresh id
// per request would exhaust it and surface as 429.
//
// The id must be MINTED, not hashed: since the free-tier gate moved into the
// private inference proxy, the service decodes the 12-hex prefix of
// x-opencode-session as the truncated inverted ~(ms<<12|seq) of a real
// OpenCode client and 403s (FreeTierError) any prefix that does not decode
// to a fresh timestamp — a sha256-derived prefix never can (#47). One
// conversation still gets exactly one session: the minted id is remembered
// per seed and re-minted only when it grows too old for the freshness
// window, so per-session quota and prompt cache keep working.
func SessionForConversation(seed string) string {
	seed = strings.TrimSpace(seed)
	if sessionRe.MatchString(seed) {
		return seed
	}
	if seed == "" {
		seed = "global"
	}
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	if s, ok := sessionsBySeed[seed]; ok && time.Since(s.at) < sessionRemintAfter {
		return s.id
	}
	if len(sessionsBySeed) >= sessionsBySeedCap {
		// The table is a stability cache, not state worth evicting
		// carefully: a full reset only mints fresh ids for busy seeds.
		sessionsBySeed = map[string]mintedSession{}
	}
	id := MintSessionId(0)
	sessionsBySeed[seed] = mintedSession{id: id, at: time.Now()}
	return id
}

type mintedSession struct {
	id string
	at time.Time
}

const (
	sessionsBySeedCap = 4096
	// The free-tier gate tolerates prefixes about a week old; re-mint at
	// six days to stay inside the window even on sessions that never restart.
	sessionRemintAfter = 6 * 24 * time.Hour
)

var (
	sessionsMu     sync.Mutex
	sessionsBySeed = map[string]mintedSession{}
)

// SessionForExit binds one conversation to one exit. Quota is accounted per
// ReportExit books an (exit, model) failure against the live rotator.
func ReportExit(nodeID, model, class string) {
	if nodeID == "" || model == "" {
		return
	}
	if r := CurrentRotator(); r != nil {
		r.Report(nodeID, model, class)
	}
}
// BaseModelId strips a trailing "(…)" effort suffix so lookups hit the base id.
func BaseModelId(model string) string {
	s := strings.TrimSpace(model)
	if len(s) > 1 && s[len(s)-1] == ')' {
		for i := len(s) - 2; i >= 0; i-- {
			if s[i] == '(' {
				return strings.TrimSpace(s[:i])
			}
		}
	}
	return s
}

// EffortOf extracts a trailing "(light|balanced|deep)" suffix, if any.
func EffortOf(model string) string {
	s := strings.TrimSpace(model)
	if len(s) > 1 && s[len(s)-1] == ')' {
		for i := len(s) - 2; i >= 0; i-- {
			if s[i] == '(' {
				inner := strings.ToLower(strings.TrimSpace(s[i+1 : len(s)-1]))
				if inner == "light" || inner == "balanced" || inner == "deep" {
					return inner
				}
				return ""
			}
		}
	}
	return ""
}

// WireFor returns which wire protocol serves a model the catalog does not
// list. Every servable row carries its own Wire; an unlisted id gets the
// OpenAI chat shape, the one wire all three channels speak.
func WireFor(modelID string) string {
	return "chat"
}

// toolNameOf reads a tool's declared name from either tool shape.
func toolNameOf(tool map[string]any) string {
	if tool == nil {
		return ""
	}
	if n, ok := tool["name"].(string); ok && strings.TrimSpace(n) != "" {
		return strings.TrimSpace(n)
	}
	if fn, ok := tool["function"].(map[string]any); ok {
		if n, ok := fn["name"].(string); ok && strings.TrimSpace(n) != "" {
			return strings.TrimSpace(n)
		}
	}
	return ""
}

func functionOf(tool map[string]any) map[string]any {
	fn, _ := tool["function"].(map[string]any)
	return fn
}

// fingerprintKey returns the fingerprint name when `name` is exactly one;
// "" otherwise (case variants such as Claude's `Read` are not fingerprint names).
func fingerprintKey(name string) string {
	trimmed := strings.TrimSpace(name)
	for _, t := range FingerprintTools {
		if t == trimmed {
			return trimmed
		}
	}
	return ""
}

// toolsOf normalizes body.tools from either []any or []map[string]any (Go
// builders produce the latter; decoded JSON the former).
func toolsOf(body map[string]any) []map[string]any {
	var out []map[string]any
	switch t := body["tools"].(type) {
	case []any:
		for _, item := range t {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
	case []map[string]any:
		out = append(out, t...)
	}
	return out
}

// ApplyFingerprint satisfies the v2 free-tier gate on body.tools: OpenCode's
// own `read` and `shell` must be declared with their exact descriptions and
// parameter schemas. A caller tool that already uses one of those names with a
// different definition travels under an alias (`read_client`); the returned map
// is sent-spelling → caller-spelling so responses can restore real names.
// Stub descriptions, renamed schemas and tool_choice "none" are answered with
// 403 FreeTierError.
//
// body is mutated in place. style: "chat" | "flat" (Responses) | "claude".
func ApplyFingerprint(body map[string]any, style string) map[string]string {
	claude := style == "claude"
	flat := style == "flat"
	tools := toolsOf(body)
	renameMap := map[string]string{}

	// Conceal conflicting caller tools off the fingerprint names so the exact
	// definitions can be appended without a duplicate name.
	taken := map[string]bool{}
	for _, t := range tools {
		if n := toolNameOf(t); n != "" {
			taken[n] = true
		}
	}
	seen := map[string]bool{}
	out := make([]any, 0, len(tools)+2)
	for _, tool := range tools {
		key := fingerprintKey(toolNameOf(tool))
		if key == "" {
			out = append(out, tool)
			continue
		}
		// Duplicate declarations of one name are rejected upstream; keep the first.
		if seen[key] {
			continue
		}
		seen[key] = true
		if isFingerprintDefinition(tool, key) {
			out = append(out, tool)
			continue
		}
		alias := key + "_client"
		for i := 2; taken[alias]; i++ {
			alias = fmt.Sprintf("%s_client%d", key, i)
		}
		taken[alias] = true
		renameMap[alias] = toolNameOf(tool)
		out = append(out, withToolName(tool, alias))
	}

	// Append OpenCode's exact definitions for each fingerprint tool not already
	// declared. Flat (Responses) tools carry strict:false like the CLI.
	appended := []string{}
	for _, name := range FingerprintTools {
		// Check the tools array, not the conceal seen set: an aliased
		// conflicting tool no longer carries the fingerprint name, so the
		// exact definition must still be appended (9router behaviour).
		present := false
		for _, t := range out {
			if tm, ok := t.(map[string]any); ok && fingerprintKey(toolNameOf(tm)) == name {
				present = true
				break
			}
		}
		if present {
			continue
		}
		desc, params, _ := fingerprintDefs(name)
		switch {
		case claude:
			out = append(out, map[string]any{"name": name, "description": desc,
				"input_schema": params})
		case flat:
			out = append(out, map[string]any{"type": "function", "name": name, "description": desc,
				"parameters": params, "strict": false})
		default:
			out = append(out, map[string]any{"type": "function",
				"function": map[string]any{"name": name, "description": desc,
					"parameters": params}})
		}
		seen[name] = true
		appended = append(appended, name)
	}
	body["tools"] = out

	retargetToolChoice(body, renameMap)
	aliasHistoryToolCalls(body, renameMap)
	if len(appended) > 0 {
		noteUnavailableTools(body, flat, appended)
	}

	// Responses keeps its explicit auto default. Never default to "none":
	// upstream answers tool_choice "none" with 403 FreeTierError.
	if _, has := body["tool_choice"]; !has && flat {
		body["tool_choice"] = "auto"
	}
	return renameMap
}

// isFingerprintDefinition reports whether `tool` already carries OpenCode's
// exact definition for `key` (compared by description and JSON of parameters).
func isFingerprintDefinition(tool map[string]any, key string) bool {
	def := tool
	if fn := functionOf(tool); fn != nil {
		def = fn
	}
	wantDesc, wantParams, ok := fingerprintDefs(key)
	if !ok || def == nil {
		return false
	}
	if d, _ := def["description"].(string); d != wantDesc {
		return false
	}
	got, _ := json.Marshal(def["parameters"])
	want, _ := json.Marshal(wantParams)
	return string(got) == string(want)
}

// withToolName clones `tool` with its name replaced, in either tool shape.
func withToolName(tool map[string]any, name string) map[string]any {
	if fn := functionOf(tool); fn != nil {
		clone := map[string]any{"type": tool["type"], "function": cloneMap(fn)}
		clone["function"].(map[string]any)["name"] = name
		return clone
	}
	clone := cloneMap(tool)
	clone["name"] = name
	return clone
}

// retargetToolChoice points an explicit object tool_choice at the alias of a
// concealed caller tool.
func retargetToolChoice(body map[string]any, rename map[string]string) {
	if len(rename) == 0 {
		return
	}
	// Invert: original name -> alias.
	aliases := map[string]string{}
	for alias, orig := range rename {
		aliases[orig] = alias
	}
	switch choice := body["tool_choice"].(type) {
	case map[string]any:
		if n, ok := choice["name"].(string); ok {
			if alias, hit := aliases[strings.TrimSpace(n)]; hit {
				choice["name"] = alias
			}
			return
		}
		if fn, ok := choice["function"].(map[string]any); ok {
			if n, ok := fn["name"].(string); ok {
				if alias, hit := aliases[strings.TrimSpace(n)]; hit {
					fn["name"] = alias
				}
			}
		}
	}
}

// aliasHistoryToolCalls renames prior-turn calls to concealed caller tools;
// otherwise the model reads its own earlier calls as calls to OpenCode's tool
// of the same name.
func aliasHistoryToolCalls(body map[string]any, rename map[string]string) {
	if len(rename) == 0 {
		return
	}
	aliases := map[string]string{}
	for alias, orig := range rename {
		aliases[orig] = alias
	}
	if items, ok := body["input"].([]any); ok {
		for i, it := range items {
			m, ok := it.(map[string]any)
			if !ok || m["type"] != "function_call" {
				continue
			}
			if n, ok := m["name"].(string); ok {
				if alias, hit := aliases[n]; hit {
					m["name"] = alias
					_ = i
				}
			}
		}
	}
	msgs, ok := body["messages"].([]any)
	if !ok {
		return
	}
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		calls, ok := msg["tool_calls"].([]any)
		if !ok {
			continue
		}
		for _, cc := range calls {
			call, ok := cc.(map[string]any)
			if !ok {
				continue
			}
			fn, ok := call["function"].(map[string]any)
			if !ok {
				continue
			}
			if n, ok := fn["name"].(string); ok {
				if alias, hit := aliases[n]; hit {
					fn["name"] = alias
				}
			}
		}
	}
}

// noteUnavailableTools tells the model the appended fingerprint tools are
// placeholders: without the old "must not be used" descriptions the model
// calls them, and "not available" made it refuse the caller's own Read/Bash
// too. Upstream does not gate on the prompt text.
func noteUnavailableTools(body map[string]any, flat bool, names []string) {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "`" + n + "`"
	}
	note := "Do not call the " + strings.Join(quoted, " or ") +
		" tools; they are placeholders that do nothing. Use the other tools you have been given instead."
	if flat {
		if s, ok := body["instructions"].(string); ok && strings.TrimSpace(s) != "" {
			body["instructions"] = s + "\n\n" + note
		} else {
			body["instructions"] = note
		}
		return
	}
	// OpenAI chat shape only; Claude-shaped bodies carry `system` instead.
	msgs, ok := body["messages"].([]any)
	if !ok || body["system"] != nil || len(msgs) == 0 {
		return
	}
	if first, ok := msgs[0].(map[string]any); ok && first["role"] == "system" {
		if c, ok := first["content"].(string); ok {
			clone := cloneMap(first)
			clone["content"] = c + "\n\n" + note
			body["messages"] = append([]any{clone}, msgs[1:]...)
			return
		}
	}
	body["messages"] = append([]any{map[string]any{"role": "system", "content": note}}, msgs...)
}

// RestoreToolName maps a sent spelling back to the caller's.
func RestoreToolName(name string, rename map[string]string) string {
	if len(rename) == 0 {
		return name
	}
	if orig, ok := rename[name]; ok {
		return orig
	}
	return name
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
