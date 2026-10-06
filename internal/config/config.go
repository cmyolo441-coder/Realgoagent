// Package config loads, validates and persists Nova's configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

// OutputLimit is the largest completion the model can produce, falling back to
// defaultMaxOutput when the config does not state one. The number doubles as
// the request ceiling, so a generous value is safe: a provider that caps lower
// simply stops streaming early, whereas a value set too low truncates the
// answer mid-sentence.
func (m *Model) OutputLimit() int {
	if m == nil || m.MaxOut <= 0 {
		return defaultMaxOutput
	}
	return m.MaxOut
}

const (
	// defaultMaxOutput is assumed when a model declares no max_output. It is
	// high enough that a normal agent turn never truncates, and the
	// context-window clamp in MaxOutputTokens keeps it reachable.
	defaultMaxOutput = 32768

	// contextMarginTokens is headroom kept free inside the context window so the
	// request carries slack for the provider's own framing, tokenizer drift and
	// reasoning tokens the estimate cannot see. Without it a prompt estimated
	// at exactly the window size is rejected as too long.
	contextMarginTokens = 8192

	// minOutputTokens is the floor applied after clamping. A prompt can fill
	// the whole window on its own; asking for a token or two is still refused,
	// but it fails fast and predictably instead of sending a nonsensical budget.
	minOutputTokens = 256
)

// EstimateTokens approximates a token count for s without a tokenizer. English
// prose and source code both land near four characters per token, which is
// accurate enough to keep a request inside the context window.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}

// MaxOutputTokens returns the max_tokens to request from m when the prompt is
// about promptTokens tokens long.
//
// Config.MaxTokens is an optional ceiling: set it to cap output across models,
// leave it at zero to let each model use its own limit. Either way the result
// is clamped to the space left in the model's context window, so a long
// conversation narrows the answer instead of overrunning the window.
func (c *Config) MaxOutputTokens(m *Model, promptTokens int) int {
	if m == nil {
		if c.MaxTokens > 0 {
			return c.MaxTokens
		}
		return defaultMaxOutput
	}
	limit := m.OutputLimit()
	if c.MaxTokens > 0 && c.MaxTokens < limit {
		limit = c.MaxTokens
	}
	if m.Context > 0 {
		if room := m.Context - promptTokens - contextMarginTokens; room < limit {
			limit = room
		}
	}
	if limit < minOutputTokens {
		limit = minOutputTokens
	}
	return limit
}

