package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// closeLogFile closes the log file held by a New/NewLogger result so
// TempDir cleanup can delete it (Windows refuses to delete open files).
func closeLogFile(t *testing.T, logger *slog.Logger) {
	t.Helper()
	switch th := logger.Handler().(type) {
	case *textHandler:
		th.mu.Lock()
		defer th.mu.Unlock()
		if th.file != nil {
			_ = th.file.Close()
			th.file = nil
		}
	case *jsonHandler:
		th.mu.Lock()
		defer th.mu.Unlock()
		if th.file != nil {
			_ = th.file.Close()
			th.file = nil
		}
	default:
		t.Fatalf("logger handler is %T, want *textHandler or *jsonHandler", logger.Handler())
	}
}

// captureStderr reroutes os.Stderr for the duration of fn and returns
// everything written to it. Not safe under t.Parallel.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()
	fn()
	_ = w.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return string(data)
}

func TestNewLoggerLevelSelection(t *testing.T) {
	infoFile := filepath.Join(t.TempDir(), "info.log")
	infoLogger := NewLogger(false, infoFile)
	infoLogger.Debug("debug line")
	infoLogger.Info("info line")
	closeLogFile(t, infoLogger)

	data, err := os.ReadFile(infoFile)
	if err != nil {
		t.Fatalf("read %s: %v", infoFile, err)
	}
	got := string(data)
	if !strings.Contains(got, `msg="info line"`) {
		t.Errorf("Info line missing from log file: %q", got)
	}
	if strings.Contains(got, "debug line") {
		t.Errorf("Debug line logged at Info level: %q", got)
	}

	debugFile := filepath.Join(t.TempDir(), "debug.log")
	debugLogger := NewLogger(true, debugFile)
	debugLogger.Debug("debug line")
	closeLogFile(t, debugLogger)

	data, err = os.ReadFile(debugFile)
	if err != nil {
		t.Fatalf("read %s: %v", debugFile, err)
	}
	if !strings.Contains(string(data), `msg="debug line"`) {
		t.Errorf("Debug line missing at Debug level: %q", data)
	}
}

func TestNewLoggerAppendsToFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	first := NewLogger(true, path)
	first.Info("first")
	second := NewLogger(true, path)
	second.Info("second")
	closeLogFile(t, first)
	closeLogFile(t, second)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	got := string(data)
	if !strings.Contains(got, "msg=first") || !strings.Contains(got, "msg=second") {
		t.Errorf("expected both lines appended, got: %q", got)
	}
	if strings.Contains(got, "\x1b[") {
		t.Errorf("log file contains ANSI color escapes: %q", got)
	}
}

func TestNewLoggerColors(t *testing.T) {
	// The colorized level tokens are rendered by the text handler when its
	// colorize flag is set. New() only sets it on an interactive console
	// and the test runner's stderr is a pipe, so pin the token rendering
	// with an explicitly colorized handler — and pin the piped default
	// (plain text, no ANSI) via NewLogger.
	out := captureStderr(t, func() {
		logger := slog.New(&textHandler{w: os.Stderr, level: slog.LevelDebug, colorize: true})
		logger.Debug("m-debug")
		logger.Info("m-info")
		logger.Warn("m-warn")
		logger.Error("m-error")
	})
	for _, want := range []struct{ msg, token string }{
		{"m-debug", "\x1b[90mDEBUG\x1b[0m"},
		{"m-info", "\x1b[32mINFO\x1b[0m"},
		{"m-warn", "\x1b[33mWARN\x1b[0m"},
		{"m-error", "\x1b[31mERROR\x1b[0m"},
	} {
		if !strings.Contains(out, want.msg) {
			t.Errorf("stderr missing message %q", want.msg)
		}
		if !strings.Contains(out, want.token) {
			t.Errorf("stderr missing color token %q (for %s): %q", want.token, want.msg, out)
		}
	}
	plain := captureStderr(t, func() {
		logger := NewLogger(true, "")
		logger.Info("m-info-plain")
	})
	if strings.Contains(plain, "\x1b[") {
		t.Errorf("piped stderr must not contain ANSI escapes: %q", plain)
	}
	if !strings.Contains(plain, "level=INFO") || !strings.Contains(plain, "m-info-plain") {
		t.Errorf("piped stderr missing plain line: %q", plain)
	}
}

func TestRedactHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer topsecret")
	h.Add("x-api-key", "key-1")
	h.Add("x-api-key", "key-2")
	h.Set("X-Codebuff-Api-Key", "cb_secret")
	h.Set("Cookie", "session=abc")
	h.Set("Set-Cookie", "sid=xyz")
	h.Set("Content-Type", "application/json")
	h.Set("X-Custom", "keepme")

	got := RedactHeaders(h)

	if h.Get("Authorization") != "Bearer topsecret" {
		t.Error("RedactHeaders modified the input header")
	}
	for _, k := range []string{"Authorization", "X-Api-Key", "X-Codebuff-Api-Key", "Cookie", "Set-Cookie"} {
		if v := got[k][0]; v != "[redacted]" {
			t.Errorf("RedactHeaders[%q] = %q, want [redacted]", k, v)
		}
	}
	if v := got["Content-Type"][0]; v != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", v)
	}
	if v := got["X-Custom"][0]; v != "keepme" {
		t.Errorf("X-Custom = %q, want keepme", v)
	}
	if vs := got["X-Api-Key"]; len(vs) != 2 || vs[0] != "[redacted]" || vs[1] != "[redacted]" {
		t.Errorf("X-Api-Key values = %v, want two [redacted]", vs)
	}
}

func TestRedactHeadersNonCanonicalKey(t *testing.T) {
	h := http.Header{"x-api-key": {"k"}}
	got := RedactHeaders(h)
	if v := got["x-api-key"][0]; v != "[redacted]" {
		t.Errorf("lowercase raw key value = %q, want [redacted]", v)
	}
}

// TestRedactHeadersFreebuffPrefix verifies every x-freebuff-* header is
// redacted (session tokens, instance ids and account metadata are sensitive
// request context) while unrelated headers pass through untouched.
func TestRedactHeadersFreebuffPrefix(t *testing.T) {
	h := http.Header{}
	h.Set("X-Freebuff-Session-Id", "sess-abc")
	h.Set("x-freebuff-model", "deepseek-v4-pro")
	h.Set("X-Freebuff-Instance-Id", "inst-1")
	h.Set("X-Freebuff-Heartbeat", "1")
	h.Set("X-Request-Id", "req-123")
	h.Set("Content-Type", "application/json")

	got := RedactHeaders(h)
	for _, k := range []string{"X-Freebuff-Session-Id", "X-Freebuff-Model", "X-Freebuff-Instance-Id", "X-Freebuff-Heartbeat"} {
		if v := got[k][0]; v != "[redacted]" {
			t.Errorf("RedactHeaders[%q] = %q, want [redacted]", k, v)
		}
	}
	if v := got["X-Request-Id"][0]; v != "req-123" {
		t.Errorf("X-Request-Id = %q, want req-123 (not sensitive)", v)
	}
	if v := got["Content-Type"][0]; v != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", v)
	}
}

