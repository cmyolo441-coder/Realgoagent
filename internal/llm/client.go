// Package llm provides streaming clients for OpenAI-compatible chat APIs.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

// Role constants.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// ContentPart is a multimodal message fragment.
type ContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// ToolCall is a function call requested by the model.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Index    int    `json:"index"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Message is one chat turn.
type Message struct {
	Role       string        `json:"role"`
	Content    string        `json:"content,omitempty"`
	Parts      []ContentPart `json:"-"`
	ToolCalls  []ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	Name       string        `json:"name,omitempty"`
}

// Tool defines a callable function exposed to the model.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction is the function schema.
type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// Request is the chat completions request payload.
type Request struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	ToolChoice  string    `json:"tool_choice,omitempty"`
	Temperature float64   `json:"temperature,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
}

// Delta is a single streamed chunk.
type Delta struct {
	Content string     `json:"content,omitempty"`
	Role    string     `json:"role,omitempty"`
	Tools   []ToolCall `json:"tool_calls,omitempty"`
}

// Chunk is a parsed SSE chunk.
type Chunk struct {
	Delta  Delta
	Done   bool
	Finish string
	Usage  *Usage
	Err    error
	Raw    string
}

// Usage reports token accounting.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Model identifies the selected model.
type Model struct {
	// Ref is "provider/model" for display.
	Ref string
	// Id is the upstream model id.
	Id string
}

// Client talks to a single provider.
type Client struct {
	BaseURL    string
	APIKey     string
	Model      string
	HTTP       *http.Client
	MaxRetries int
	// OnRetry is invoked for observability.
	OnRetry func(attempt int, err error)
	// OnWait is invoked before a cool-down sleep, with the length of the wait.
	// A 429 costs twenty seconds and a transient error up to thirty, and
	// without this the caller sees nothing at all for that whole time — the
	// turn simply stops, which is indistinguishable from a hung program.
	OnWait func(d time.Duration, err error)
}

// NewClient returns a client with sane defaults.
func NewClient(baseURL, apiKey, model string) *Client {
	return &Client{
		BaseURL:    strings.TrimSuffix(strings.TrimSpace(baseURL), "/"),
		APIKey:     apiKey,
		Model:      model,
		HTTP:       &http.Client{Timeout: 600 * time.Second},
		MaxRetries: 3,
	}
}

// httpError carries an HTTP status code for proper error classification.
type httpError struct {
	StatusCode int
	Body       string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("unexpected status %d: %s", e.StatusCode, e.Body)
}

// Stream sends req and returns a channel of chunks. The channel is closed on
// completion. The caller must drain it fully.
func (c *Client) Stream(ctx context.Context, req Request) <-chan Chunk {
	out := make(chan Chunk, 64)
	go func() {
		defer close(out)
		req.Stream = true
		req.Model = c.Model
		var lastErr error
		for attempt := 0; attempt <= c.MaxRetries; attempt++ {
			if attempt > 0 {
				d := backoff(attempt)
				if c.OnWait != nil {
					c.OnWait(d, lastErr)
				}
				select {
				case <-time.After(d):
				case <-ctx.Done():
					out <- Chunk{Err: ctx.Err()}
					return
				}
				if c.OnRetry != nil {
					c.OnRetry(attempt, lastErr)
				}
			}
			chunks, err := c.doStream(ctx, req)
			if err == nil {
				for ch := range chunks {
					select {
					case out <- ch:
						if ch.Err != nil || ch.Done {
							return
						}
					case <-ctx.Done():
						select {
						case out <- Chunk{Err: ctx.Err()}:
						default:
						}
						return
					}
				}
				return
			}
			lastErr = err
			if !retryable(err) {
				out <- Chunk{Err: err}
				return
			}
			if ctx.Err() != nil {
				out <- Chunk{Err: ctx.Err()}
				return
			}
			// 429 rate limits need a much longer cool-down than transient errors.
			if isRateLimit(err) {
				const rateLimitCooldown = 20 * time.Second
				if c.OnWait != nil {
					c.OnWait(rateLimitCooldown, err)
				}
				select {
				case <-time.After(rateLimitCooldown):
				case <-ctx.Done():
					out <- Chunk{Err: ctx.Err()}
					return
				}
			}
		}
		out <- Chunk{Err: fmt.Errorf("stream failed after %d attempts: %w", c.MaxRetries+1, lastErr)}
	}()
	return out
}

func backoff(attempt int) time.Duration {
	// exponential: 250ms, 1s, 2.25s, 4s ... capped at 30s
	d := time.Duration(attempt*attempt*attempt/3+1) * 250 * time.Millisecond
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	j := time.Duration(rand.Int63n(int64(200 * time.Millisecond)))
	return d + j
}

func retryable(err error) bool {
	if err == nil {
		return false
	}
	var he *httpError
	if errors.As(err, &he) {
		return he.StatusCode == 429 || he.StatusCode >= 500
	}
	s := err.Error()
	return strings.Contains(s, "timeout") || strings.Contains(s, "EOF") || strings.Contains(s, "connection reset")
}

// isRateLimit reports whether err is a 429 so the caller can slow down.
func isRateLimit(err error) bool {
	var he *httpError
	if errors.As(err, &he) {
		return he.StatusCode == 429
	}
	return err != nil && strings.Contains(err.Error(), "429")
}

// IsRateLimit reports whether err is a provider-side rate limit. It is
// exported so a caller can explain a twenty-second retry in the user's terms
// instead of reporting every wait as a generic connection fault.
func IsRateLimit(err error) bool { return isRateLimit(err) }

// doneMarker is the SSE payload a provider sends to close a stream. It is a
// sentinel rather than a frame, so it is recognised before anything tries to
// parse the payload as JSON.
const doneMarker = "[DONE]"

// streamBrokenError reports a stream that broke off mid-response: data
// arrived but the provider never sent its end-of-stream marker, so the turn
// would otherwise end silently on a truncated reply. It is typed so the
// agent can tell a broken stream from any other failure and offer a retry.
type streamBrokenError struct{ msg string }

func (e *streamBrokenError) Error() string { return e.msg }

// IsStreamBroken reports whether err is a mid-response stream break.
func IsStreamBroken(err error) bool {
	var target *streamBrokenError
	return errors.As(err, &target)
}

func (c *Client) doStream(ctx context.Context, req Request) (<-chan Chunk, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &httpError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	// Captured for the "no data at all" diagnostic below: a 200 carrying an
	// HTML error page or an empty body is the common shape of a misconfigured
	// base URL, and the status alone does not say so.
	httpResp := &http.Response{StatusCode: resp.StatusCode, Header: resp.Header.Clone()}

	out := make(chan Chunk, 64)
	go func() {
		defer close(out)
		defer resp.Body.Close()

		// send is the reader's only way out. Every send is paired with a
		// select on ctx so a consumer that walks away — Esc, a tool error, a
		// retry — cannot park this goroutine on a full channel: a parked
		// reader holds the response body, the TCP connection and a slot in the
		// transport's pool, and nothing ever wakes it.
		send := func(c Chunk) bool {
			select {
			case out <- c:
				return true
			case <-ctx.Done():
				return false
			}
		}

		br := bufio.NewReaderSize(resp.Body, 1<<16)
		var event string
		// A data: payload that does not parse on its own is held rather than
		// dropped, so a JSON body split across two data: lines is reassembled
		// instead of silently losing the frame.
		var partial string
		delivered := false
		sawDone := false
		// sawFinish tracks a finish_reason on any chunk. Some providers end
		// the stream with finish_reason instead of the [DONE] sentinel; that
		// is a clean end, not a break.
		sawFinish := false

		// abruptEnd reports how an EOF-terminated stream ended. A stream that
		// delivered data but never sent its end-of-stream marker died
		// mid-reply — without this the turn ended silently on a truncated
		// reply, which reads as the model just stopping mid-sentence.
		abruptEnd := func() {
			switch {
			case delivered && !sawDone && !sawFinish:
				send(Chunk{Err: &streamBrokenError{msg: "stream broke off mid-response (the provider never sent its end-of-stream marker)"}})
			case !delivered && !sawDone:
				// Nothing left to read. A 200 that carried no frame at all
				// is a failed response, not an empty reply: reporting it as
				// success would end the turn silently with no text and no
				// error for the user to see.
				send(Chunk{Err: fmt.Errorf("stream ended before any data (%s)", streamStatus(httpResp))})
			}
		}

		// frame handles one complete event payload.
		frame := func(data string) bool {
			if data == "[DONE]" {
				sawDone = true
				send(Chunk{Done: true})
				return false
			}
			chunk, err := parseChunk(data)
			if err != nil {
				return true // heartbeats / keep-alives / unparseable
			}
			chunk.Raw = event + ":" + data
			event = ""
			delivered = true
			if chunk.Finish != "" {
				sawFinish = true
			}
			if !send(chunk) {
				return false
			}
			return !chunk.Done
		}

		for {
			line, rerr := br.ReadString('\n')
			// A stream may end without a trailing newline. The bytes already
			// read are a complete event and must be parsed, not discarded.
			last := rerr != nil
			if last {
				if rerr != io.EOF {
					send(Chunk{Err: rerr})
					return
				}
				if line == "" {
					abruptEnd()
					return
				}
			}
			line = strings.TrimRight(line, "\r\n")

			switch {
			case line == "":
				// Event boundary. Anything still buffered was an unparseable
				// prefix, not a split frame; drop it rather than carrying it
				// into the next event.
				partial = ""
			case strings.HasPrefix(line, ":"):
				// comment / keep-alive
			case strings.HasPrefix(line, "event:"):
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if data == "" {
					continue
				}
				// The end-of-stream sentinel is checked before the JSON
				// validation below, and it has to be. "[DONE]" is not valid
				// JSON, so a validator runs first treats it as the first half of a
				// frame that is still arriving: it is buffered, frame() is never
				// called, and the sentinel is silently dropped. The turn then ends
				// on EOF rather than on the provider's own end-of-stream, and a
				// response that carried nothing but the sentinel is reported as
				// "stream ended before any data" instead of the empty reply the
				// provider actually sent.
				if data == doneMarker {
					if !frame(doneMarker) {
						return
					}
					if last {
						return
					}
					continue
				}
				if partial != "" {
					// A data: field may span several lines; SSE joins them
					// with a newline, which is exactly what JSON needs.
					data = partial + "\n" + data
					partial = ""
				}
				// Try the payload as it stands. Only if it is not yet valid
				// JSON is it held for the next data: line.
				if !json.Valid([]byte(data)) {
					partial = data
					if last {
						return
					}
					continue
				}
				if !frame(data) {
					return
				}
			default:
				// Some servers emit raw JSON without the data: prefix.
				if strings.HasPrefix(line, "{") {
					if !frame(line) {
						return
					}
				}
			}
			if last {
				// The final bytes were a frame, not the terminator: the
				// provider ended without its end-of-stream marker.
				abruptEnd()
				return
			}
		}
	}()
	return out, nil
}

// streamStatus describes a 200 response for a diagnostic, so an empty stream
// can say what it actually was.
func streamStatus(r *http.Response) string {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return fmt.Sprintf("status %d, no content-type", r.StatusCode)
	}
	return fmt.Sprintf("status %d, content-type %s", r.StatusCode, ct)
}

// streamChunk is the wire shape of one SSE data frame. It is hoisted to
// package level because parseChunk runs once per streamed token: a struct
// type declared inside the function forces the compiler to build its implicit
// descriptor work on every call instead of once.
type streamChunk struct {
	Choices []struct {
		Delta        Delta  `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
}

