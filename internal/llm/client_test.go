package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// A provider that rate-limits a request is retried after a twenty-second
// cool-down. The caller sees nothing at all during that sleep, so the client
// has to say it is waiting: without the notification a twenty-second pause and
// a hung program look identical from the outside.
func TestStreamReportsRateLimitWait(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "m")
	c.MaxRetries = 1

	var (
		wmu     sync.Mutex
		waits   []time.Duration
		limited []error
	)
	c.OnWait = func(d time.Duration, err error) {
		wmu.Lock()
		waits = append(waits, d)
		limited = append(limited, err)
		wmu.Unlock()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for range c.Stream(ctx, Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}}) {
	}

	wmu.Lock()
	defer wmu.Unlock()
	if len(waits) == 0 {
		t.Fatalf("no wait was reported, but the request was retried %d times", calls)
	}
	for i, d := range waits {
		if d <= 0 {
			t.Errorf("wait %d reported a non-positive duration %s", i, d)
		}
		if !IsRateLimit(limited[i]) {
			t.Errorf("wait %d reported a non-rate-limit error: %v", i, limited[i])
		}
	}
}

// A transient 5xx is retried on the ordinary backoff path, and that wait is
// reported the same way — naming it "rate limit" would be a lie, so
// IsRateLimit must be false for it.
func TestStreamReportsTransientWait(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "m")
	c.MaxRetries = 1

	var (
		wmu   sync.Mutex
		waits []time.Duration
		errs  []error
	)
	c.OnWait = func(d time.Duration, err error) {
		wmu.Lock()
		waits = append(waits, d)
		errs = append(errs, err)
		wmu.Unlock()
	}

	var got string
	for ch := range c.Stream(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}}) {
		if ch.Err != nil {
			t.Fatalf("stream error: %v", ch.Err)
		}
		got += ch.Delta.Content
	}

	if got != "ok" {
		t.Errorf("reply = %q, want %q", got, "ok")
	}
	wmu.Lock()
	defer wmu.Unlock()
	if len(waits) == 0 {
		t.Fatalf("the 502 was retried but no wait was reported")
	}
	if IsRateLimit(errs[0]) {
		t.Errorf("a 502 was classified as a rate limit: %v", errs[0])
	}
}

// The wait has to be reported before the sleep, not after it. If it came
// after, the user would see the explanation once the stall was already over.
func TestWaitIsReportedBeforeSleeping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", "m")
	c.MaxRetries = 1

	seen := make(chan time.Duration, 4)
	c.OnWait = func(d time.Duration, err error) { seen <- d }

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range c.Stream(ctx, Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}}) {
		}
	}()

	select {
	case d := <-seen:
		// The context is only cancelled at 500ms, and the first backoff is
		// ~250ms of jitter, so observing the notice well inside that window
		// means it preceded the sleep rather than followed it.
		if d <= 0 {
			t.Errorf("wait of %s reported", d)
		}
	case <-done:
		t.Fatalf("the turn ended without ever reporting the wait")
	case <-time.After(400 * time.Millisecond):
		t.Fatalf("no wait reported before the stream finished")
	}
	<-done
}