// TestRedactSecrets pins the token scrubber: cb_-prefixed tokens and
// Bearer <token> sequences (base64url alphabet) are replaced everywhere,
// and strings without secrets pass through unchanged.
func TestRedactSecrets(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"cb token alone", "cb_AbC123", "[redacted]"},
		{"cb token in body", `{"error":"free_mode_limited","token":"cb_xyz789"}`, `{"error":"free_mode_limited","token":"[redacted]"}`},
		{"cb token punctuation", "cb_abc-DEF_ghi.jkl~tail", "[redacted]"},
		{"bearer header", "Authorization: Bearer abcDEF012._~+/=-9", "Authorization: [redacted]"},
		{"bearer in json", `{"auth":"Bearer xYz","ok":1}`, `{"auth":"[redacted]","ok":1}`},
		{"both forms", "token=cb_a1B2 auth=Bearer q.r-s", "token=[redacted] auth=[redacted]"},
		{"no secret", "plain text with no tokens", "plain text with no tokens"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactSecrets(tc.in); got != tc.want {
				t.Errorf("RedactSecrets(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	// The input string is not mutated.
	in := "cb_keep"
	if got := RedactSecrets(in); got != "[redacted]" {
		t.Errorf("RedactSecrets returned %q", got)
	}
	if in != "cb_keep" {
		t.Errorf("RedactSecrets mutated its input: %q", in)
	}
}

func TestParseLevel(t *testing.T) {
	if _, ok := ParseLevel(""); ok {
		t.Error(`ParseLevel("") ok=true, want false`)
	}
	if lv, ok := ParseLevel("debug"); !ok || lv != slog.LevelDebug {
		t.Errorf("ParseLevel(debug) = %v, ok=%v; want %v, true", lv, ok, slog.LevelDebug)
	}
	if lv, ok := ParseLevel("INFO"); !ok || lv != slog.LevelInfo {
		t.Errorf("ParseLevel(INFO) = %v, ok=%v; want %v, true", lv, ok, slog.LevelInfo)
	}
	if _, ok := ParseLevel("bogus"); ok {
		t.Error("ParseLevel(bogus) ok=true, want false")
	}
	// trace is a first-class level, one step below debug, case-insensitive.
	for _, s := range []string{"trace", "TRACE", "Trace"} {
		if lv, ok := ParseLevel(s); !ok || lv != LevelTrace {
			t.Errorf("ParseLevel(%q) = %v, ok=%v; want LevelTrace, true", s, lv, ok)
		}
	}
	if LevelTrace >= slog.LevelDebug {
		t.Errorf("LevelTrace = %v, want strictly below LevelDebug (%v)", LevelTrace, slog.LevelDebug)
	}
	if got := LevelTrace.String(); got != "DEBUG-4" {
		t.Errorf("LevelTrace.String() = %q, want slog's DEBUG-4 (banner must special-case TRACE)", got)
	}
}

func TestSanitizeName(t *testing.T) {
	input := `a\b:c*d?e"f<g>h|i`
	got := sanitizeName(input)
	for _, r := range got {
		if strings.ContainsRune(`/\:*?"<>|.`, r) {
			t.Errorf("sanitizeName(%q) = %q, contains invalid file-name char %q", input, got, r)
		}
	}
	if len(got) > 60 {
		t.Errorf("sanitizeName(%q) length = %d, want <= 60", input, len(got))
	}
}

func TestTruncateUTF8Safe(t *testing.T) {
	// 50 multi-byte runes (100 bytes): byte slicing would split a rune.
	s := strings.Repeat("é", 50)
	got := truncate(s, 10)
	if n := len([]rune(got)); n != 13 {
		t.Errorf("truncate(50×é, 10) = %d runes, want 13 (10 + ellipsis)", n)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncate(50×é, 10) = %q, want ellipsis suffix", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("truncate(50×é, 10) = %q, want valid UTF-8", got)
	}

	if short := truncate("abc", 5); short != "abc" {
		t.Errorf("truncate(abc, 5) = %q, want abc (unchanged)", short)
	}
}

// TestNewLoggerCreatesNestedLogDir verifies that LOG_FILE may point into a
// fresh nested directory: the parent is created before the file is opened.
func TestNewLoggerCreatesNestedLogDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "logs", "proxy.log")
	logger := NewLogger(true, path)
	logger.Info("nested-dir line")
	closeLogFile(t, logger)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (parent dir was not created)", path, err)
	}
	if !strings.Contains(string(data), `msg="nested-dir line"`) {
		t.Errorf("line missing from log file in nested dir: %q", data)
	}
}

func TestColorizeWhenLogFileFailsToOpen(t *testing.T) {
	// A log file whose parent cannot be created (a regular file occupies the
	// directory position, so MkdirAll fails) cannot be opened; the logger
	// falls back to stderr-only. Colors are kept only on an interactive
	// console — the test runner's stderr is a pipe, so the fallback must
	// be plain text (no ANSI) while still carrying the line.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out := captureStderr(t, func() {
		logger := NewLogger(true, filepath.Join(blocker, "x.log"))
		logger.Info("still-logged")
	})
	if strings.Contains(out, "\x1b[") {
		t.Errorf("piped stderr fallback must not contain ANSI escapes: %q", out)
	}
	if !strings.Contains(out, "level=INFO") || !strings.Contains(out, "still-logged") {
		t.Errorf("stderr lost the line after log file open failure: %q", out)
	}
}

