package tui

import (
	"github.com/nova-ai/nova/internal/config"
	"github.com/nova-ai/nova/internal/theme"
	"github.com/nova-ai/nova/internal/util"
)

// The model picker: `/models` opens a list of every configured model, walked
// with the arrow keys or PgUp/PgDn and applied with Enter. It has the same
// shape as the command palette, but it changes the session rather than the
// composer, so it is a mode of its own.

// modelItem is one selectable entry. ref is "provider/model", which is what
// ResolveModel takes.
type modelItem struct {
	ref   string
	id    string
	prov  string
	alias string
	ctx   int
}

// inUse reports whether this is the model already selected.
func (m modelItem) inUse(current string) bool { return m.ref == current }

type modelPicker struct {
	items   []modelItem
	sel     int
	offset  int
	current string
}

// newModelPicker lists every model of every enabled provider, in config
// order, and starts the highlight on the model in use.
func newModelPicker(cfg *config.Config, current string) *modelPicker {
	if cfg == nil {
		return nil
	}
	p := &modelPicker{current: current}
	for i := range cfg.Providers {
		prov := &cfg.Providers[i]
		if !prov.Enabled {
			continue
		}
		for _, m := range prov.Models {
			p.items = append(p.items, modelItem{
				ref:   prov.Name + "/" + m.ID,
				id:    m.ID,
				prov:  prov.Name,
				alias: m.Alias,
				ctx:   m.Context,
			})
		}
	}
	if len(p.items) == 0 {
		return nil
	}
	// Starting on the model already in use means Enter without moving changes
	// nothing, instead of silently jumping to the first entry.
	for i, it := range p.items {
		if it.inUse(current) {
			p.sel = i
			break
		}
	}
	return p
}

// move walks the highlight by delta, keeping it in range.
func (p *modelPicker) move(delta int) {
	if p == nil {
		return
	}
	p.sel += delta
	if p.sel < 0 {
		p.sel = 0
	}
	if p.sel >= len(p.items) {
		p.sel = len(p.items) - 1
	}
}

// selected is the highlighted model, or nil when there is none.
func (p *modelPicker) selected() *modelItem {
	if p == nil || p.sel < 0 || p.sel >= len(p.items) {
		return nil
	}
	return &p.items[p.sel]
}

// rows renders the picker, scrolling so the highlight stays visible.
func (p *modelPicker) rows(pal theme.Palette, w, budget int) []string {
	if p == nil || budget < 2 {
		return nil
	}
	// One row for the header; the scroll indicator shares it.
	visible := budget - 1
	if visible < 1 {
		visible = 1
	}
	if visible > len(p.items) {
		visible = len(p.items)
	}
	// Keep the highlight inside the window with as few scrolls as possible.
	if p.sel < p.offset {
		p.offset = p.sel
	}
	if p.sel >= p.offset+visible {
		p.offset = p.sel - visible + 1
	}
	if p.offset+visible > len(p.items) {
		p.offset = len(p.items) - visible
	}
	if p.offset < 0 {
		p.offset = 0
	}

	more := ""
	if p.offset > 0 {
		more = " ↑" + itoa(p.offset)
	}
	if p.offset+visible < len(p.items) {
		more = " ↓" + itoa(len(p.items)-p.offset-visible) + more
	}
	// The header carries the scroll indicator, so it is truncated like any
	// other row: an over-wide row wraps, and the wrap drags the box below it
	// out of alignment.
	head := util.Truncate(" models — ↑↓ move · PgUp/PgDn jump · ⏎ use · esc close", w-util.VisibleWidth(more))
	out := []string{pal.Style("dim", head) + pal.Style("dim", more)}

	// Fixed columns for the provider and the model id, so the aliases and
	// context windows line up down the list.
	provWidth, idWidth := 0, 0
	for _, it := range p.items {
		if n := util.VisibleWidth(it.prov); n > provWidth {
			provWidth = n
		}
		if n := util.VisibleWidth(it.id); n > idWidth {
			idWidth = n
		}
	}
	prevProvider := ""
	if p.offset > 0 {
		prevProvider = p.items[p.offset-1].prov
	}
	for i := p.offset; i < p.offset+visible; i++ {
		it := p.items[i]

		prov := pal.Style("dim", pad("", provWidth+1))
		if it.prov != prevProvider {
			// Name the group once, on its first entry.
			prov = pal.Style("accent", pad(it.prov, provWidth)) + " "
			prevProvider = it.prov
		}

		marker := "  "
		idStyle := pal.Style("accent2", pad(it.id, idWidth))
		if it.inUse(p.current) {
			// A dot marks the model already in use, so a stray Enter changes
			// nothing.
			marker = pal.Style("success", "● ")
			idStyle = pal.Style("success", pad(it.id, idWidth))
		}
		if i == p.sel {
			marker = pal.Style("accent", "❯ ")
			idStyle = pal.Style("text", pad(it.id, idWidth))
		}
		line := marker + prov + idStyle
		if it.alias != "" {
			line += pal.Style("dim", " ("+it.alias+")")
		}
		if it.ctx > 0 {
			line += pal.Style("dim", "  ctx "+humanTokens(it.ctx))
		}
		out = append(out, util.Truncate(line, w))
	}
	return out
}
