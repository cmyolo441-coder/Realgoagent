// Package llm provides streaming clients for OpenAI-compatible chat APIs.
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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
				select {
				case <-time.After(backoff(attempt)):
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
						out <- Chunk{Err: ctx.Err()}
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
				select {
				case <-time.After(20 * time.Second):
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
	s := err.Error()
	for _, needle := range []string{"429", "500", "502", "503", "504", "timeout", "EOF", "connection reset"} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return strings.Contains(s, "unexpected status 5")
}

// isRateLimit reports whether err is a 429 so the caller can slow down.
func isRateLimit(err error) bool {
	return err != nil && strings.Contains(err.Error(), "429")
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
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	out := make(chan Chunk, 64)
	go func() {
		defer close(out)
		defer resp.Body.Close()

		br := bufio.NewReaderSize(resp.Body, 1<<16)
		var event string
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					out <- Chunk{Err: err}
				}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				continue
			}
			switch {
			case strings.HasPrefix(line, ":"):
				continue
			case strings.HasPrefix(line, "event:"):
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
				continue
			case strings.HasPrefix(line, "data:"):
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if data == "" {
					continue
				}
				if data == "[DONE]" {
					out <- Chunk{Done: true}
					return
				}
				chunk, err := parseChunk(data)
				if err != nil {
					continue // tolerate heartbeats / keep-alives
				}
				chunk.Raw = event + ":" + data
				event = ""
				out <- chunk
				if chunk.Done {
					return
				}
			default:
				// Some servers emit raw JSON without the data: prefix.
				if strings.HasPrefix(line, "{") {
					chunk, err := parseChunk(line)
					if err == nil {
						out <- chunk
						if chunk.Done {
							return
						}
					}
				}
			}
		}
	}()
	return out, nil
}

func parseChunk(data string) (Chunk, error) {
	var raw struct {
		Choices []struct {
			Delta        Delta  `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *Usage `json:"usage"`
	}
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
	for c := range ch {
		if c.Err != nil {
			return msg, usage, c.Err
		}
		if c.Delta.Content != "" {
			msg.Content += c.Delta.Content
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
	if finish == "tool_calls" && len(msg.ToolCalls) == 0 {
		return msg, usage, fmt.Errorf("model requested tool calls but none were streamed")
	}
	return msg, usage, nil
}

// mergeToolCall merges streamed tool-call fragments into the message.
func mergeToolCall(m *Message, tc ToolCall) {
	if tc.ID != "" {
		for i := range m.ToolCalls {
			if m.ToolCalls[i].ID == tc.ID {
				appendToolFragment(&m.ToolCalls[i], tc)
				return
			}
		}
		cp := tc
		cp.Type = "function"
		m.ToolCalls = append(m.ToolCalls, cp)
		return
	}
	// fragment without id: attach to last known index
	if len(m.ToolCalls) > 0 {
		appendToolFragment(&m.ToolCalls[len(m.ToolCalls)-1], tc)
		return
	}
	// index-based fragment
	m.ToolCalls = append(m.ToolCalls, tc)
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
