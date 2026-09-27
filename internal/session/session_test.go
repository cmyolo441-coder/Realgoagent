package session

import (
	"testing"
	"time"

	"github.com/nova-ai/nova/internal/llm"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("NOVA_HOME", t.TempDir())
	s := New("/tmp/ws", "kiosai/grok")
	s.Messages = []llm.Message{
		{Role: "user", Content: "build the thing"},
		{Role: "assistant", Content: "done"},
	}
	s.Usage = llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	back, err := Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.Workspace != "/tmp/ws" || back.Model != "kiosai/grok" {
		t.Errorf("metadata mismatch: %+v", back)
	}
	if len(back.Messages) != 2 {
		t.Errorf("messages = %d", len(back.Messages))
	}
	if back.Usage.TotalTokens != 15 {
		t.Errorf("usage = %+v", back.Usage)
	}
}

func TestListSortedNewestFirst(t *testing.T) {
	t.Setenv("NOVA_HOME", t.TempDir())
	a := New("/w", "m")
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	b := New("/w", "m")
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	all, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(all))
	}
	if all[0].ID != b.ID {
		t.Errorf("newest first violated: %s then %s", all[0].ID, all[1].ID)
	}
}

func TestLatest(t *testing.T) {
	t.Setenv("NOVA_HOME", t.TempDir())
	if _, err := Latest(); err == nil {
		t.Error("expected error with no sessions")
	}
	s := New("/w", "m")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Latest()
	if err != nil || got.ID != s.ID {
		t.Fatalf("Latest() = %v, %v", got, err)
	}
}

func TestDelete(t *testing.T) {
	t.Setenv("NOVA_HOME", t.TempDir())
	s := New("/w", "m")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if err := Delete(s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(s.ID); err == nil {
		t.Error("session should be gone")
	}
}

func TestSummaryUsesFirstUserMessage(t *testing.T) {
	s := &Session{Model: "grok", Messages: []llm.Message{
		{Role: "user", Content: "first line\nsecond line"},
	}}
	got := s.Summary()
	if got == "" {
		t.Fatal("empty summary")
	}
	if !contains(got, "first line") {
		t.Errorf("summary = %q", got)
	}
	if contains(got, "second line") {
		t.Errorf("summary should be one line: %q", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
