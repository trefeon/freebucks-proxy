package server

// Relay hot-path benchmarks. They measure the CPU/alloc cost the proxy adds
// between upstream and client per streamed chunk, and pin the single-decode
// win of convert.SanitizeChunkMapped over the previous
// SanitizeChunkOpts + json.Unmarshal pair. No network, no timing flakiness:
// the upstream side is an in-memory SSE string and the client side a
// discarding flusher.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/backend/internal/convert"
)

// discardWriter is an http.ResponseWriter that drops the body and implements
// Flush, so a relay benchmark measures CPU, not buffer growth.
type discardWriter struct{ h http.Header }

func (d *discardWriter) Header() http.Header {
	if d.h == nil {
		d.h = make(http.Header)
	}
	return d.h
}
func (d *discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (d *discardWriter) WriteHeader(int)             {}
func (d *discardWriter) Flush()                      {}

// benchChatChunk builds one plain-text upstream chat.completion.chunk frame
// (a fully-populated chunk that takes the sanitize fast path).
func benchChatChunk(i int) string {
	return fmt.Sprintf("data: {\"id\":\"chatcmpl-b\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"tok%d\"},\"finish_reason\":null}]}\n\n", i)
}

// benchUpstreamStream renders n text chunks as a single SSE body.
func benchUpstreamStream(n int) string {
	var sb strings.Builder
	for i := range n {
		sb.WriteString(benchChatChunk(i))
	}
	return sb.String()
}

// BenchmarkRelayStream measures the OpenAI relay end to end over a stream of
// plain-text chunks: parse + rewrite + encode + flush per chunk.
func BenchmarkRelayStream(b *testing.B) {
	const chunks = 200
	ss := benchUpstreamStream(chunks)
	s := testRelayServer()
	b.ReportAllocs()
	b.SetBytes(int64(len(ss)))
	b.ResetTimer()
	for b.Loop() {
		s.relayStream(context.Background(), &discardWriter{}, strings.NewReader(ss), &relayStats{}, time.Now())
	}
	b.ReportMetric(float64(chunks)*float64(b.N)/b.Elapsed().Seconds(), "chunks/s")
}

// BenchmarkSanitizeChunkDecode isolates the double decode the relay used to
// pay: SanitizeChunkMapped (one decode, map handed to the rewriter) versus
// SanitizeChunkOpts followed by a json.Unmarshal of its bytes (the previous
// relay shape).
func BenchmarkSanitizeChunkDecode(b *testing.B) {
	line := []byte(benchChatChunk(7))
	opts := convert.DefaultOptions()

	b.Run("single-decode", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			clean, chunk, drop := convert.SanitizeChunkMapped(line, opts)
			if drop || chunk == nil || clean == nil {
				b.Fatal("chunk dropped")
			}
		}
	})

	b.Run("double-decode", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			clean, drop := convert.SanitizeChunkOpts(line, opts)
			if drop || clean == nil {
				b.Fatal("chunk dropped")
			}
			var chunk map[string]any
			if err := json.Unmarshal(clean, &chunk); err != nil {
				b.Fatal(err)
			}
		}
	})
}
