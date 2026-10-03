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
	} else if m.ID != "mimo-v2.6-flash" {
		t.Errorf("substring resolved to %s", m.ID)
	}

	// the models added to kiosai resolve by id and by alias
	for _, ref := range []string{"longcat-2.5-preview", "longcat", "mimo-v2.6-flash", "atria-dawn-preview", "atria", "qwen3.8-flash-free", "qwen", "ling-3.1-flash", "ling", "fledge-alpha", "fledge"} {
		p, m, err := c.ResolveModel(ref)
		if err != nil {
			t.Errorf("ResolveModel(%q): %v", ref, err)
			continue
		}
		if p.Name != "kiosai" {
			t.Errorf("ResolveModel(%q) provider = %s, want kiosai", ref, p.Name)
		}
		if m.MaxOut <= 0 {
			t.Errorf("kiosai/%s has no max_output", m.ID)
		}
	}

	// inferera was removed, so nothing of it resolves any more
	if _, _, err := c.ResolveModel("inferera/union-alpha-free"); err == nil {
		t.Error("expected error for removed provider inferera")
	}
	if _, _, err := c.ResolveModel("union"); err == nil {
		t.Error("expected error for removed inferera model union")
	}

	if _, _, err := c.ResolveModel("nope"); err == nil {
		t.Error("expected error for unknown model")
	}
	if _, _, err := c.ResolveModel("badprovider/model"); err == nil {
		t.Error("expected error for unknown provider")
	}
}

func TestLoadMergesNewDefaultModels(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	stale := Default()
	stale.Providers[0].Models = stale.Providers[0].Models[:4]
	stale.path = path
	if err := stale.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Providers[0].Models) != 10 {
		t.Fatalf("stale config should merge to 10 models, got %d", len(got.Providers[0].Models))
	}
	for _, ref := range []string{"atria-dawn-preview", "atria", "qwen3.8-flash-free", "qwen", "ling-3.1-flash", "ling", "fledge-alpha", "fledge"} {
		if _, _, err := got.ResolveModel(ref); err != nil {
			t.Errorf("ResolveModel(%q): %v", ref, err)
		}
	}
}

func TestDefaultHasAllProviders(t *testing.T) {
	c := Default()
	want := []string{"kiosai", "stepfun"}
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
	// kios models: the original four plus longcat-2.5-preview, mimo-v2.6-flash, atria-dawn-preview, qwen3.8-flash-free, ling-3.1-flash and fledge-alpha
	if len(c.Providers[0].Models) != 10 {
		t.Errorf("kiosai should have 10 models, got %d", len(c.Providers[0].Models))
	}
	for _, m := range c.Providers[0].Models {
		if m.MaxOut <= 0 {
			t.Errorf("kiosai/%s has no max_output", m.ID)
		}
		if m.Context <= m.MaxOut {
			t.Errorf("kiosai/%s context %d must exceed its output limit %d",
				m.ID, m.Context, m.MaxOut)
		}
	}
	// fireworks/ember-1 was removed: it no longer has a provider of its own
	// and the model does not resolve any more.
	if _, _, err := c.ResolveModel("fireworks/accounts/fireworks/models/ember-1"); err == nil {
		t.Error("expected error for removed provider fireworks")
	}
	if _, _, err := c.ResolveModel("ember"); err == nil {
		t.Error("expected error for removed fireworks model ember")
	}
}

func TestMaxOutputTokens(t *testing.T) {
	c := Default()

	_, m, err := c.ResolveModel("kiosai/grok-4.7-free")
	if err != nil {
		t.Fatal(err)
	}

	// A short prompt leaves the model's own limit untouched: the agent asks for
	// everything the model can produce.
	if got, want := c.MaxOutputTokens(m, 2000), m.MaxOut; got != want {
		t.Errorf("short prompt = %d, want the full %d", got, want)
	}

	// A prompt that fills the window squeezes the completion down, and the
	// budget it leaves plus the prompt still fits inside the context.
	got := c.MaxOutputTokens(m, m.Context-1000)
	if got >= m.MaxOut {
		t.Errorf("long prompt = %d, want less than the full %d", got, m.MaxOut)
	}
	if got > 1000 {
		t.Errorf("long prompt = %d, want at most the 1000 tokens left", got)
	}

	// A prompt larger than the whole window still yields a positive budget
	// rather than a negative one the server would reject as malformed.
	if got := c.MaxOutputTokens(m, m.Context*2); got < 1 {
		t.Errorf("oversized prompt = %d, want a positive budget", got)
	}

	// A positive Config.MaxTokens caps every model...
	c.MaxTokens = 4096
	if got := c.MaxOutputTokens(m, 2000); got != 4096 {
		t.Errorf("capped = %d, want 4096", got)
	}
	// ...and the context window still wins over the cap.
	if got := c.MaxOutputTokens(m, m.Context-1000); got > 4096 {
		t.Errorf("capped long prompt = %d, want at most 4096", got)
	}

	// A model that declares no output limit falls back to the default, once the
	// global cap above is lifted again.
	c.MaxTokens = 0
	plain := &Model{Context: 200000}
	if got, want := c.MaxOutputTokens(plain, 1000), defaultMaxOutput; got != want {
		t.Errorf("undeclared limit = %d, want %d", got, want)
	}
}

func TestEstimateTokens(t *testing.T) {
	if got := EstimateTokens(""); got != 0 {
		t.Errorf("empty = %d, want 0", got)
	}
	if got := EstimateTokens("abcd"); got != 1 {
		t.Errorf("4 chars = %d, want 1", got)
	}
	if got := EstimateTokens("abcde"); got != 2 {
		t.Errorf("5 chars = %d, want 2", got)
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
	if len(back.Providers) != 2 {
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
