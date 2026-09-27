package tui

import (
	"strings"
	"testing"

	"github.com/nova-ai/nova/internal/config"
	"github.com/nova-ai/nova/internal/theme"
	"github.com/nova-ai/nova/internal/util"
)

func pickerConfig() *config.Config {
	return &config.Config{
		Providers: []config.Provider{
			{
				Name: "alpha", Enabled: true,
				Models: []config.Model{
					{ID: "a1", Alias: "one", Context: 1000},
					{ID: "a2"},
				},
			},
			{
				Name: "beta", Enabled: true,
				Models: []config.Model{{ID: "b1"}},
			},
			{
				Name: "off", Enabled: false,
				Models: []config.Model{{ID: "hidden"}},
			},
		},
	}
}

// The picker must list every model of every enabled provider, and skip the
// disabled ones — offering a model that cannot be used is worse than not
// offering it.
func TestPickerListsEnabledModels(t *testing.T) {
	p := newModelPicker(pickerConfig(), "alpha/a1")
	if p == nil {
		t.Fatal("picker is nil for a config with models")
	}
	if len(p.items) != 3 {
		t.Fatalf("picker has %d items, want 3 (disabled provider excluded): %v", len(p.items), itemRefs(p))
	}
	for _, it := range p.items {
		if it.prov == "off" {
			t.Errorf("disabled provider %q was offered", it.prov)
		}
	}
	if itemRefs(p)[0] != "alpha/a1" {
		t.Errorf("first item = %q, want alpha/a1", itemRefs(p)[0])
	}
}

// Opening the picker starts on the model already in use, so Enter without
// moving changes nothing.
func TestPickerStartsOnCurrentModel(t *testing.T) {
	p := newModelPicker(pickerConfig(), "alpha/a2")
	if p.sel != 1 {
		t.Errorf("sel = %d, want 1 (the model in use)", p.sel)
	}
	if got := p.selected().ref; got != "alpha/a2" {
		t.Errorf("selected = %q, want alpha/a2", got)
	}
}

// An unknown current model leaves the highlight at the top rather than
// pointing at nothing.
func TestPickerStartsAtTopWhenModelUnknown(t *testing.T) {
	p := newModelPicker(pickerConfig(), "nowhere/none")
	if p.sel != 0 {
		t.Errorf("sel = %d, want 0", p.sel)
	}
}

// Nothing configured means no picker, so /models can say so instead of opening
// an empty box.
func TestPickerNilWithoutModels(t *testing.T) {
	if p := newModelPicker(&config.Config{}, "x/y"); p != nil {
		t.Error("picker built for a config with no models")
	}
	if p := newModelPicker(nil, "x/y"); p != nil {
		t.Error("picker built for a nil config")
	}
}

// Walking clamps at both ends rather than wrapping or running off the list.
func TestPickerMoveClamps(t *testing.T) {
	p := newModelPicker(pickerConfig(), "alpha/a1")
	p.move(-10)
	if p.sel != 0 {
		t.Errorf("sel = %d moving up from the top, want 0", p.sel)
	}
	p.move(1000)
	if p.sel != len(p.items)-1 {
		t.Errorf("sel = %d moving down past the end, want %d", p.sel, len(p.items)-1)
	}
}

// PgUp/PgDn jump several rows at a time, and clamp at the ends.
func TestPickerPageKeysJump(t *testing.T) {
	cfg := &config.Config{Providers: []config.Provider{{
		Name: "many", Enabled: true,
		Models: []config.Model{
			{ID: "m1"}, {ID: "m2"}, {ID: "m3"}, {ID: "m4"},
			{ID: "m5"}, {ID: "m6"}, {ID: "m7"}, {ID: "m8"},
		},
	}}}
	p := newModelPicker(cfg, "many/m1")
	p.move(palettePage)
	if p.sel != palettePage {
		t.Errorf("sel = %d after jumping down, want %d", p.sel, palettePage)
	}
	p.move(-palettePage)
	if p.sel != 0 {
		t.Errorf("sel = %d after jumping up, want 0", p.sel)
	}
	// A jump past the end lands on the last entry, not beyond it.
	p.move(palettePage * 10)
	if p.sel != len(p.items)-1 {
		t.Errorf("sel = %d after jumping past the end, want %d", p.sel, len(p.items)-1)
	}
}