// TestAttrValueEscaping verifies that attr values containing newlines, tabs,
// quotes or other control characters are quoted, so one log record always
// writes exactly one line. Regression: values were concatenated via
// a.Value.String() unescaped, so a client-controlled model name or URL path
// containing "\nlevel=ERROR injected" forged a second log line.
func TestAttrValueEscaping(t *testing.T) {
	var buf bytes.Buffer
	h := &textHandler{w: &buf, level: slog.LevelInfo}
	logger := slog.New(h)
	logger.Info("request handled",
		"model", "codebuff-1\nlevel=ERROR injected",
		"path", "/v1/chat/completions",
		"user_agent", "tab\t\"quoted\"\x1b[31mred",
	)

	out := buf.String()
	// The injected token must survive only in its strconv.Quote-escaped form
	// inside the quoted value, never as a raw newline splitting the record.
	if !strings.Contains(out, `model="codebuff-1\nlevel=ERROR injected"`) {
		t.Errorf("model attr not escaped via strconv.Quote, got: %q", out)
	}
	if n := strings.Count(out, "\n"); n != 1 {
		t.Errorf("record split across %d lines, want exactly 1: %q", n, out)
	}
	if !strings.Contains(out, `user_agent="tab\t\"quoted\"\x1b[31mred"`) {
		t.Errorf("tab/quote/control chars not escaped, got: %q", out)
	}
	if !strings.Contains(out, "path=/v1/chat/completions") {
		t.Errorf("clean attr value must stay unquoted, got: %q", out)
	}
}

// TestTextHandlerWithAttrsWithGroup pins the text handler's copy-on-write
// WithAttrs/WithGroup contract: bound attrs are appended to every record,
// group keys get the dotted group.key prefix, and the handler a With*
// call was made on is never mutated (immutable base attrs).
func TestTextHandlerWithAttrsWithGroup(t *testing.T) {
	var buf bytes.Buffer
	h := &textHandler{w: &buf, level: slog.LevelInfo}

	base := slog.New(h)
	if got := h.WithAttrs([]slog.Attr{slog.String("k", "v")}); got == slog.Handler(h) {
		t.Error("WithAttrs returned the same handler, want a copy")
	}
	if got := h.WithGroup("grp"); got == slog.Handler(h) {
		t.Error("WithGroup returned the same handler, want a copy")
	}

	// Without bound state the base handler output is untouched.
	base.Info("plain")
	if !strings.Contains(buf.String(), "msg=plain\n") {
		t.Errorf("base handler output changed by With* calls: %q", buf.String())
	}

	// Bound attrs first (at their bind-time depth, no group prefix yet),
	// record attrs under the group — slog's text-handler order.
	buf.Reset()
	logger := slog.New(h).With("bound", "attr").WithGroup("grp")
	logger.Info("msg", "k", "v")
	out := buf.String()
	for _, want := range []string{"msg=msg", "bound=attr", "grp.k=v"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q missing %q", out, want)
		}
	}
	if strings.Contains(out, "grp.bound=") {
		t.Errorf("bound attr wrongly prefixed by a later group: %q", out)
	}
	if n := strings.Count(out, "\n"); n != 1 {
		t.Errorf("record split across %d lines, want 1: %q", n, out)
	}

	// A second WithGroup nests: grp.a.k=v. Record group attrs prefix too.
	buf.Reset()
	logger = slog.New(h).WithGroup("grp").WithGroup("a").With("b", "1")
	logger.Info("nested", slog.Group("g", slog.Int("n", 2)))
	out = buf.String()
	for _, want := range []string{"grp.a.b=1", "grp.a.g.n=2"} {
		if !strings.Contains(out, want) {
			t.Errorf("nested output %q missing %q", out, want)
		}
	}

	// The original handler still has no bound state after all the With*
	// calls above (copy-on-write left it immutable).
	buf.Reset()
	base.Info("still plain")
	if strings.Contains(buf.String(), "bound=attr") || strings.Contains(buf.String(), "grp.") {
		t.Errorf("base handler leaked bound state: %q", buf.String())
	}
}

