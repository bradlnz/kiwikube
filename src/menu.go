package main

import (
	"fmt"
	"os"
	"strings"
)

type popupMenu struct {
	x, y, width, selected int // One-based terminal coordinates.
	items                 []footerAction
}

type topMenu struct {
	label string
	items []footerAction
}

func (a *app) topMenus() []topMenu {
	edit := []footerAction{{"Search  Ctrl+F", key{r: 6}}}
	resource := a.resource
	if a.view != nil {
		resource = a.view.resource
	}
	if resource == "secrets" || resource == "configmaps" {
		edit = append(edit, footerAction{"Edit values  e", key{r: 'e'}})
		if a.view != nil && a.view.kind == "values" {
			edit = append(edit, footerAction{"Copy selected value  c", key{r: 'c'}})
		}
	}
	return []topMenu{
		{"File", []footerAction{{"Refresh", key{code: keyRefresh}}, {"Save settings", key{code: keySave}}, {"Quit", key{code: keyQuit}}}},
		{"Edit", edit},
		{"View", []footerAction{{"Resources", key{code: keyResources}}, {"Logs", key{code: keyLogs}}, {"Flow graph", key{code: keyFlow}}, {"Toggle sidebar", key{code: keySidebar}}}},
		{"Help", []footerAction{{"Keyboard shortcuts", key{code: keyHelp}}}},
	}
}

func (a *app) openMenu(x, y, selected int, items []footerAction) {
	w := 0
	for _, item := range items {
		w = max(w, width(item.label)+4)
	}
	w = min(w, a.cols)
	a.menu = &popupMenu{x: clamp(x, 1, a.cols-w+1), y: clamp(y, 2, max(2, a.rows-len(items)-1)), width: w, selected: selected, items: items}
}

func (a *app) topMenuAt(column int) bool {
	x := 1
	for _, menu := range a.topMenus() {
		end := x + width(menu.label) + 2
		if column >= x && column < end {
			a.openMenu(x, 2, 0, menu.items)
			return true
		}
		x = end
	}
	return false
}

func (a *app) handleMenu(k key) (quit, refresh bool) {
	m := a.menu
	switch {
	case k.code == keyUp || k.r == 'k':
		m.selected = max(0, m.selected-1)
	case k.code == keyDown || k.r == 'j':
		m.selected = min(len(m.items)-1, m.selected+1)
	case k.code == keyEnter:
		a.menu = nil
		return a.handle(m.items[m.selected].key)
	case k.code == keyEscape || k.r == 'q':
		a.menu = nil
	}
	return
}

func (a *app) drawMenu() {
	m, p := a.menu, colors(a.config.Theme)
	lines := []string{p.header + "╭" + strings.Repeat("─", m.width-2) + "╮"}
	for i, item := range m.items {
		style := p.header
		if i == m.selected {
			style = p.selected
		}
		lines = append(lines, p.header+"│"+style+fit(" "+item.label, m.width-2)+p.header+"│")
	}
	lines = append(lines, p.header+"╰"+strings.Repeat("─", m.width-2)+"╯")
	for i, line := range lines {
		fmt.Fprintf(os.Stdout, "\x1b[%d;%dH%s%s", m.y+i, m.x, line, reset)
	}
	// Redraw the covered cells when the menu closes or moves.
	a.previousFrame = nil
}

func (a *app) searchBounds() (left, top, w int) {
	w = min(a.cols-4, 72)
	return (a.cols - w) / 2, (a.rows - 5) / 2, w
}

func (a *app) searchFrame(frame []string) []string {
	if a.rows < 10 || a.cols < 40 {
		return frame
	}
	p := colors(a.config.Theme)
	query, target := a.query, a.resource
	if a.view != nil {
		query, target = a.view.query, a.view.kind
	}
	left, top, w := a.searchBounds()
	input := "/" + query
	input = dropCells(input, max(0, width(input)-(w-5))) + "▏"
	box := []string{
		"╭─ " + fit("Search · "+target, w-5) + "─╮",
		"│" + fit("", w-2) + "│",
		"│" + fit(" "+input, w-2) + "│",
		"│" + fit(" Enter apply  Esc clear", w-2) + "│",
		"╰" + strings.Repeat("─", w-2) + "╯",
	}
	for i, line := range box {
		frame[top+i] = p.base + strings.Repeat(" ", left) + p.header + line + p.base + strings.Repeat(" ", a.cols-left-w) + reset
	}
	return frame
}