// Every row must fit the terminal, or the box below gets dragged apart.
func TestPickerRowsFitWidth(t *testing.T) {
	pal := theme.Get("nova")
	p := newModelPicker(pickerConfig(), "alpha/a1")
	for _, w := range []int{24, 40, 80, 120} {
		for _, budget := range []int{2, 3, 5, 20} {
			for i := range p.items {
				p.sel = i
				p.offset = 0
				for _, row := range p.rows(pal, w, budget) {
					if got := util.VisibleWidth(row); got > w {
						t.Errorf("width %d budget %d row %q: visible width %d", w, budget, util.Strip(row), got)
					}
				}
			}
		}
	}
}

// A short budget must still keep the highlight inside the visible window.
func TestPickerScrollsToKeepHighlightVisible(t *testing.T) {
	pal := theme.Get("nova")
	p := newModelPicker(pickerConfig(), "alpha/a1")
	rows := p.rows(pal, 80, 3) // header + two entries
	if len(rows) > 3 {
		t.Fatalf("rendered %d rows, budget was 3", len(rows))
	}
	p.sel = len(p.items) - 1
	rows = p.rows(pal, 80, 3)
	if len(rows) > 3 {
		t.Fatalf("rendered %d rows, budget was 3", len(rows))
	}
	if !strings.Contains(util.Strip(rows[len(rows)-1]), p.items[p.sel].id) {
		t.Errorf("last row = %q, want the highlighted model %q", util.Strip(rows[len(rows)-1]), p.items[p.sel].id)
	}
}

// The picker claims the navigation keys and nothing else.
func TestModelPickerKeyRouting(t *testing.T) {
	tui := &TUI{app: &App{history: NewBuffer(100)}}
	tui.openModelPicker(newModelPicker(pickerConfig(), "alpha/a1"))

	if !tui.modelPickerOpen() {
		t.Fatal("picker should be open")
	}
	if !tui.modelPickerKey("down") {
		t.Error("down was not consumed by the picker")
	}
	if tui.modelPick.sel != 1 {
		t.Errorf("sel = %d after down, want 1", tui.modelPick.sel)
	}
	if tui.modelPickerKey("backspace") {
		t.Error("the picker consumed backspace, which belongs to the composer")
	}
	if tui.modelPickerKey("a") {
		t.Error("the picker consumed a printable rune")
	}
}

// Esc closes the picker without touching the model.
func TestModelPickerEscClosesWithoutChange(t *testing.T) {
	tui := &TUI{app: &App{history: NewBuffer(100), SelectedModel: "alpha/a1"}}
	tui.openModelPicker(newModelPicker(pickerConfig(), "alpha/a1"))
	tui.modelPick.move(2)

	tui.handleEsc()

	if tui.modelPickerOpen() {
		t.Error("Esc did not close the picker")
	}
	if tui.app.SelectedModel != "alpha/a1" {
		t.Errorf("model changed to %q just from opening and backing out", tui.app.SelectedModel)
	}
}

// A command typed in full runs on Enter; a partial one completes first.
func TestPaletteEnterRunsExactCommandOnly(t *testing.T) {
	// "/help" is an exact command.
	tui := paletteTUI("/help")
	handled, run := tui.paletteKeyPress("enter")
	if !handled {
		t.Fatal("Enter was not consumed")
	}
	if run != "/help" {
		t.Errorf("run = %q, want /help to run directly", run)
	}

	// "/hel" is a prefix, so Enter completes rather than runs.
	tui = paletteTUI("/hel")
	handled, run = tui.paletteKeyPress("enter")
	if !handled {
		t.Fatal("Enter was not consumed for a prefix")
	}
	if run != "" {
		t.Errorf("run = %q, want the prefix completed instead of run", run)
	}
	if tui.inputLines[0] != "/help " {
		t.Errorf("composer = %q, want it completed to /help", tui.inputLines[0])
	}
}

// Tab only completes, even on an exact match.
func TestPaletteTabOnlyCompletes(t *testing.T) {
	tui := paletteTUI("/help")
	handled, run := tui.paletteKeyPress("tab")
	if !handled || run != "" {
		t.Errorf("tab = (handled %v, run %q), want it to complete without running", handled, run)
	}
}

func itemRefs(p *modelPicker) []string {
	out := make([]string, len(p.items))
	for i, it := range p.items {
		out[i] = it.ref
	}
	return out
}
