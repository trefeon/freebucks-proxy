package upstream

import (
	"context"
	cryptoRand "crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ChatOptions carries the envelope values for a chat completion request.
type ChatOptions struct {
	Model             string
	RunID             string
	SessionInstanceID string // "" when the session is disabled
	// RequestID is the server's per-request correlation id (D1): the
	// access wrapper mints it once and threads it here so the client's
	// do()/retry log lines (upstream ok/error/transient/retry) share the
	// server's req_id. Never sent upstream.
	RequestID string
	// TraceSessionID is the per-run trace id minted once by the run manager
	// (crypto/rand UUID) and reused across the run's requests, mirroring the
	// CLI (run.ts: previousRun?.traceSessionId ?? randomUUID). Injected as
	// codebuff_metadata["trace_session_id"] when set.
	TraceSessionID string
	// ClientID is codebuff_metadata["client_id"], minted once per run by the
	// run manager and repeated by every chat call of that run (CLI: one
	// promptId per prompt, run.ts:722/822). Empty falls back to a fresh draw
	// so callers without a run manager still send a well-shaped id.
	ClientID string
	// AgentID is the run's root agent id (e.g. base2-free-luna,
	// base3-free-luna). It selects which canonical root identity opens the
	// system prompt: base3-free-* roots speak the base3 sentence (agents/
	// base3.ts), everything else keeps the base2 one. Empty = base2 marker.
	AgentID string
	// AgentMode is the CLI agent mode (DEFAULT/LITE/MAX/PLAN) selecting
	// codebuff_metadata.cost_mode per AGENT_MODE_TO_COST_MODE
	// (upstream/freebuff cli/src/utils/constants.ts:182-187):
	// LITE=free, DEFAULT/PLAN=normal, MAX=max. Empty falls back to the
	// client's configured cost mode so callers without a mode keep the
	// legacy behavior.
	AgentMode string
	// RepoSnapshot is the JSON-encoded aggregate repository snapshot
	// (the REPO_SNAPSHOT_FIELDS projection: counts and bounded enums
	// only, never paths/patches/branches) stamped verbatim as
	// codebuff_metadata.repo_snapshot, exactly like the CLI's
	// JSON.stringify(repoSnapshot) (upstream/freebuff sdk/src/run.ts:
	// 1002-1046). Empty means absent. The proxy never fabricates one:
	// with no repo access there is nothing honest to send.
	RepoSnapshot string
	// StepNumber is the 1-based per-run agent step counter (CLI parity:
	// llm_step_number is merged on every chat call, String(n);
	// upstream/freebuff agent-runtime run-agent-step.ts:1175-1177).
	// Injected as codebuff_metadata["llm_step_number"] when > 0; the run
	// manager sets it per chat call at the server construction sites.
	StepNumber int
	// N is codebuff_metadata["n"] (llm.ts:118 `...(n && {n})`). 0 means
	// absent (mirrors JS truthiness). CLI forwards it when sampling multiple
	// completions.
	N int
	// CacheDebugCorrelation is codebuff_metadata["cache_debug_correlation"]
	// (llm.ts:120-122). Empty means absent.
	CacheDebugCorrelation string
	// ExtraCodebuffMetadata is the CLI's extraCodebuffMetadata spread
	// (llm.ts:115 `...(extraCodebuffMetadata ?? {})`) — caller-supplied
	// keys merged BEFORE reserved identifiers so reserved keys win.
	ExtraCodebuffMetadata map[string]string
	// OMPFloorOnly marks an OMP-family floor-only request
	// (convert ToolMapper.FloorOnly: the client defs were replaced by the
	// 16 canonical wire definitions). It gates ONLY the harness-only
	// capability reminder appended to the prepended system marker
	// (appendOMPFloorReminder): the 16+end_turn wire is untouched, and
	// every other family keeps the zero value (byte-identical). Set at the
	// server relay construction site from the request's own mapper — never
	// recomputed here from the body (already normalized: family detection
	// would read familyNone).
	OMPFloorOnly bool
}

// ChatCompletions POSTs an OpenAI-shaped request to the upstream chat
// endpoint, injecting the CLI envelope, and returns the raw SSE body reader
// on 2xx. On error status it drains (up to 500 chars), classifies, and
// returns a typed error. The returned reader must be closed; closing it
// releases the connection.
//
// Single attempt: a classified gate error (waiting room, capacity,
// superseded, run refusals, rate limits, bans) is returned immediately —
// the CLI never retries a refused turn in-request (send-message.ts
// fails the turn; the 30s session poll resyncs and the next message mints
// a fresh run). Transport-level failures (dial/TLS/reset/EOF) keep the
// fresh-connection retry under TRANSIENT_RETRIES inside do().
func (c *Client) ChatCompletions(ctx context.Context, opts ChatOptions, body []byte) (io.ReadCloser, error) {
	// D1: thread the server's correlation id into the request context so
	// every do()/retry log line for this chat shares the server's req_id.
	if opts.RequestID != "" {
		ctx = withReqID(ctx, opts.RequestID)
	}
	if c.requestJitter > 0 {
		var b [8]byte
		_, _ = cryptoRand.Read(b[:])
		u := binary.BigEndian.Uint64(b[:])
		jitterNano := int64(u % uint64(c.requestJitter))
		timer := time.NewTimer(time.Duration(jitterNano))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
	}

	enveloped, err := injectEnvelope(body, c.costMode, opts)
	if err != nil {
		return nil, fmt.Errorf("upstream: envelope: %w", err)
	}
	// Catalog mode (vendor freebuff-catalog-agent.ts + codebuff-client.ts
	// requestHeaders hook): the turn runs as the row's HANDLE, not the raw
	// id, and the POST carries the held fetch id plus a device signature
	// over the exact bytes sent. Nil catalog is fallback — the raw id with
	// no extra headers, today's exact shape.
	held := c.heldModelCatalog()
	if held != nil {
		if handle := chatHandleFor(held, opts, enveloped); handle != "" {
			enveloped = rewriteChatModel(enveloped, handle)
		}
	}

	// No in-request retry: a refused turn fails here and recovery happens
	// at the engine level with a fresh run (CLI parity). Re-POSTing the
	// same session burns a new admission on 409-refunded rows and escalates
	// 503-wait loops into terminal + account bans.
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v1/chat/completions", enveloped)
	if err != nil {
		return nil, err
	}
	// The streamed response body must stay readable after this call
	// returns, so no deadline is attached to the request context: the
	// transport's ResponseHeaderTimeout (REQUEST_TIMEOUT) bounds only
	// the wait for response headers, and the body streams until
	// upstream EOF or the caller cancels (client disconnect). cancel
	// stays nil here; cancelBody exists so a future deadline-based
	// caller still gets correct release-on-close semantics.
	var cancel context.CancelFunc
	// No Accept header: the CLI's chat POST carries exactly Authorization +
	// the ai-sdk UA (+ optional acting-user-id) via the ai-sdk fetch
	// (model-provider.ts); the proxy's old
	// "Accept: application/json, text/event-stream" was proxy-only and is
	// removed until a capture proves the wire carries it.
	// Chat is the ONLY path carrying the ai-sdk UA: the real
	// CLI pins it on model calls alone; newRequest defaulted this
	// request to the plain Bun fetch UA every other call sends.
	req.Header.Set("User-Agent", cliUserAgent)
	// The chat POST carries NO x-freebuff-model / x-freebuff-instance-id
	// headers (#106): the official CLI sends exactly Authorization + the
	// ai-sdk UA (+ optional acting-user-id) on chat
	// (upstream/freebuff model-provider.ts:146-152); the model and
	// instance id ride only in the body metadata (injectEnvelope).
	if held != nil {
		// The completion headers (vendor freebuffCatalogCompletionHeaders):
		// the fetch id the handle came from, plus the device signature
		// over the exact enveloped bytes. Unsigned when unregistered —
		// the same degradation as the session path.
		if held.FetchID != "" {
			req.Header.Set(CatalogFetchHeader, held.FetchID)
		}
		for name, value := range c.deviceHeaders(ctx, http.MethodPost, req.URL.String(), enveloped, held.FetchID, true) {
			req.Header.Set(name, value)
		}
	}
	if c.userID != "" {
		// The official CLI sends x-freebuff-acting-user-id on every
		// chat call with the account's OWN id, derived from
		// GET /api/v1/me (upstream/freebuff sdk/src/run.ts:649-658;
		// sdk/src/impl/model-provider.ts:148-153 — agent-runs
		// START/FINISH carry it too, database.ts:318-320/396-398).
		// The server treats it as a trusted server-to-server header
		// honored only when the request authenticates as the FreeBuff
		// Web service account (upstream/freebuff
		// common/src/constants/freebuff-models.ts:1180-1183).
		// ACTING_USER_ID is therefore only safe when it equals the
		// token's own account id; any other value impersonates a
		// foreign user (a possible flag).
		req.Header.Set("x-freebuff-acting-user-id", c.userID)
	}
	resp, _, cerr := c.do(req, 0)
	if cerr != nil && resp == nil {
		// Transport failure (no upstream response read): surface it.
		releaseCancel(cancel)
		return nil, cerr
	}
	if cerr != nil {
		// Classified >=400 response: do() already classified the body once
		// (the 428 waiting-room flag and the rate-limit ledger are
		// recorded). Preserve the chat path's debug dump, then fail the
		// turn immediately — no same-session wait-and-retry (CLI parity:
		// send-message.ts:575-631 has no 503 arm; the turn fails, the 30s
		// poll resyncs, and the next message mints a fresh run).
		bodyText := drainBody(resp.Body)
		_ = resp.Body.Close()
		releaseCancel(cancel)
		c.dump("chat", req, enveloped, resp.StatusCode, bodyText)
		return nil, cerr
	}
	// Callers MUST close the returned body to release the timeout
	// context; abandoning it leaks the timer goroutine until it fires.
	// Served turn (2xx headers): stamp chat-surface ad activity. A due
	// round runs detached in the background - chat latency and the
	// body below are untouched. The enveloped body carries the served
	// turn's transcript so the cli_chat auction mirrors the CLI's
	// user+assistant messages (waiting-room passes nil → []).
	c.noteChatServedWithBody(ctx, enveloped)
	c.dump("chat", req, enveloped, resp.StatusCode, "[streaming 2xx]")
	return &cancelBody{ReadCloser: resp.Body, cancel: cancel}, nil
}

const (
	// cliSystemMarker is the base2 root identity prepended at position 0 of
	// the first system message. Its leading sentence is canonical opening #1
	// of FREEBUFF_ROOT_SYSTEM_PROMPT_OPENINGS (pinned free-agents.ts:693-722,
	// agents/base2/base2.ts createBase2('free', …)).
	cliSystemMarker       = "You are Buffy, the strategic coding assistant. You are the AI agent behind the product, Freebuff, a tool where users can chat with you to code with AI for free."
	cliSystemMarkerPhrase = "You are Buffy, the strategic coding assistant"
	// cliSystemMarkerBase3 is canonical opening #2 (agents/base3.ts
	// createBase3(…)): every base3-free-* Web/Cloud/CLI root composes its
	// prompt onto this sentence. PR #207 routes Luna (and vendor cce4800 Ox
	// Alpha) onto base3 roots, so runs on those agents must open with THEIR
	// canonical identity, not base2's.
	cliSystemMarkerBase3 = "You are Buffy, the coding agent behind Codebuff."
	// cliSystemMarkerBase3Instructions is the canonical base3 coding agent instructions block
	// from upstream agents/base3.ts. Upstream's free-mode gate checks for this instructions block;
	// sending only a single opening sentence triggers a 503 "The model is temporarily unavailable" refusal.
	cliSystemMarkerBase3Instructions = `You are Buffy, the coding agent behind Codebuff. You help users with software engineering tasks: fixing bugs, adding functionality, refactoring, and explaining code.

Current date: %s.

- Match the project's existing conventions. Verify a library is already used in the project before employing it.
- Prefer editing existing files over creating new ones. Make the fewest changes that address the request.
- Verify non-trivial changes by running the project's typecheck and relevant tests.
- Use write_todos to plan and track multi-step tasks.
- Your responses are displayed in a terminal. Keep them short and concise.
- Don't run destructive or hard-to-undo commands (git push, resets, deploys) unless the user asks for them.

# Working with the user

- **Ask about important decisions:** Use the ask_user tool to collaborate with the user on non-obvious choices — alternate implementation strategies, ambiguous requirements. Gather context first, and skip it when the answer is obvious or the detail can be changed later.
- **Suggest next steps:** At the end of your turn, use the suggest_followups tool to suggest ~3 next steps the user might want to take. Keep every suggestion short and goal-oriented: one sentence naming the outcome you want, not the steps to get there.

# Freebuff Meta-information

You are running on the %s model.

You are the AI agent behind Freebuff, a tool where users can chat with you to code with AI for free. See freebuff.com for more information about the product.

# System Info

Operating System: %s
Shell: bash
Chrome: installed

<user_shell_config_files>
</user_shell_config_files>

The following are the most recently read files according to the OS atime. This is cached from the start of this conversation:
<recently_read_file_paths_most_recent_first>
</recently_read_file_paths_most_recent_first>`
)

// cliSystemGateOpenings mirrors FREEBUFF_ROOT_SYSTEM_PROMPT_OPENINGS (pinned
// free-agents.ts:693-722): the free-mode gate is an any-of-five trimmed
// PREFIX test at position 0 (hasFreebuffRootSystemPromptOpening), so a request
// that already opens with ANY canonical identity must be left untouched —
// prepending would corrupt a prompt that already passes the gate.
var cliSystemGateOpenings = []string{
	cliSystemMarkerPhrase,
	"You are Buffy, the coding agent behind Codebuff.",
	"You are Buffy, the Freebuff Cloud project planner.",
	"You are Buffy, the auto-run agent behind Freebuff Desktop.",
	"You are Buffy, a strategic assistant that orchestrates complex coding tasks through specialized sub-agents.",
}

// foreignHarnessPromptMarkers mirrors upstream FOREIGN_HARNESS_PROMPT_MARKERS
// (foreign-client-signals.ts:183-188). Upstream inspects all system-role
// messages for these markers and rejects matching requests as foreign.
var foreignHarnessPromptMarkers = []string{
	"You are Claude Code",
	"Anthropic's official CLI",
	"cc_version=",
	"cc_entrypoint=",
	"You are Kimi Code CLI",
	"You are Hermes Agent, built by Nous Research",
	"You are a general-purpose AI agent called goose",
	"You are an expert on the AI coding tool called Aider",
	"Gemini CLI",
	"Generated with Crush",
	"Assisted-by: Crush",
	"Co-Authored-By: Crush",
	"Co-Authored-By: Claude Code",
	"*** Begin Patch",
	"*** End Patch",
}

// sanitizeForeignPromptMarkers replaces foreign harness prompt markers in
// s with a neutral placeholder, preventing upstream foreign_system_prompt
// detection while preserving surrounding instructions.
func sanitizeForeignPromptMarkers(s string) string {
	for _, marker := range foreignHarnessPromptMarkers {
		if strings.Contains(s, marker) {
			s = strings.ReplaceAll(s, marker, "")
		}
	}
	return s
}

// sanitizeSystemMessageContent scrubs foreign harness markers from
// content, handling both plain string content and array-of-parts content.
func sanitizeSystemMessageContent(content any) any {
	switch v := content.(type) {
	case string:
		return sanitizeForeignPromptMarkers(v)
	case []any:
		cleanedParts := make([]any, len(v))
		for i, part := range v {
			if partMap, ok := part.(map[string]any); ok {
				if txt, ok := partMap["text"].(string); ok {
					copyMap := make(map[string]any, len(partMap))
					for k, val := range partMap {
						copyMap[k] = val
					}
					copyMap["text"] = sanitizeForeignPromptMarkers(txt)
					cleanedParts[i] = copyMap
					continue
				}
			}
			cleanedParts[i] = part
		}
		return cleanedParts
	default:
		return content
	}
}

// systemMarkerFor picks the canonical identity matching the run's root agent
// family: base3 roots speak base3, everything else keeps the base2 marker.
func systemMarkerFor(agentID, model string) string {
	if strings.HasPrefix(agentID, "base3") {
		if model == "" {
			model = "z-ai/glm-5.3-flash"
		}
		osName := "win32"
		switch runtime.GOOS {
		case "darwin":
			osName = "darwin"
		case "linux":
			osName = "linux"
		}
		return fmt.Sprintf(cliSystemMarkerBase3Instructions, time.Now().Format("January 2, 2006"), model, osName)
	}
	return cliSystemMarker
}

// hasCanonicalOpening reports whether content already begins with one of the
// five gate openings after trimming leading whitespace (the gate tolerates
// template-literal trim differences, nothing else).
func hasCanonicalOpening(content string) bool {
	trimmed := strings.TrimLeft(content, " \t\n\r")
	for _, opening := range cliSystemGateOpenings {
		if strings.HasPrefix(trimmed, opening) {
			return true
		}
	}
	return false
}

// ensureCliSystemMarker guarantees the run's canonical "You are Buffy…"
// opening at byte position 0 of the first system message (the free-mode
// gate's trimmed prefix test — see the check loop below). It prepends the
// marker rather than replacing, so custom system instructions survive. A
// message that already opens with ANY of the five canonical identities is
// left alone regardless of agentID: the gate is any-of-five.
func ensureCliSystemMarker(payload map[string]any, agentID string, model ...string) {
	m := ""
	if len(model) > 0 {
		m = model[0]
	}
	marker := systemMarkerFor(agentID, m)
	rawMsgs, ok := payload["messages"].([]any)
	if !ok || len(rawMsgs) == 0 {
		payload["messages"] = []any{
			map[string]any{"role": "system", "content": marker},
		}
		return
	}

	// Step 1: sanitize all system messages against foreign harness prompt markers.
	for i, m := range rawMsgs {
		msg, ok := m.(map[string]any)
		if !ok || msg["role"] != "system" {
			continue
		}
		if content, exists := msg["content"]; exists {
			msg["content"] = sanitizeSystemMessageContent(content)
			rawMsgs[i] = msg
		}
	}

	// Step 2: check if any system message already has a canonical opening.
	for _, m := range rawMsgs {
		msg, ok := m.(map[string]any)
		if !ok || msg["role"] != "system" {
			continue
		}
		// The server gate is a TRIMMED PREFIX test at position 0
		// (hasFreebuffRootSystemPromptOpening, free-agents.ts:739-744),
		// hardened against the prepend-and-cancel proxy trick: a message
		// that merely mentions the phrase mid-string must NOT suppress
		// the canonical prefix (#110).
		if content, ok := msg["content"].(string); ok && hasCanonicalOpening(content) {
			return // already canonical
		}
		if parts, ok := msg["content"].([]any); ok {
			for _, p := range parts {
				if partMap, ok := p.(map[string]any); ok {
					if txt, ok := partMap["text"].(string); ok && hasCanonicalOpening(txt) {
						return // already canonical
					}
				}
			}
		}
	}

	// Step 3: Not present. Merge into first system message if exists, else unshift.
	for i, m := range rawMsgs {
		msg, ok := m.(map[string]any)
		if !ok || msg["role"] != "system" {
			continue
		}
		if str, ok := msg["content"].(string); ok {
			trimmed := strings.TrimSpace(str)
			if trimmed == "" {
				msg["content"] = marker
			} else {
				msg["content"] = marker + "\n\n" + str
			}
		} else if parts, ok := msg["content"].([]any); ok {
			msg["content"] = append([]any{map[string]any{"type": "text", "text": marker}}, parts...)
		} else {
			msg["content"] = marker
		}
		rawMsgs[i] = msg
		payload["messages"] = rawMsgs
		return
	}

	newMsgs := append([]any{map[string]any{"role": "system", "content": marker}}, rawMsgs...)
	payload["messages"] = newMsgs
}

// ompFloorReminderSentinel guards appendOMPFloorReminder idempotency: the
// reminder is appended once per body, never duplicated on re-injection.
const ompFloorReminderSentinel = "callable by name with these exact arguments though absent from tools"

// ompFloorCapabilityReminder names the harness-only tools for OMP-family
// floor-only requests. The gate rejects any foreign-schema definition riding
// the wire, so these tools are deliberately absent from tools[] — but live
// MITM proved models never emit undeclared tools and fumble the shapes of
// the ones they reach from prompt vocabulary alone (harness "Missing tasks"
// on singular {agent, task} emissions). This paragraph restores the
// names+shapes in prose, where the gate is exonerated (system text with the
// full 77 KB OMP prompt returns 200). OMP-only: every other family keeps the
// zero ChatOptions and never sees it.
const ompFloorCapabilityReminder = `Harness tools callable by name with these exact arguments though absent from tools (no foreign definitions may ride the wire):
- task {"tasks":[{"name?,"agent?,"task"}],"context?"} — tasks must be a non-empty array.
- todo op-machine {"op",...}: init {list:[{phase,items:[string]}]}, done {task}, view.
- ask {"questions":[{"id","question","options"}]} — every question needs its id.
- eval {"language","code"}; learn {"memory"}; manage_skill {"action",...}.
- hub {"op":"wait","job_id",...} — the only way to pause for a background job.
- advise {"note","severity"?} — nit|concern|blocker, omit severity for plain nit.`

// appendOMPFloorReminder appends ompFloorCapabilityReminder to the first
// system message (string content gains a trailing paragraph, parts content a
// trailing text part). Gate-safe: the canonical opening stays the trimmed
// prefix at position 0, so already-canonical bodies keep passing. Total:
// missing/non-system messages are left alone, and a body that already
// carries the reminder is untouched.
func appendOMPFloorReminder(payload map[string]any) {
	rawMsgs, ok := payload["messages"].([]any)
	if !ok || len(rawMsgs) == 0 {
		return
	}
	for i, m := range rawMsgs {
		msg, ok := m.(map[string]any)
		if !ok || msg["role"] != "system" {
			continue
		}
		switch content := msg["content"].(type) {
		case string:
			if strings.Contains(content, ompFloorReminderSentinel) {
				return
			}
			msg["content"] = content + "\n\n" + ompFloorCapabilityReminder
		case []any:
			for _, p := range content {
				if partMap, ok := p.(map[string]any); ok {
					if txt, ok := partMap["text"].(string); ok && strings.Contains(txt, ompFloorReminderSentinel) {
						return
					}
				}
			}
			msg["content"] = append(content, map[string]any{"type": "text", "text": ompFloorCapabilityReminder})
		default:
			continue
		}
		rawMsgs[i] = msg
		payload["messages"] = rawMsgs
		return
	}
}

// ensureCliTools enforces the free-tier traffic-gate tool floor on the wire
// (topUpCliTools in clitools.go): a request that carried no tools gets the
// full canonical 16 declarations, and a request that DID declare tools keeps
// them verbatim while missing officials are topped up. tool_choice is never
// fabricated: a client that sent none keeps none (the OpenAI default is
// auto), and inventing one rewrites an envelope the client deliberately
// shaped (TestConformanceGooseNoToolChoiceUsageTail).
func ensureCliTools(payload map[string]any) {
	topUpCliTools(payload)
}

// injectEnvelope merges the CLI fingerprint into the request body without
// disturbing client-supplied fields: codebuff_metadata, provider
// data_collection=deny, and forced streaming. The envelope carries no stop
// sequence: upstream deleted globalStopSequence and the stopSequences
// argument that passed it (agent-runtime constants.ts, prompt-agent-stream.ts
// at vendor 0.0.183), so the real CLI now sends none and a request only ever
// carries a stop list the client supplied itself.
// agentModeCostMode maps a CLI agent mode to its wire cost_mode, mirroring
// AGENT_MODE_TO_COST_MODE (upstream/freebuff cli/src/utils/constants.ts:
// 182-187, IS_FREEBUFF=true): DEFAULT/PLAN=normal, LITE=free, MAX=max.
// Empty or unknown modes return "" so the caller falls back to the
// client's configured cost mode. BYOK callers send normal — they never
// reach this path with a Freebuff agent mode.
func agentModeCostMode(mode string) string {
	switch strings.ToUpper(strings.TrimSpace(mode)) {
	case "LITE":
		return "free"
	case "MAX":
		return "max"
	case "DEFAULT", "PLAN":
		return "normal"
	default:
		return ""
	}
}

// chatHandleFor resolves the model a catalog-mode turn runs as: the held
// catalog's handle for the requested model (vendor freebuff-catalog-agent.ts
// builds the root with model: row.handle — the server resolves it and
// admits no plain id on that root). Empty when the catalog names no row for
// the id: the caller sends the raw id (vendor freebuffCatalogHandleFor ??
// model), and the server answers stale if it must. The requested model is
// the caller's explicit option, else the body's own model field.
func chatHandleFor(held *modelCatalog, opts ChatOptions, body []byte) string {
	if held == nil {
		return ""
	}
	model := opts.Model
	if model == "" {
		model = chatBodyModel(body)
	}
	return catalogHandleFor(held, model)
}

// chatBodyModel extracts the body's top-level model field, "" when absent
// or not a string.
func chatBodyModel(body []byte) string {
	var payload struct {
		Model any `json:"model"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	model, _ := payload.Model.(string)
	return model
}

// rewriteChatModel swaps the enveloped body's top-level model field for the
// catalog handle. It fails open to the input bytes (a body that cannot
// round-trip is sent as-is — the device signature below always covers the
// exact bytes that go out, so the two can never disagree).
func rewriteChatModel(body []byte, handle string) []byte {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil || handle == "" {
		return body
	}
	payload["model"] = handle
	out, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return out
}

func injectEnvelope(body []byte, costMode string, opts ChatOptions) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse request body: %w", err)
	}

	ensureCliSystemMarker(payload, opts.AgentID, opts.Model)
	if opts.OMPFloorOnly {
		appendOMPFloorReminder(payload)
	}
	ensureCliTools(payload)

	// client_id is minted ONCE PER RUN and repeated here — never a fresh
	// draw per chat call. The CLI mints it once per prompt (run.ts:722
	// `const promptId = Math.random().toString(36).substring(2,15)`, handed
	// to the agent loop as `clientSessionId` at run.ts:822 and stamped on
	// every LLM step by llm.ts:117), so a per-call draw made ONE run_id fan
	// out across N client ids — which is what upstream refuses as
	// free_mode_run_fanout. The shape stays SDK-faithful 13-char base36:
	// never the sess:/run:-prefixed forms the server fingerprints as a
	// proxy, and never pingmike's ^wf-[a-z0-9]{8}$ (#103;
	// cf-worker-signals.ts looksLikeProxyClientId). trace_session_id is
	// likewise per run and freebuff_instance_id stays per session.
	clientID := opts.ClientID
	if clientID == "" {
		clientID = generateClientID()
	}
	// Preserve any extra caller-supplied codebuff_metadata keys (e.g.
	// cache_debug_correlation, n) the CLI's getProviderOptions merges via
	// extraCodebuffMetadata before stamping reserved identifiers (llm.ts:112-122).
	// Reserved keys are always overwritten below so the server trusts only
	// proxy-minted identifiers; non-reserved extras are forwarded verbatim.
	extraMeta := map[string]any{}
	if raw, ok := payload["codebuff_metadata"].(map[string]any); ok {
		for k, v := range raw {
			switch k {
			case "run_id", "client_id", "trace_session_id", "freebuff_instance_id", "freebuff_multi_session", "surface", "llm_step_number", "cost_mode", "freebuff_reasoning_effort", "repo_snapshot":
				// reserved — overwritten below (server-trusted identifiers)
			default:
				extraMeta[k] = v
			}
		}
	}
	for k, v := range opts.ExtraCodebuffMetadata {
		switch k {
		case "run_id", "client_id", "trace_session_id", "freebuff_instance_id", "freebuff_multi_session", "surface", "llm_step_number", "cost_mode", "freebuff_reasoning_effort", "repo_snapshot":
			// reserved — must not be smuggled via extra
			continue
		default:
			extraMeta[k] = v
		}
	}
	metadata := map[string]any{}
	for k, v := range extraMeta {
		metadata[k] = v
	}
	metadata["run_id"] = opts.RunID
	metadata["client_id"] = clientID
	if opts.TraceSessionID != "" {
		metadata["trace_session_id"] = opts.TraceSessionID
	}
	if opts.SessionInstanceID != "" {
		metadata["freebuff_instance_id"] = opts.SessionInstanceID
		if _, attemptMode := sessionAttemptSuffix(opts.SessionInstanceID); attemptMode {
			metadata["freebuff_multi_session"] = "1"
			metadata["surface"] = "cli"
		}
	}
	// llm_step_number is the 1-based per-run agent step, String(n) on the
	// wire (#113; upstream/freebuff run-agent-step.ts:1175-1177).
	if opts.StepNumber > 0 {
		metadata["llm_step_number"] = strconv.Itoa(opts.StepNumber)
	}
	if opts.N > 0 {
		metadata["n"] = opts.N
	}
	if opts.CacheDebugCorrelation != "" {
		metadata["cache_debug_correlation"] = opts.CacheDebugCorrelation
	}
	// freebuff_reasoning_effort mirrors the normalized top-level
	// reasoning_effort the convert layer already clamped to the model's
	// ladder. This is the field the upstream server's effort authority
	// actually reads (upstream/freebuff freebuff-models.ts
	// resolveFreebuffReasoningEffort, carried per request by the CLI as
	// codebuff_metadata.freebuff_reasoning_effort — use-send-message.ts
	// :602-608); a top-level reasoning_effort alone may never reach it.
	// Absent when the client requested none, so upstream applies its own
	// catalog default — matching the CLI's silent-turn contract.
	if re, ok := payload["reasoning_effort"].(string); ok && re != "" {
		metadata["freebuff_reasoning_effort"] = re
	}
	// cost_mode follows the CLI agent mode when the caller names one
	// (use-send-message.ts:749 `costMode: AGENT_MODE_TO_COST_MODE[agentMode]`),
	// else the client's configured cost mode (llm.ts:121 stamps costMode
	// only when set — absent means upstream applies its own default).
	effectiveCostMode := costMode
	if m := agentModeCostMode(opts.AgentMode); m != "" {
		effectiveCostMode = m
	}
	if effectiveCostMode != "" {
		metadata["cost_mode"] = effectiveCostMode
		if effectiveCostMode == "free" {
			metadata["surface"] = "cli"
		}
	}
	// repo_snapshot rides verbatim when the caller supplies the
	// JSON-encoded aggregate snapshot (the CLI's JSON.stringify form);
	// absent otherwise — never fabricated, never forwarded from raw
	// client bodies (reserved above).
	if opts.RepoSnapshot != "" {
		metadata["repo_snapshot"] = opts.RepoSnapshot
	}
	payload["codebuff_metadata"] = metadata
	// Provider routing passes the client's OpenRouter keys through: the CLI
	// builds providerConfig from the agent's provider options when set, else
	// {order, allow_fallbacks} keyed off the model (llm.ts getProviderOptions).
	// Only well-typed order ([]any) / allow_fallbacks (bool) are forwarded;
	// data_collection stays deny (the CLI envelope default) — a client asking
	// for allow is not honored silently.
	provider := map[string]any{"data_collection": "deny"}
	if rawProvider, ok := payload["provider"].(map[string]any); ok {
		if order, ok := rawProvider["order"].([]any); ok {
			provider["order"] = order
		}
		if fallbacks, ok := rawProvider["allow_fallbacks"].(bool); ok {
			provider["allow_fallbacks"] = fallbacks
		}
	}
	if strings.HasPrefix(opts.Model, "anthropic/") {
		provider["only"] = []any{"amazon-bedrock"}
	}
	payload["provider"] = provider
	payload["stream"] = true
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("re-marshal envelope: %w", err)
	}
	return out, nil
}