// TestTextHandlerShape pins the byte-for-byte output shape at info/debug
// for attr-less records (the process-logger contract) and the trace token.
func TestTextHandlerShape(t *testing.T) {
	fixed := time.Date(2026, 8, 18, 12, 0, 0, 123000000, time.UTC)
	var buf bytes.Buffer
	h := &textHandler{w: &buf, level: LevelTrace}

	rec := slog.NewRecord(fixed, slog.LevelInfo, "hello", 0)
	rec.AddAttrs(slog.String("k", "v"))
	if err := h.Handle(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	want := "time=2026-08-18T12:00:00.123Z level=INFO msg=hello k=v\n"
	if got := buf.String(); got != want {
		t.Errorf("text record = %q, want %q", got, want)
	}

	buf.Reset()
	rec = slog.NewRecord(fixed, LevelTrace, "trace line", 0)
	if err := h.Handle(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "level=TRACE ") {
		t.Errorf("trace record level token not TRACE: %q", buf.String())
	}
}

// TestJSONHandlerShape writes records through New with format "json" and
// verifies each line parses as JSON with the RFC3339-ms time, the level and
// msg fields, and real group nesting.
func TestJSONHandlerShape(t *testing.T) {
	out := captureStderr(t, func() {
		logger := New(LevelTrace, "", "json").With("node", "n1").WithGroup("svc")
		logger.Info("hello", "k", "v", slog.Int("status", 200), slog.Group("http", slog.Int("latency_ms", 12)))
		logger.Warn("warn line")
	})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 JSON lines, got %d: %q", len(lines), out)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("line 1 is not valid JSON: %v\n%q", err, lines[0])
	}
	// RFC3339 with milliseconds (the record time is local, so no zone
	// assertion).
	tm, err := time.Parse("2006-01-02T15:04:05.000Z07:00", rec["time"].(string))
	if err != nil {
		t.Errorf("time field %q not RFC3339-ms: %v", rec["time"], err)
	} else if tm.IsZero() {
		t.Errorf("time field parsed to zero time: %v", rec["time"])
	}
	if rec["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", rec["level"])
	}
	if rec["msg"] != "hello" {
		t.Errorf("msg = %v, want hello", rec["msg"])
	}
	// The bound "node" attr predates WithGroup("svc"), so it sits at the top
	// level; record attrs nest inside svc — slog's semantics.
	if rec["node"] != "n1" {
		t.Errorf("node = %v, want n1 (bound before the group)", rec["node"])
	}
	svc, ok := rec["svc"].(map[string]any)
	if !ok {
		t.Fatalf("svc field not nested object: %v", rec["svc"])
	}
	if svc["k"] != "v" {
		t.Errorf("svc.k = %v, want v", svc["k"])
	}
	if svc["status"] != float64(200) {
		t.Errorf("svc.status = %v, want 200", svc["status"])
	}
	httpGroup, ok := svc["http"].(map[string]any)
	if !ok {
		t.Fatalf("svc.http not nested object: %v", svc["http"])
	}
	if httpGroup["latency_ms"] != float64(12) {
		t.Errorf("svc.http.latency_ms = %v, want 12", httpGroup["latency_ms"])
	}
	// Color-free: JSON must not contain ANSI escapes.
	if strings.Contains(out, "\x1b[") {
		t.Errorf("json output contains ANSI escapes: %q", out)
	}
	// The warn line parses too and levels map to their slog names.
	var w map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &w); err != nil {
		t.Fatalf("line 2 is not valid JSON: %v\n%q", err, lines[1])
	}
	if w["level"] != "WARN" || w["msg"] != "warn line" {
		t.Errorf("warn record = %v, want level WARN msg warn line", w)
	}
}

// TestJSONHandlerEmptyGroups verifies that groups that end up empty are
// suppressed (slog's contract), so no "key":{} shells pollute the JSON.
func TestJSONHandlerEmptyGroups(t *testing.T) {
	var buf bytes.Buffer
	h := &jsonHandler{w: &buf, level: slog.LevelInfo}
	logger := slog.New(h).WithGroup("x").WithGroup("y")
	logger.Info("no groups")
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("output not valid JSON: %v\n%q", err, buf.String())
	}
	if _, ok := rec["x"]; ok {
		t.Errorf("empty group x leaked into output: %v", rec)
	}
	if rec["msg"] != "no groups" {
		t.Errorf("msg = %v, want no groups", rec["msg"])
	}

	// A group with content is kept; an inner empty group is dropped.
	buf.Reset()
	logger = slog.New(&jsonHandler{w: &buf, level: slog.LevelInfo}).
		WithGroup("x").With("a", 1).WithGroup("y")
	logger.Info("kept", slog.Group("g"))
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("output not valid JSON: %v\n%q", err, buf.String())
	}
	x, ok := rec["x"].(map[string]any)
	if !ok {
		t.Fatalf("x not nested object: %v", rec)
	}
	if x["a"] != float64(1) {
		t.Errorf("x.a = %v, want 1", x["a"])
	}
	if _, ok := x["y"]; ok {
		t.Errorf("empty group y leaked: %v", x)
	}
	if _, ok := x["g"]; ok {
		t.Errorf("empty group attr g leaked: %v", x)
	}
}

