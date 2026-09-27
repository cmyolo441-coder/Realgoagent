package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestChatURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://api.x.com/v1", "https://api.x.com/v1/chat/completions"},
		{"https://api.x.com", "https://api.x.com/chat/completions"},
		{"https://api.x.com/v1/chat/completions", "https://api.x.com/v1/chat/completions"},
		{"https://kiosapi.com/v1/", "https://kiosapi.com/v1/chat/completions"},
	}
	for _, c := range cases {
		p := Provider{BaseURL: c.in}
		if got := p.ChatURL(); got != c.want {
			t.Errorf("ChatURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestResolveModel(t *testing.T) {
	c := Default()

	if p, m, err := c.ResolveModel("kiosai/grok-4.7-free"); err != nil {
		t.Fatal(err)
	} else if p.Name != "kiosai" || m.ID != "grok-4.7-free" {
		t.Errorf("got %s/%s", p.Name, m.ID)
	}

	// bare alias
	if _, m, err := c.ResolveModel("grok"); err != nil {
		t.Fatal(err)
	} else if m.ID != "grok-4.7-free" {
		t.Errorf("alias resolved to %s", m.ID)
	}

	// bare id
	if _, m, err := c.ResolveModel("step-5-preview"); err != nil {
		t.Fatal(err)
	} else if m.Alias != "step" {
		t.Errorf("id resolved to alias %q", m.Alias)
	}

	// substring match
	if _, m, err := c.ResolveModel("mimo"); err != nil {
		t.Fatal(err)
	} else if m.ID != "xiaomi-mimo-v2.6-pro-free" {
		t.Errorf("substring resolved to %s", m.ID)
	}

	if _, _, err := c.ResolveModel("nope"); err == nil {
		t.Error("expected error for unknown model")
	}
	if _, _, err := c.ResolveModel("badprovider/model"); err == nil {
		t.Error("expected error for unknown provider")
	}
}

func TestDefaultHasAllProviders(t *testing.T) {
	c := Default()
	want := []string{"kiosai", "inferera", "stepfun"}
	if len(c.Providers) != len(want) {
		t.Fatalf("want %d providers, got %d", len(want), len(c.Providers))
	}
	for i, name := range want {
		if c.Providers[i].Name != name {
			t.Errorf("provider %d = %s, want %s", i, c.Providers[i].Name, name)
		}
		if !c.Providers[i].Enabled {
			t.Errorf("provider %s should be enabled", name)
		}
	}
	// kios models
	if len(c.Providers[0].Models) != 4 {
		t.Errorf("kiosai should have 4 models, got %d", len(c.Providers[0].Models))
	}
}

func TestDefaultKiosModel(t *testing.T) {
	c := Default()
	p, m, err := c.ResolveModel(c.DefaultModel)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "kiosai" || m.ID != "grok-4.7-free" {
		t.Errorf("default model = %s/%s", p.Name, m.ID)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := Default()
	c.path = filepath.Join(dir, "config.json")
	c.Theme = "dracula"

	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	// perms must be 0600 because the file holds API keys
	info, err := os.Stat(c.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perm = %o, want 600", info.Mode().Perm())
	}

	raw, err := os.ReadFile(c.path)
	if err != nil {
		t.Fatal(err)
	}
	var back Config
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Theme != "dracula" {
		t.Errorf("theme = %q", back.Theme)
	}
	if len(back.Providers) != 3 {
		t.Errorf("providers = %d", len(back.Providers))
	}
}

func TestFindModelPrefix(t *testing.T) {
	p := Provider{Models: []Model{{ID: "xiaomi-mimo-v2.6-pro-free"}}}
	m := p.FindModel("mimo")
	if m == nil || m.ID != "xiaomi-mimo-v2.6-pro-free" {
		t.Errorf("prefix match failed: %+v", m)
	}
	if m := p.FindModel("nothing"); m != nil {
		t.Errorf("expected nil, got %+v", m)
	}
}

func TestAPIKeyFromEnv(t *testing.T) {
	t.Setenv("MYPROV_API_KEY", "secret123")
	p := Provider{Name: "myprov"}
	if got := p.APIKeyFromEnv(); got != "secret123" {
		t.Errorf("env key = %q", got)
	}
	p.APIKey = "inline"
	if got := p.APIKeyFromEnv(); got != "inline" {
		t.Errorf("inline key should win: %q", got)
	}
}
