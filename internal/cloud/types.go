// Package cloud implements Devin-style cloud sessions for nova: the agent
// runs on a server (goagent serve) with its own workspace, and a terminal
// streams the session over SSE. The wire protocol is plain JSON over HTTP,
// so no extra dependencies are needed.
package cloud

import (
	"time"

	"github.com/nova-ai/nova/internal/llm"
)

// SessionInfo describes a cloud session.
type SessionInfo struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Repo      string    `json:"repo,omitempty"`
	Branch    string    `json:"branch,omitempty"`
	Status    string    `json:"status"` // idle | running
	CreatedAt time.Time `json:"created_at"`
}

// CreateRequest asks the server to start a session.
type CreateRequest struct {
	Name   string `json:"name,omitempty"`
	Repo   string `json:"repo,omitempty"`   // git URL or local path to clone
	Branch string `json:"branch,omitempty"` // branch to check out after clone
}

// PromptRequest submits a prompt to a running session.
type PromptRequest struct {
	Text string `json:"text"`
}

// AnswerRequest answers a pending ask_user question.
type AnswerRequest struct {
	Text string `json:"text"`
}

// PlanModeRequest toggles plan mode on a session.
type PlanModeRequest struct {
	Enabled bool `json:"enabled"`
}

// HistoryRequest replaces a session's conversation history (for /handoff).
type HistoryRequest struct {
	Messages []llm.Message `json:"messages"`
}

// HandoffRequest moves a local session to the cloud: the server clones the
// repo, checks out the branch, and injects the conversation history.
type HandoffRequest struct {
	CreateRequest
	Messages []llm.Message `json:"messages,omitempty"`
}

// ErrorResponse is the JSON body for API errors.
type ErrorResponse struct {
	Error string `json:"error"`
}
