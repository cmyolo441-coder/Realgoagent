package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/nova-ai/nova/internal/cloud"
)

// cloudServerAddr returns the cloud server address from the environment.
func cloudServerAddr() string {
	if v := os.Getenv("NOVA_CLOUD_SERVER"); v != "" {
		return v
	}
	return "http://localhost:8080"
}

func cloudToken() string { return os.Getenv("NOVA_CLOUD_TOKEN") }

// switchToCloud moves the TUI to a cloud session: history is handed off,
// the agent is swapped for a cloudAgent, and events stream over SSE.
func (t *TUI) switchToCloud(task string) error {
	p := t.app.Theme
	if _, ok := t.app.Agent.(*cloudAgent); ok {
		t.app.history.Append(p.Style("dim", "already in a cloud session"))
		return nil
	}
	if t.streaming {
		return fmt.Errorf("stop the running turn (esc) before switching to cloud")
	}

	client := cloud.NewClient(cloudServerAddr(), cloudToken())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Clone the local repo on the server when inside one.
	req := cloud.CreateRequest{Name: "terminal"}
	if repo, branch := localGitRemote(t.app.Cwd()); repo != "" {
		req.Repo = repo
		req.Branch = branch
	}
	info, err := client.Create(ctx, req)
	if err != nil {
		return fmt.Errorf("create cloud session: %w", err)
	}

	// Hand off the conversation so the cloud session picks up where the
	// local one left off.
	if msgs := t.app.Agent.Messages(); len(msgs) > 0 {
		if err := client.Handoff(ctx, msgs); err != nil {
			t.app.history.Append(p.Style("warning", "history handoff failed: "+err.Error()))
		}
	}

	t.app.Cloud = client
	t.app.Agent = newCloudAgent(client, t.send)
	t.app.history.Append(p.Style("success", "✓ cloud session "+info.ID))
	t.app.history.Append(p.Style("dim", "the session runs on the server — closing this terminal will not stop it"))
	if task != "" {
		t.app.history.Append(p.Style("dim", "handed off with task: "+task))
		// Submit through the normal path so the prompt is recorded.
		t.app.PromptText = task
		return t.Submit(task)
	}
	return nil
}

// switchToLocal moves the TUI back to a local agent.
func (t *TUI) switchToLocal() error {
	p := t.app.Theme
	ca, ok := t.app.Agent.(*cloudAgent)
	if !ok {
		t.app.history.Append(p.Style("dim", "already running locally"))
		return nil
	}
	if t.streaming {
		return fmt.Errorf("stop the running turn (esc) before switching to local")
	}
	ca.Close()
	t.app.Cloud = nil

	// Rebuild the local agent. The cloud workspace lives on the server;
	// its files are at ~/.goagent/cloud/sessions/<id>/workspace there.
	cfg := t.app.Cfg
	prov, m, err := cfg.ResolveModel(t.app.SelectedModel)
	if err != nil {
		return fmt.Errorf("no model available: %w", err)
	}
	if err := rebuildAgent(t, prov, m); err != nil {
		return err
	}
	t.app.history.Append(p.Style("success", "✓ back on local agent"))
	t.app.history.Append(p.Style("dim", "cloud files stay on the server — sync them with git if needed"))
	return nil
}

// localGitRemote returns the origin URL and branch for dir.
func localGitRemote(dir string) (repo, branch string) {
	if dir == "" {
		return "", ""
	}
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", ""
	}
	repo = strings.TrimSpace(string(out))
	out, err = exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return repo, ""
	}
	return repo, strings.TrimSpace(string(out))
}

func cmdCloud(t *TUI, args string) error {
	return t.switchToCloud("")
}

func cmdHandoff(t *TUI, args string) error {
	task := strings.TrimSpace(args)
	if task == "" {
		// No task: the cloud session continues from the handed-off history.
		return t.switchToCloud("")
	}
	return t.switchToCloud(task)
}

func cmdPickup(t *TUI, args string) error {
	return t.switchToLocal()
}

func cmdCloudSessions(t *TUI, args string) error {
	p := t.app.Theme
	client := cloud.NewClient(cloudServerAddr(), cloudToken())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sessions, err := client.List(ctx)
	if err != nil {
		return fmt.Errorf("list cloud sessions: %w", err)
	}
	if len(sessions) == 0 {
		t.app.history.Append(p.Style("dim", "no cloud sessions"))
		return nil
	}
	t.app.history.Append(p.Style("accent_bold", "Cloud sessions"))
	for _, s := range sessions {
		age := time.Since(s.CreatedAt).Round(time.Second)
		t.app.history.Append(fmt.Sprintf("  %s  %s  %s  %s",
			p.Style("accent2", s.ID), s.Status, p.Style("dim", age.String()), s.Name))
	}
	return nil
}
