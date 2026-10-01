// Package tokenestimate implements a local, deterministic token estimator
// aligned with the FreeBuff upstream context estimator (see
// CodebuffAI/freebuff packages/agent-runtime/src/util/token-counter.ts):
// the chars/3 character heuristic (deliberately NOT a BPE tokenizer), plus
// per-message overhead, a flat per-image cost, and structured counting of
// tool calls/results so base64 media is never tokenized as plain text.
//
// The upstream rule (token-counter.ts estimatedTextLength/countTokens) is:
//
//	estimatedTextLength(text) = text.length + (utf8Bytes(text) - text.length) * 3
//	countTokens(text)         = ceil(estimatedTextLength(text) / 3)
//
// where text.length is the JS UTF-16 code-unit count, so every multibyte
// character is charged one token per extra UTF-8 byte and non-BMP characters
// count as two UTF-16 units. There are no special-token semantics: OpenAI-style
// markers (<|endoftext|> etc.) are ordinary ASCII text.
//
// The result is an estimate for Anthropic-compatible clients, not a
// provider-exact token count.
package tokenestimate

import (
	"encoding/json"
	"errors"
	"strings"
)

// Constants mirror the FreeBuff upstream estimator as of 2026-08-17
// (packages/agent-runtime/src/util/token-counter.ts). They are heuristics,
// not Anthropic-published formula values.
const (
	// charsPerToken is CHARS_PER_TOKEN: the divisor of the UTF-8-expanded
	// character estimate (token-counter.ts / CHARS_PER_TOKEN).
	charsPerToken = 3
	// perMessageOverhead is PER_MESSAGE_TOKEN_OVERHEAD: role marker and
	// delimiters Anthropic adds on top of the raw content per message.
	perMessageOverhead = 8
	// imageTokenEstimate is IMAGE_TOKEN_ESTIMATE: the ceiling Anthropic bills
	// for a large image, used flat instead of counting base64 characters.
	imageTokenEstimate = 1600
)

// ErrDocument is returned when a document block is present: the proxy's
// /v1/messages conversion does not consume documents, so this estimator
// refuses to guess a PDF token count instead of faking accuracy. The server
// maps it to a distinct 400 code (unsupported_content).
var ErrDocument = errors.New("document blocks are not supported by this proxy")

// Estimator is a stateless chars/3 text estimator. It holds no mutable state
// and is safe for concurrent use.
type Estimator struct{}

// New returns a ready Estimator. It never fails; the error return mirrors the
// original codec-backed constructor so the server wiring is unchanged.
func New() (*Estimator, error) {
	return &Estimator{}, nil
}

// CountText estimates tokens for one text string with the upstream formula:
// the UTF-16 code-unit length plus three times every extra UTF-8 byte over it,
// divided by three and rounded up (token-counter.ts estimatedTextLength /
// countTokens).
func (e *Estimator) CountText(text string) int {
	units := utf16Len(text)
	estimated := units + (len(text)-units)*charsPerToken
	return (estimated + charsPerToken - 1) / charsPerToken
}

// CountJSON estimates tokens for a value's deterministic JSON encoding. Go's
// encoding/json sorts map keys, so repeated counts of the same semantic
// object are stable.
func (e *Estimator) CountJSON(v any) int {
	if v == nil {
		return 0
	}
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return e.CountText(string(b))
}

// utf16Len returns the UTF-16 code-unit length of s, matching JavaScript's
// String.length without allocating. Invalid UTF-8 bytes decode to one
// replacement rune each, so they count as one unit apiece.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n++ // surrogate pair in UTF-16
		}
		n++
	}
	return n
}

// CountAnthropicRequest estimates input tokens for an Anthropic
// messages/count_tokens request body: system text + tool definitions + each
// message (per-message overhead plus structured content), mirroring FreeBuff's
// estimateContextTokensLocally. It returns an error for request shapes the
// proxy cannot consume (missing messages, document blocks).
func (e *Estimator) CountAnthropicRequest(raw map[string]any) (int, error) {
	total := 0
	if system, ok := raw["system"]; ok && system != nil {
		n, err := e.countSystem(system)
		if err != nil {
			return 0, err
		}
		total += n
	}
	rawMessages, ok := raw["messages"].([]any)
	if !ok {
		return 0, errors.New(`missing or invalid "messages" (want an array)`)
	}
	for _, rawMsg := range rawMessages {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			// Non-object messages are dropped by /v1/messages conversion and
			// never reach the model, so they add nothing here either.
			continue
		}
		total += perMessageOverhead
		n, err := e.countContent(msg["content"])
		if err != nil {
			return 0, err
		}
		total += n
	}
	if tools, ok := raw["tools"].([]any); ok {
		total += e.CountJSON(e.toolsForCount(tools))
	}
	return total, nil
}

