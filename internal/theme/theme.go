// Package theme provides colour palettes and styling helpers for Nova's TUI.
package theme

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
)

// Palette holds all colours used by the interface.
type Palette struct {
	Name string

	Accent   string
	Accent2  string
	Text     string
	Dim      string
	Success  string
	Warning  string
	Error    string
	Tool     string
	UserBG   string
	Box      string
	Border   string
	SelBG    string
	BorderBG string
}

// Themes is the built-in palette registry.
var Themes = map[string]Palette{
	"nova": {
		Name: "nova", Accent: "#a78bfa", Accent2: "#22d3ee", Text: "#e5e7eb",
		Dim: "#6b7280", Success: "#34d399", Warning: "#fbbf24", Error: "#f87171",
		Tool: "#f0abfc", UserBG: "#1f2937", Box: "#a78bfa", Border: "#4b5563",
		SelBG: "#374151", BorderBG: "#111827",
	},
	"catppuccin": {
		Name: "catppuccin", Accent: "#cba6f7", Accent2: "#89b4fa", Text: "#cdd6f4",
		Dim: "#6c7086", Success: "#a6e3a1", Warning: "#f9e2af", Error: "#f38ba8",
		Tool: "#f5c2e7", UserBG: "#313244", Box: "#cba6f7", Border: "#6c7086",
		SelBG: "#45475a", BorderBG: "#181825",
	},
	"dracula": {
		Name: "dracula", Accent: "#bd93f9", Accent2: "#8be9fd", Text: "#f8f8f2",
		Dim: "#6272a4", Success: "#50fa7b", Warning: "#f1fa8c", Error: "#ff5555",
		Tool: "#ff79c6", UserBG: "#44475a", Box: "#bd93f9", Border: "#6272a4",
		SelBG: "#44475a", BorderBG: "#282a36",
	},
	"gruvbox": {
		Name: "gruvbox", Accent: "#d3869b", Accent2: "#8ec07c", Text: "#ebdbb2",
		Dim: "#928374", Success: "#b8bb26", Warning: "#fabd2f", Error: "#fb4934",
		Tool: "#d3869b", UserBG: "#3c3836", Box: "#d3869b", Border: "#665c54",
		SelBG: "#504945", BorderBG: "#282828",
	},
	"nord": {
		Name: "nord", Accent: "#88c0d0", Accent2: "#81a1c1", Text: "#eceff4",
		Dim: "#4c566a", Success: "#a3be8c", Warning: "#ebcb8b", Error: "#bf616a",
		Tool: "#b48ead", UserBG: "#3b4252", Box: "#88c0d0", Border: "#4c566a",
		SelBG: "#434c5e", BorderBG: "#2e3440",
	},
	"mono": {
		Name: "mono", Accent: "#e5e7eb", Accent2: "#9ca3af", Text: "#e5e7eb",
		Dim: "#6b7280", Success: "#e5e7eb", Warning: "#d1d5db", Error: "#f87171",
		Tool: "#9ca3af", UserBG: "#1f2937", Box: "#e5e7eb", Border: "#4b5563",
		SelBG: "#374151", BorderBG: "#111827",
	},
}

// Names lists available theme names sorted.
func Names() []string {
	out := make([]string, 0, len(Themes))
	for k := range Themes {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Get returns a theme by name, falling back to nova.
func Get(name string) Palette {
	if p, ok := Themes[strings.ToLower(strings.TrimSpace(name))]; ok {
		return p
	}
	if np := os.Getenv("NOVA_THEME"); np != "" {
		if p, ok := Themes[strings.ToLower(np)]; ok {
			return p
		}
	}
	return Themes["nova"]
}

// colorProfile degrades gracefully: we always emit 24-bit colour, and terminals
// that lack support will simply clamp.
var trueColorSupported atomic.Bool

func init() { trueColorSupported.Store(true) }

// SetTrueColor overrides colour capability (used by --no-color).
func SetTrueColor(v bool) { trueColorSupported.Store(v) }

// fg renders s in the given hex colour.
func fg(hex, s string) string {
	if !trueColorSupported.Load() {
		return s
	}
	r, g, b := hexRGB(hex)
	if r < 0 {
		return s
	}
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm%s\x1b[0m", r, g, b, s)
}

// bg renders s on the given hex background.
func bg(hex, s string) string {
	if !trueColorSupported.Load() {
		return s
	}
	r, g, b := hexRGB(hex)
	if r < 0 {
		return s
	}
	return fmt.Sprintf("\x1b[48;2;%d;%d;%dm%s\x1b[0m", r, g, b, s)
}

// fgBold renders s bold in the given colour.
func fgBold(hex, s string) string {
	if !trueColorSupported.Load() {
		return s
	}
	return "\x1b[1m" + fg(hex, s)
}

func hexRGB(hex string) (int, int, int) {
	h := strings.TrimPrefix(strings.TrimSpace(hex), "#")
	if len(h) == 3 {
		return hexToByte(h[0]) * 17, hexToByte(h[1]) * 17, hexToByte(h[2]) * 17
	}
	if len(h) != 6 {
		return -1, -1, -1
	}
	return hexToByte(h[0])*16 + hexToByte(h[1]),
		hexToByte(h[2])*16 + hexToByte(h[3]),
		hexToByte(h[4])*16 + hexToByte(h[5])
}

func hexToByte(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return 0
}

// Style renders text in a named palette slot.
func (p Palette) Style(slot, s string) string {
	switch slot {
	case "accent":
		return fg(p.Accent, s)
	case "accent2":
		return fg(p.Accent2, s)
	case "accent_bold":
		return fgBold(p.Accent, s)
	case "text":
		return fg(p.Text, s)
	case "dim":
		return fg(p.Dim, s)
	case "success":
		return fg(p.Success, s)
	case "warning":
		return fg(p.Warning, s)
	case "error":
		return fg(p.Error, s)
	case "tool":
		return fg(p.Tool, s)
	case "border":
		return fg(p.Border, s)
	case "sel":
		return bg(p.SelBG, s)
	case "user":
		return bg(p.UserBG, s)
	}
	return s
}
