package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stream runs a canned SSE body through the client and returns what the
// consumer saw.
func streamBody(t *testing.T, body string) []Chunk {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, line := range strings.SplitAfter(body, "\n") {
			if line == "" {
				continue
			}
			_, _ = w.Write([]byte(line))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "m")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got []Chunk
	for ch := range c.Stream(ctx, Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}}) {
		got = append(got, ch)
	}
	return got
}

// The end-of-stream sentinel has to reach the consumer as a Done chunk.
//
// "[DONE]" is not valid JSON, so a reader that validates the payload before
// checking for the sentinel treats it as a frame that is still arriving: it
// buffers the text, never parses it, and the sentinel is dropped. The stream
// then only ends at EOF, and a response carrying nothing but the sentinel is
// reported as a failed one.
func TestDoneMarkerIsDelivered(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	got := streamBody(t, body)

	if !hasDone(got) {
		t.Fatalf("the sentinel was not delivered: %+v", got)
	}
	if text := chunkText(got); text != "hi" {
		t.Errorf("streamed text = %q, want %q", text, "hi")
	}
}

// A reply of nothing but the sentinel is a real — if empty — answer, not a
// broken response. Reporting it as a failure makes a provider that answers
// with no text look like an outage.
func TestSentinelOnlyStreamIsNotAnError(t *testing.T) {
	got := streamBody(t, "data: [DONE]\n\n")

	for _, ch := range got {
		if ch.Err != nil {
			t.Fatalf("a sentinel-only stream reported %v, want a clean empty reply", ch.Err)
		}
	}
	if !hasDone(got) {
		t.Fatalf("the sentinel was not delivered: %+v", got)
	}
}

// The sentinel still ends the stream when it arrives behind a frame whose JSON
// was split across data: lines, which the reader holds until it parses.
func TestDoneMarkerAfterASplitFrame(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":\n" +
		"data: {\"content\":\"split\"}}]}\n\n" +
		"data: [DONE]\n\n"
	got := streamBody(t, body)

	if !hasDone(got) {
		t.Fatalf("the sentinel was not delivered after a split frame: %+v", got)
	}
	if text := chunkText(got); text != "split" {
		t.Errorf("streamed text = %q, want %q", text, "split")
	}
}

// A stream that dies without ever sending a frame is a failed response, and
// must still say so — the sentinel path must not have swallowed this.
func TestStreamWithNoFramesReportsFailure(t *testing.T) {
	got := streamBody(t, ": keep-alive\n\n")

	var err error
	for _, ch := range got {
		if ch.Err != nil {
			err = ch.Err
		}
	}
	if err == nil {
		t.Fatalf("a stream with no frames reported success: %+v", got)
	}
}

func hasDone(chunks []Chunk) bool {
	for _, c := range chunks {
		if c.Done {
			return true
		}
	}
	return false
}

func chunkText(chunks []Chunk) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString(c.Delta.Content)
	}
	return b.String()
}