// countSystem counts the top-level system field: a plain string directly, an
// array via its text blocks (JSON fallback for anything else recognized by
// the conversion surface).
func (e *Estimator) countSystem(system any) (int, error) {
	switch typed := system.(type) {
	case string:
		return e.CountText(typed), nil
	case []any:
		total := 0
		for _, rawPart := range typed {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			switch strings.ToLower(asString(part["type"])) {
			case "text":
				total += e.CountText(asString(part["text"]))
			case "image":
				// Billed flat like every other image path; the base64 payload
				// is never tokenized (and never reaches the model: the
				// conversion drops non-text system parts).
				total += imageTokenEstimate
			case "document":
				return 0, ErrDocument
			default:
				total += e.CountJSON(part)
			}
		}
		return total, nil
	default:
		// Non-string/non-array system values are dropped by the conversion,
		// so nothing is counted.
		return 0, nil
	}
}

// countContent counts one message's content: a string directly, an array of
// content blocks structurally, or a conservative JSON fallback for any other
// accepted shape.
func (e *Estimator) countContent(content any) (int, error) {
	switch typed := content.(type) {
	case nil:
		return 0, nil
	case string:
		return e.CountText(typed), nil
	case []any:
		total := 0
		for _, rawPart := range typed {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			n, err := e.countContentPart(part)
			if err != nil {
				return 0, err
			}
			total += n
		}
		return total, nil
	default:
		return e.CountJSON(typed), nil
	}
}

// countContentPart counts one Anthropic content block. Types the /v1/messages
// conversion does not understand get a JSON fallback so accepted structures
// never silently under-count.
func (e *Estimator) countContentPart(part map[string]any) (int, error) {
	switch strings.ToLower(asString(part["type"])) {
	case "text":
		return e.CountText(asString(part["text"])), nil
	case "thinking":
		// The conversion accepts the thinking field, falling back to text.
		text := asString(part["thinking"])
		if text == "" {
			text = asString(part["text"])
		}
		return e.CountText(text), nil
	case "tool_use", "server_tool_use":
		return e.CountText(asString(part["name"])) + e.CountJSON(part["input"]), nil
	case "tool_result":
		return e.countToolResult(part)
	case "image":
		return imageTokenEstimate, nil
	case "document":
		return 0, ErrDocument
	default:
		return e.CountJSON(part), nil
	}
}

// countToolResult counts a tool_result part: textual or structured result
// content, with images billed flat instead of tokenizing base64.
func (e *Estimator) countToolResult(part map[string]any) (int, error) {
	return e.countToolResultContent(part["content"])
}

func (e *Estimator) countToolResultContent(content any) (int, error) {
	switch typed := content.(type) {
	case nil:
		return 0, nil
	case string:
		return e.CountText(typed), nil
	case []any:
		total := 0
		for _, rawItem := range typed {
			switch item := rawItem.(type) {
			case string:
				total += e.CountText(item)
			case map[string]any:
				switch strings.ToLower(asString(item["type"])) {
				case "text":
					total += e.CountText(asString(item["text"]))
				case "image":
					total += imageTokenEstimate
				case "document":
					return 0, ErrDocument
				default:
					total += e.CountJSON(item)
				}
			default:
				total += e.CountJSON(item)
			}
		}
		return total, nil
	default:
		return e.CountJSON(typed), nil
	}
}

// anthropicToolCount is the tool representation the model effectively
// receives, kept in the same field order FreeBuff counts ({name,
// description, input_schema}) so the JSON is byte-stable.
type anthropicToolCount struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema,omitempty"`
}

func (e *Estimator) toolsForCount(tools []any) []anthropicToolCount {
	out := make([]anthropicToolCount, 0, len(tools))
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		tc := anthropicToolCount{Name: asString(tool["name"])}
		tc.Description = asString(tool["description"])
		if schema, ok := tool["input_schema"]; ok && schema != nil {
			tc.InputSchema = schema
		}
		out = append(out, tc)
	}
	return out
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
