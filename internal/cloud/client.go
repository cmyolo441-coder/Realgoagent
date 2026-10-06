package cloud

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nova-ai/nova/internal/agent"
	"github.com/nova-ai/nova/internal/llm"
)

// Client talks to a cloud server.
type Client struct {
	base   string
	token  string
	http   *http.Client
	sessID string
}

// NewClient creates a client for base (e.g. "http://localhost:8080").
func NewClient(base, token string) *Client {
	base = strings.TrimSuffix(base, "/")
	return &Client{
		base:  base,
		token: token,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

// SetSession pins the client to a session id.
func (c *Client) SetSession(id string) { c.sessID = id }

// SessionID returns the pinned session.
func (c *Client) SessionID() string { return c.sessID }

func (c *Client) req(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var er ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&er)
		if er.Error == "" {
			er.Error = resp.Status
		}
		return nil, fmt.Errorf("%s", er.Error)
	}
	return resp, nil
}

func decodeBody(resp *http.Response, v any) error {
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

// Create starts a session on the server.
func (c *Client) Create(ctx context.Context, req CreateRequest) (*SessionInfo, error) {
	resp, err := c.req(ctx, "POST", "/api/sessions", req)
	if err != nil {
		return nil, err
	}
	var info SessionInfo
	if err := decodeBody(resp, &info); err != nil {
		return nil, err
	}
	c.sessID = info.ID
	return &info, nil
}

// List returns the server's sessions.
func (c *Client) List(ctx context.Context) ([]SessionInfo, error) {
	resp, err := c.req(ctx, "GET", "/api/sessions", nil)
	if err != nil {
		return nil, err
	}
	var out []SessionInfo
	if err := decodeBody(resp, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Prompt submits a prompt to the pinned session.
func (c *Client) Prompt(ctx context.Context, text string) error {
	resp, err := c.req(ctx, "POST", "/api/sessions/"+c.sessID+"/prompt", PromptRequest{Text: text})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Cancel stops the pinned session's running turn.
func (c *Client) Cancel(ctx context.Context) error {
	resp, err := c.req(ctx, "POST", "/api/sessions/"+c.sessID+"/cancel", nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Answer delivers an ask_user answer.
func (c *Client) Answer(ctx context.Context, text string) error {
	resp, err := c.req(ctx, "POST", "/api/sessions/"+c.sessID+"/answer", AnswerRequest{Text: text})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Handoff injects local history into the pinned session.
func (c *Client) Handoff(ctx context.Context, msgs []llm.Message) error {
	resp, err := c.req(ctx, "POST", "/api/sessions/"+c.sessID+"/handoff", HistoryRequest{Messages: msgs})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Messages returns the pinned session's conversation.
func (c *Client) Messages(ctx context.Context) ([]llm.Message, error) {
	resp, err := c.req(ctx, "GET", "/api/sessions/"+c.sessID+"/messages", nil)
	if err != nil {
		return nil, err
	}
	var out []llm.Message
	if err := decodeBody(resp, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Usage returns the pinned session's token usage.
func (c *Client) Usage(ctx context.Context) (llm.Usage, error) {
	resp, err := c.req(ctx, "GET", "/api/sessions/"+c.sessID+"/usage", nil)
	if err != nil {
		return llm.Usage{}, err
	}
	var out llm.Usage
	if err := decodeBody(resp, &out); err != nil {
		return llm.Usage{}, err
	}
	return out, nil
}

// Reset clears the pinned session's conversation.
func (c *Client) Reset(ctx context.Context) error {
	resp, err := c.req(ctx, "POST", "/api/sessions/"+c.sessID+"/reset", nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// SetPlanMode toggles plan mode on the pinned session.
func (c *Client) SetPlanMode(ctx context.Context, on bool) error {
	resp, err := c.req(ctx, "POST", "/api/sessions/"+c.sessID+"/planmode", PlanModeRequest{Enabled: on})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// SystemPrompt returns the pinned session's system prompt.
func (c *Client) SystemPrompt(ctx context.Context) (string, error) {
	resp, err := c.req(ctx, "GET", "/api/sessions/"+c.sessID+"/systemprompt", nil)
	if err != nil {
		return "", err
	}
	var out map[string]string
	if err := decodeBody(resp, &out); err != nil {
		return "", err
	}
	return out["system_prompt"], nil
}

// Stream opens the SSE event stream for the pinned session, calling onEvent
// for every agent event until ctx ends. It reconnects on quiet drops with
// backoff, so a brief network blip does not kill the terminal.
func (c *Client) Stream(ctx context.Context, onEvent func(agent.Event)) error {
	backoff := time.Second
	for {
		err := c.streamOnce(ctx, onEvent)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 10*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) streamOnce(ctx context.Context, onEvent func(agent.Event)) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+"/api/sessions/"+c.sessID+"/events", nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var er ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&er)
		if er.Error == "" {
			er.Error = resp.Status
		}
		return fmt.Errorf("%s", er.Error)
	}

	br := bufio.NewReader(resp.Body)
	var data strings.Builder
	var evType string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return fmt.Errorf("stream closed")
			}
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			// The hello frame is informational; only dispatch agent events.
			if data.Len() > 0 && evType != "hello" {
				var ev agent.Event
				if jerr := json.Unmarshal([]byte(data.String()), &ev); jerr == nil {
					onEvent(ev)
				}
			}
			data.Reset()
			evType = ""
		case strings.HasPrefix(line, ":"):
			// heartbeat / comment
		case strings.HasPrefix(line, "event:"):
			evType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
}
