// Package config loads, validates and persists Nova's configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Provider describes an OpenAI-compatible LLM endpoint.
type Provider struct {
	Name    string  `json:"name"`
	Kind    string  `json:"kind,omitempty"` // "openai" | "anthropic"; default openai
	BaseURL string  `json:"base_url"`
	APIKey  string  `json:"api_key"`
	Models  []Model `json:"models"`
	Enabled bool    `json:"enabled"`
}

// Model is a single selectable model under a provider.
type Model struct {
	ID       string `json:"id"`
	Alias    string `json:"alias,omitempty"`
	Context  int    `json:"context_window,omitempty"`
	MaxOut   int    `json:"max_output,omitempty"`
	Vision   bool   `json:"vision,omitempty"`
	Reasoner bool   `json:"reasoner,omitempty"`
}

// Config is the root configuration document.
type Config struct {
	DefaultModel  string            `json:"default_model,omitempty"`
	Providers     []Provider        `json:"providers"`
	Theme         string            `json:"theme,omitempty"`
	AutoApprove   bool              `json:"auto_approve,omitempty"`
	MaxIterations int               `json:"max_iterations,omitempty"`
	Temperature   float64           `json:"temperature,omitempty"`
	MaxTokens     int               `json:"max_tokens,omitempty"`
	Shell         string            `json:"shell,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	MCP           []MCPConfig       `json:"mcp,omitempty"`
	Prompt        PromptConfig      `json:"prompt,omitempty"`

	path string `json:"-"`
}

// PromptConfig controls interactive prompt behaviour.
type PromptConfig struct {
	Animation   bool   `json:"animation,omitempty"`
	Smooth      bool   `json:"smooth_scroll,omitempty"`
	Mouse       bool   `json:"mouse,omitempty"`
	Transparent bool   `json:"transparent,omitempty"`
	Spinner     string `json:"spinner,omitempty"`
}

// MCPConfig is a reserved hook for external tool servers.
type MCPConfig struct {
	Name string   `json:"name"`
	Cmd  []string `json:"command"`
}

// Home returns the Nova data directory.
func Home() string {
	if v := os.Getenv("NOVA_HOME"); v != "" {
		return v
	}
	if runtime.GOOS == "windows" {
		if v := os.Getenv("APPDATA"); v != "" {
			return filepath.Join(v, "nova")
		}
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ".nova"
	}
	return filepath.Join(h, ".nova")
}

// ProjectDir returns per-project state so several repos can coexist.
func ProjectDir(cwd string) string {
	sum := uint32(2166136261)
	for i := 0; i < len(cwd); i++ {
		sum ^= uint32(cwd[i])
		sum *= 16777619
	}
	return filepath.Join(Home(), "projects", fmt.Sprintf("%08x", sum))
}

// Dir returns the global config directory.
func Dir() string { return filepath.Join(Home(), "config") }

// Path returns the global config file path.
func Path() string { return filepath.Join(Dir(), "config.json") }

// Default returns a default configuration seeded with known providers.
func Default() *Config {
	return &Config{
		DefaultModel:  "kiosai/grok-4.7-free",
		Theme:         "nova",
		MaxIterations: 60,
		Temperature:   0.6,
		MaxTokens:     8192,
		AutoApprove:   false,
		Providers: []Provider{
			{
				Name:    "kiosai",
				Kind:    "openai",
				BaseURL: "https://kiosapi.com/v1",
				APIKey:  "sk-cFXQ576lsIctpudkYD5lPniF5UgHGLy1nKeXDscCEvK1LMZV",
				Enabled: true,
				Models: []Model{
					{ID: "muse-spark-1.3-contributor", Alias: "muse", Context: 262144},
					{ID: "grok-4.7-free", Alias: "grok", Context: 262144, Reasoner: true},
					{ID: "deepseek-v4.1-flash-free", Alias: "deepseek", Context: 128000},
					{ID: "space-bunny-alpha", Alias: "bunny", Context: 262144},
				},
			},
			{
				Name:    "inferera",
				Kind:    "openai",
				BaseURL: "https://api.inferera.com/v1",
				APIKey:  "sk-TPfzw5EFzUZOAhGB58D46702447b4e45BbF588B07eF1Bf20",
				Enabled: true,
				Models: []Model{
					{ID: "coding-kimi-k3-free", Alias: "kimi", Context: 262144, Reasoner: false},
					{ID: "union-alpha-free", Alias: "union", Context: 262144},
					{ID: "xiaomi-mimo-v2.6-pro-free", Alias: "mimo", Context: 262144},
				},
			},
			{
				Name:    "stepfun",
				Kind:    "openai",
				BaseURL: "https://api.stepfun.ai/step_plan/v1",
				APIKey:  "1fLhqREkwguf6zoWaiD4we3Y7TObp7kk0qYNH1y1kUf6MsjOU91fvOjgN52RFCr4i",
				Enabled: true,
				Models: []Model{
					{ID: "step-5-preview", Alias: "step", Context: 1000000, MaxOut: 65536, Vision: true},
				},
			},
		},
		Shell:  defaultShell(),
		Prompt: PromptConfig{Animation: true, Smooth: true, Mouse: true, Spinner: "braille"},
	}
}

func defaultShell() string {
	if runtime.GOOS == "windows" {
		if p := os.Getenv("COMSPEC"); p != "" {
			return p
		}
		return "cmd.exe"
	}
	if p := os.Getenv("SHELL"); p != "" {
		return p
	}
	if _, err := os.Stat("/bin/bash"); err == nil {
		return "/bin/bash"
	}
	return "/bin/sh"
}

// Load reads config from an explicit path.
func LoadFrom(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := Default()
	if err := json.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.path = path
	return c, nil
}

// Load reads config from disk, creating a default one when absent.
func Load() (*Config, error) {
	p := Path()
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			c := Default()
			c.path = p
			if err := c.Save(); err != nil {
				return c, nil
			}
			return c, nil
		}
		return nil, err
	}
	c := Default()
	if err := json.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	c.path = p
	if len(c.Providers) == 0 {
		c.Providers = Default().Providers
	}
	if c.MaxIterations <= 0 {
		c.MaxIterations = 60
	}
	if c.Temperature <= 0 {
		c.Temperature = 0.6
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = 8192
	}
	return c, nil
}

// Save writes the config atomically with 0600 permissions (contains API keys).
func (c *Config) Save() error {
	if c.path == "" {
		c.path = Path()
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, c.path); err != nil {
		os.Remove(tmp)
		return err
	}
	// Drop API keys from the on-disk file if the user exported them instead.
	return nil
}

// Path returns the config file location.
func (c *Config) Path() string {
	if c.path == "" {
		c.path = Path()
	}
	return c.path
}

// ResolveModel finds a provider/model pair by any of: "provider/model",
// bare model id, or alias.
func (c *Config) ResolveModel(ref string) (*Provider, *Model, error) {
	if ref == "" {
		ref = c.DefaultModel
	}
	// exact provider/model
	if i := strings.Index(ref, "/"); i > 0 {
		pn, mid := ref[:i], ref[i+1:]
		for i := range c.Providers {
			if !strings.EqualFold(c.Providers[i].Name, pn) {
				continue
			}
			if m := c.Providers[i].FindModel(mid); m != nil {
				return &c.Providers[i], m, nil
			}
			return nil, nil, fmt.Errorf("model %q not found in provider %q", mid, pn)
		}
		return nil, nil, fmt.Errorf("unknown provider %q", pn)
	}
	// bare model id or alias across providers
	for i := range c.Providers {
		if m := c.Providers[i].FindModel(ref); m != nil {
			return &c.Providers[i], m, nil
		}
	}
	return nil, nil, fmt.Errorf("unknown model %q", ref)
}

// FindModel matches by id or alias (case-insensitive).
func (p *Provider) FindModel(ref string) *Model {
	for i := range p.Models {
		if strings.EqualFold(p.Models[i].ID, ref) || strings.EqualFold(p.Models[i].Alias, ref) {
			return &p.Models[i]
		}
	}
	// prefix match so "mimo" finds "xiaomi-mimo-v2.6-pro-free"
	var sub *Model
	for i := range p.Models {
		id := strings.ToLower(p.Models[i].ID)
		if strings.Contains(id, strings.ToLower(ref)) {
			if sub == nil || len(p.Models[i].ID) < len(sub.ID) {
				sub = &p.Models[i]
			}
		}
	}
	return sub
}

// ChatURL returns the completions endpoint for the provider.
func (p *Provider) ChatURL() string {
	base := strings.TrimSuffix(strings.TrimSpace(p.BaseURL), "/")
	switch {
	case strings.HasSuffix(base, "/chat/completions"):
		return base
	case strings.HasSuffix(base, "/v1"):
		return base + "/chat/completions"
	default:
		return base + "/chat/completions"
	}
}

// APIKeyFromEnv allows keeping secrets out of the config file.
func (p *Provider) APIKeyFromEnv() string {
	if p.APIKey != "" {
		return p.APIKey
	}
	for _, suffix := range []string{"_API_KEY", "_KEY", "_TOKEN"} {
		env := strings.ToUpper(strings.ReplaceAll(p.Name, "-", "_")) + suffix
		if v := os.Getenv(env); v != "" {
			return v
		}
	}
	return ""
}