// TestQuoteMessageEdges pins the quoting decision table: multi-word
// messages and values with tabs/newlines/CRs/quotes/control characters are
// strconv-quoted; single tokens, empty strings and non-control Unicode are
// not.
func TestQuoteMessageEdges(t *testing.T) {
	cases := []struct {
		name string
		s    string
		want bool // needsQuote
	}{
		{"empty", "", false},
		{"single token", "hello", false},
		{"space", "two words", true},
		{"tab", "a\tb", true},
		{"newline", "a\nb", true},
		{"carriage return", "a\rb", true},
		{"double quote", `a"b`, true},
		{"control char", "a\x01b", true},
		{"non-control unicode single token", "héllo", false},
		{"non-control unicode with space", "héllo wörld", true},
		{"narrow no-break space", "a\u00a0b", false},
		{"emoji", "🚀", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsQuote(tc.s); got != tc.want {
				t.Errorf("needsQuote(%q) = %v, want %v", tc.s, got, tc.want)
			}
			// quoteMessage must round-trip: quoted iff needsQuote.
			if quoted := quoteMessage(tc.s) != tc.s; quoted != tc.want {
				t.Errorf("quoteMessage(%q) quoted=%v, want %v", tc.s, quoted, tc.want)
			}
		})
	}
}

// TestSanitizeNameBoundary pins the 60-rune truncation boundary and the
// character replacement: shorter names are unchanged, exactly-60 stays 60,
// longer names are cut to 60, invalid file-name characters are replaced with
// underscores, and multi-byte input is truncated by runes so no UTF-8
// sequence is ever split.
func TestSanitizeNameBoundary(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantLen int
	}{
		{"empty", "", 0},
		{"short", "chat-1", 6},
		{"exactly 60", strings.Repeat("a", 60), 60},
		{"61 truncated", strings.Repeat("a", 61), 60},
		{"long with specials", strings.Repeat("a/b:c", 20), 60},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeName(tc.in)
			if len(got) != tc.wantLen {
				t.Errorf("sanitizeName(%q) length = %d, want %d", tc.in, len(got), tc.wantLen)
			}
		})
	}
	if got := sanitizeName(`a\b:c*d?e"f<g>h|i.`); got != "a_b_c_d_e_f_g_h_i_" {
		t.Errorf("sanitizeName(specials) = %q", got)
	}

	// 61 two-byte runes (122 bytes): a byte slice cut at 60 would split a
	// rune and emit invalid UTF-8; rune-based truncation yields 60 whole
	// runes that are valid UTF-8.
	multi := strings.Repeat("é", 61)
	got := sanitizeName(multi)
	if n := len([]rune(got)); n != 60 {
		t.Errorf("sanitizeName(61×é) = %d runes, want 60", n)
	}
	if !utf8.ValidString(got) {
		t.Errorf("sanitizeName(61×é) produced invalid UTF-8: %q", got)
	}
}

// TestLevelColorBelowDebug pins the ANSI level palette: anything below
// DEBUG renders gray, DEBUG gray, INFO green, WARN yellow, ERROR and above
// red.
func TestLevelColorBelowDebug(t *testing.T) {
	cases := []struct {
		level slog.Level
		want  string
	}{
		{slog.LevelDebug - 8, ansiGray},
		{slog.LevelDebug, ansiGray},
		{slog.LevelInfo, ansiGreen},
		{slog.LevelWarn, ansiYellow},
		{slog.LevelError, ansiRed},
		{slog.LevelError + 8, ansiRed},
	}
	for _, tc := range cases {
		if got := levelColor(tc.level); got != tc.want {
			t.Errorf("levelColor(%v) = %q, want %q", tc.level, got, tc.want)
		}
	}
}

// TestRedactHeadersNilEmpty pins the nil/empty inputs: RedactHeaders of a
// nil or empty header returns an empty (non-nil) map.
func TestRedactHeadersNilEmpty(t *testing.T) {
	if got := RedactHeaders(nil); len(got) != 0 {
		t.Errorf("RedactHeaders(nil) = %v, want empty", got)
	}
	if got := RedactHeaders(http.Header{}); len(got) != 0 {
		t.Errorf("RedactHeaders(empty) = %v, want empty", got)
	}
}