func parseChunk(data string) (Chunk, error) {
	var raw streamChunk
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return Chunk{}, err
	}
	var ch Chunk
	if raw.Usage != nil {
		ch.Usage = raw.Usage
	}
	if len(raw.Choices) > 0 {
		ch.Delta = raw.Choices[0].Delta
		ch.Finish = raw.Choices[0].FinishReason
	}
	return ch, nil
}

// Collect drains a stream channel into a final assistant message.
func Collect(ch <-chan Chunk) (Message, *Usage, error) {
	msg := Message{Role: RoleAssistant}
	var finish string
	var usage *Usage
	// A builder, not +=: content arrives a token at a time, and re-copying
	// the whole reply per token turns a long answer into a quadratic copy.
	var content strings.Builder
	for c := range ch {
		if c.Err != nil {
			return msg, usage, c.Err
		}
		if c.Delta.Content != "" {
			content.WriteString(c.Delta.Content)
		}
		for _, tc := range c.Delta.Tools {
			mergeToolCall(&msg, tc)
		}
		if c.Finish != "" {
			finish = c.Finish
		}
		if c.Usage != nil {
			usage = c.Usage
		}
	}
	msg.Content = content.String()
	if finish == "tool_calls" && len(msg.ToolCalls) == 0 {
		return msg, usage, fmt.Errorf("model requested tool calls but none were streamed")
	}
	return msg, usage, nil
}

// mergeToolCall merges streamed tool-call fragments into the message.
func mergeToolCall(m *Message, tc ToolCall) {
	// Use Index to find the right tool call slot (OpenAI streaming format).
	if tc.Index < len(m.ToolCalls) {
		appendToolFragment(&m.ToolCalls[tc.Index], tc)
		return
	}
	// Fallback: match by ID for providers that don't send Index.
	if tc.ID != "" {
		for i := range m.ToolCalls {
			if m.ToolCalls[i].ID == tc.ID {
				appendToolFragment(&m.ToolCalls[i], tc)
				return
			}
		}
	}
	cp := tc
	cp.Type = "function"
	m.ToolCalls = append(m.ToolCalls, cp)
}

func appendToolFragment(dst *ToolCall, src ToolCall) {
	if src.Function.Name != "" {
		dst.Function.Name = src.Function.Name
	}
	if src.Function.Arguments != "" {
		dst.Function.Arguments += src.Function.Arguments
	}
	if dst.Type == "" {
		dst.Type = "function"
	}
}