// Config is the root configuration document.
type Config struct {
	DefaultModel  string            `json:"default_model,omitempty"`
	Providers     []Provider        `json:"providers"`
	Theme         string            `json:"theme,omitempty"`
	MaxIterations int               `json:"max_iterations,omitempty"`
	Temperature   float64           `json:"temperature,omitempty"`
	MaxTokens     int               `json:"max_tokens,omitempty"`
	Shell         string            `json:"shell,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	MCP           []MCPConfig       `json:"mcp,omitempty"`
	Prompt        PromptConfig      `json:"prompt,omitempty"`

	path     string
	pathOnce sync.Once
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
		// Zero means "no global ceiling": each request is sized from the active
		// model's own output limit and its remaining context window.
		MaxTokens:   0,
		Providers: []Provider{
			{
				Name:    "kiosai",
				Kind:    "openai",
				BaseURL: "https://router.kiosapi.com/v1",
				Enabled: true,
				Models: []Model{
					{ID: "muse-spark-1.3-contributor", Alias: "muse", Context: 262144, MaxOut: 131072},
					{ID: "grok-4.7-free", Alias: "grok", Context: 262144, MaxOut: 131072, Reasoner: true},
					{ID: "deepseek-v4.1-flash-free", Alias: "deepseek", Context: 131072, MaxOut: 65536},
					{ID: "space-bunny-alpha", Alias: "bunny", Context: 262144, MaxOut: 131072},
					{ID: "longcat-2.5-preview", Alias: "longcat", Context: 262144, MaxOut: 131072},
					{ID: "mimo-v2.6-flash", Alias: "mimo", Context: 262144, MaxOut: 131072},
					{ID: "atria-dawn-preview", Alias: "atria", Context: 262144, MaxOut: 131072},
					{ID: "qwen3.8-flash-free", Alias: "qwen", Context: 262144, MaxOut: 131072},
					{ID: "ling-3.1-flash", Alias: "ling", Context: 262144, MaxOut: 131072},
					{ID: "fledge-alpha", Alias: "fledge", Context: 262144, MaxOut: 131072},
				},
			},
			{
				Name:    "stepfun",
				Kind:    "openai",
				BaseURL: "https://api.stepfun.ai/step_plan/v1",
				Enabled: true,
				Models: []Model{
					{ID: "step-5-preview", Alias: "step", Context: 1000000, MaxOut: 65536, Vision: true},
				},
			},
			{
				Name:    "cline",
				Kind:    "openai",
				BaseURL: "https://api.cline.bot/api/v1",
				Enabled: true,
				Models: []Model{
					{ID: "stealth/pixel-canary", Alias: "pixel", Context: 262144, MaxOut: 131072},
					{ID: "stealth/space-bunny-alpha", Alias: "sbunny", Context: 1048576, MaxOut: 131072},
					{ID: "cline-free/deepseek-v4.1-flash", Alias: "cdeepseek", Context: 1048576, MaxOut: 131072},
					{ID: "cline-free/mimo-v2.6-flash", Alias: "cmimo", Context: 262144, MaxOut: 131072},
					{ID: "cline-free/muse-spark-1.3-contributor", Alias: "cmuse", Context: 262144, MaxOut: 131072},
					{ID: "cline-free/gemini-3.8-flash", Alias: "cgemini", Context: 1048576, MaxOut: 65536},
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

// mergeDefaultModels appends any default models missing from c, so users with
// an older saved config still see newly added models (e.g. in /models).
// Existing entries are never overwritten: match is by case-insensitive model
// ID, and whole missing providers are appended as-is.
func mergeDefaultModels(c *Config) {
	d := Default()
	for _, dp := range d.Providers {
		merged := false
		for i := range c.Providers {
			if !strings.EqualFold(c.Providers[i].Name, dp.Name) {
				continue
			}
			merged = true
			have := make(map[string]bool, len(c.Providers[i].Models))
			for _, m := range c.Providers[i].Models {
				have[strings.ToLower(m.ID)] = true
			}
			for _, dm := range dp.Models {
				if !have[strings.ToLower(dm.ID)] {
					c.Providers[i].Models = append(c.Providers[i].Models, dm)
				}
			}
			break
		}
		if !merged {
			c.Providers = append(c.Providers, dp)
		}
	}
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
	mergeDefaultModels(c)
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
				return c, err
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
	} else {
		mergeDefaultModels(c)
	}
	if c.MaxIterations <= 0 {
		c.MaxIterations = 60
	}
	if c.Temperature <= 0 {
		c.Temperature = 0.6
	}
	// MaxTokens is intentionally left alone: zero means "size each request from
	// the model", and a stale value from an older config is clamped per request.
	return c, nil
}

// Save writes the config atomically with 0600 permissions (contains API keys).
func (c *Config) Save() error {
	c.pathOnce.Do(func() {
		if c.path == "" {
			c.path = Path()
		}
	})
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
	c.pathOnce.Do(func() {
		if c.path == "" {
			c.path = Path()
		}
	})
	return c.path
}

// ResolveModel finds a provider/model pair by any of: "provider/model",
// bare model id, or alias.
func (c *Config) ResolveModel(ref string) (*Provider, *Model, error) {
	if ref == "" {
		ref = c.DefaultModel
	}
	// bare model id or alias across providers first: model IDs themselves
	// can contain slashes, so a blind split on "/" would mistake the ID
	// prefix for a provider name.
	for i := range c.Providers {
		if !c.Providers[i].Enabled {
			continue
		}
		if m := c.Providers[i].FindModel(ref); m != nil {
			return &c.Providers[i], m, nil
		}
	}
	// exact provider/model
	if i := strings.Index(ref, "/"); i > 0 {
		pn, mid := ref[:i], ref[i+1:]
		for i := range c.Providers {
			if !strings.EqualFold(c.Providers[i].Name, pn) {
				continue
			}
			if !c.Providers[i].Enabled {
				return nil, nil, fmt.Errorf("provider %q is disabled", pn)
			}
			if m := c.Providers[i].FindModel(mid); m != nil {
				return &c.Providers[i], m, nil
			}
			return nil, nil, fmt.Errorf("model %q not found in provider %q", mid, pn)
		}
		return nil, nil, fmt.Errorf("unknown provider %q", pn)
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
	// Build-time embedded fallback (set via -ldflags "-X ...embeddedKeys...").
	// Lets release binaries work without any manual key configuration.
	return embeddedKeyFor(p.Name)
}

// embeddedAPIKeys holds build-time injected keys, one per provider.
// Set via: go build -ldflags "-X github.com/nova-ai/nova/internal/config.embeddedKiosAIKey=..."
// The keys never appear in source; they only exist inside the built binary.
var (
	embeddedKiosAIKey  string
	embeddedStepFunKey string
	embeddedClineKey   string
)

func embeddedKeyFor(provider string) string {
	switch strings.ToLower(provider) {
	case "kiosai":
		return embeddedKiosAIKey
	case "stepfun":
		return embeddedStepFunKey
	case "cline":
		return embeddedClineKey
	}
	return ""
}
