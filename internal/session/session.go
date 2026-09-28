// Package session persists conversations so they can be resumed.
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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

	mu sync.Mutex
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

// Save writes the session to disk atomically.
func (s *Session) Save() error {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	s.mu.Lock()
	s.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(s, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	path := filepath.Join(Dir(), s.ID+".json")
	return atomicWriteFile(path, data, 0o600)
}

// validateID rejects IDs containing path separators or parent directory
// references, which would escape the sessions directory via filepath.Join.
func validateID(id string) error {
	if id == "" {
		return fmt.Errorf("session id must not be empty")
	}
	if strings.ContainsRune(id, '/') || strings.ContainsRune(id, '\\') || strings.ContainsRune(id, os.PathSeparator) {
		return fmt.Errorf("invalid session id %q", id)
	}
	if strings.Contains(id, "..") {
		return fmt.Errorf("invalid session id %q", id)
	}
	return nil
}

// Path returns the on-disk location of the session.
func (s *Session) Path() string {
	if err := validateID(s.ID); err != nil {
		return ""
	}
	return filepath.Join(Dir(), s.ID+".json")
}

// Load reads a session by ID.
func Load(id string) (*Session, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
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
	if err := validateID(id); err != nil {
		return err
	}
	return os.Remove(filepath.Join(Dir(), id+".json"))
}

// Summary returns a one-line description for listings.
func (s *Session) Summary() string {
	title := s.Title
	if title == "" {
		title = firstUserMessage(s.Messages)
	}
	runes := []rune(title)
	if len(runes) > 60 {
		title = string(runes[:57]) + "..."
	}
	return fmt.Sprintf("%s  %s  %s  %s", s.ID, s.UpdatedAt.Format("Jan 02 15:04"), s.Model, title)
}

// atomicWriteFile writes data to path atomically by writing to a temporary
// file in the same directory first, then renaming it over the destination.
// This prevents partial writes from corrupting existing session files.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-session-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
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
