// Package session persists conversations so they can be resumed.
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nova-ai/nova/internal/config"
	"github.com/nova-ai/nova/internal/llm"
)

// Session is a saved conversation.
type Session struct {
	ID        string        `json:"id"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	Workspace string        `json:"workspace"`
	Model     string        `json:"model"`
	Title     string        `json:"title"`
	Messages  []llm.Message `json:"messages"`
	Usage     llm.Usage     `json:"usage,omitempty"`
}

// Dir returns the session directory.
func Dir() string {
	return filepath.Join(config.Home(), "sessions")
}

// New creates an empty session.
func New(workspace, model string) *Session {
	now := time.Now()
	return &Session{
		ID:        fmt.Sprintf("%s-%s", now.Format("20060102-150405"), randSuffix()),
		CreatedAt: now,
		UpdatedAt: now,
		Workspace: workspace,
		Model:     model,
	}
}

func randSuffix() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 6)
	for i := range b {
		b[i] = alphabet[time.Now().UnixNano()%int64(len(alphabet))]
	}
	return string(b)
}

// Save writes the session to disk.
func (s *Session) Save() error {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	s.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(Dir(), s.ID+".json")
	return os.WriteFile(path, data, 0o600)
}

// Path returns the on-disk location of the session.
func (s *Session) Path() string { return filepath.Join(Dir(), s.ID+".json") }

// Load reads a session by ID.
func Load(id string) (*Session, error) {
	data, err := os.ReadFile(filepath.Join(Dir(), id+".json"))
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Latest returns the most recently updated session.
func Latest() (*Session, error) {
	all, err := List()
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("no saved sessions")
	}
	return all[0], nil
}

// ResolvePrefix finds a session whose ID starts with the given prefix, so users
// can type the short tail of an id instead of the whole timestamp.
func ResolvePrefix(prefix string) (*Session, error) {
	all, err := List()
	if err != nil {
		return nil, err
	}
	var match *Session
	for _, s := range all {
		if strings.HasPrefix(s.ID, prefix) {
			if match != nil {
				return nil, fmt.Errorf("session prefix %q is ambiguous", prefix)
			}
			match = s
		}
	}
	if match == nil {
		return nil, fmt.Errorf("no session matching %q", prefix)
	}
	return match, nil
}

// List returns saved sessions sorted newest first.
func List() ([]*Session, error) {
	entries, err := os.ReadDir(Dir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]*Session, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		s, err := Load(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue // skip unreadable/partial files
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

// Delete removes a session file.
func Delete(id string) error {
	return os.Remove(filepath.Join(Dir(), id+".json"))
}

// Summary returns a one-line description for listings.
func (s *Session) Summary() string {
	title := s.Title
	if title == "" {
		title = firstUserMessage(s.Messages)
	}
	if len(title) > 60 {
		title = title[:57] + "..."
	}
	return fmt.Sprintf("%s  %s  %s  %s", s.ID, s.UpdatedAt.Format("Jan 02 15:04"), s.Model, title)
}

func firstUserMessage(msgs []llm.Message) string {
	for _, m := range msgs {
		if m.Role == "user" && strings.TrimSpace(m.Content) != "" {
			line := m.Content
			if i := strings.IndexByte(line, '\n'); i >= 0 {
				line = line[:i]
			}
			return line
		}
	}
	return "(empty)"
}
