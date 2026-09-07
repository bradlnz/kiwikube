package main

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

type treeRow struct{ namespace, resource, label string }
type viewer struct {
	cancel                     context.CancelFunc
	workerDone                 chan struct{}
	details                    []string
	detailStatus               string
	stats                      *nodeStats
	archiveFilter              archiveFilter
	entries                    []archiveEntry
	values                     []displayValue
	pages                      []int64
	selected                   int
	entryID                    int64
	parent                     *viewer
	title, kind, query, status string
	resource                   string
	background                 []string
	backgroundCols             int
	target                     record
	container                  int
	lines                      []string
	top, left                  int
	follow, previous, done     bool
	generation                 int
}

func (v *viewer) isWorkspaceView() bool {
	return v != nil && (v.kind == "logs" || v.kind == "flow" || v.kind == "stats" || v.kind == "describe")
}

type app struct {
	shell                                                     *interactiveTerminal
	shellTitle                                                string
	showPodMetrics                                            bool
	podMetrics                                                *podMetrics
	podMetricsStatus                                          string
	namespaceColors                                           map[string]string
	snapshot                                                  snapshot
	context, status, namespace, resource, query               string
	expanded                                                  map[string]bool
	treeSelected, treeTop, tableSelected, tableTop, container int
	treeFocus, searching, loading, paused                     bool
	rows, cols                                                int
	config                                                    config
	configFile                                                string
	visible                                                   []record
	view                                                      *viewer
	outputs                                                   []*viewer
	lastOutput, closeTab                                      *viewer
	flow                                                      *viewer
	action                                                    string
	previousFrame                                             []string
	archivePath, archiveStatus                                string
	switching                                                 string
	spinner                                                   int
	eventWindow                                               int
	eventType                                                 string
	menu                                                      *popupMenu
}

var eventWindows = []struct {
	label    string
	duration time.Duration
}{{"All", 0}, {"5m", 5 * time.Minute}, {"15m", 15 * time.Minute}, {"1h", time.Hour}, {"6h", 6 * time.Hour}, {"24h", 24 * time.Hour}}

func newApp() *app {
	return &app{context: "…", status: "Loading cluster…", resource: "pods", expanded: map[string]bool{"": true}, treeFocus: true, treeSelected: 2, rows: 24, cols: 80, config: defaultConfig()}
}
func (a *app) applySnapshot(value snapshot) {
	id := ""
	if r, ok := a.selected(); ok {
		id = r.id()
	}
	tree := a.tree()
	var selectedTree treeRow
	if a.treeSelected < len(tree) {
		selectedTree = tree[a.treeSelected]
	}
	for kind := range value.Errors {
		if kind == "namespaces" {
			value.Namespaces = a.snapshot.Namespaces
		} else {
			value.Resources[kind] = a.snapshot.Resources[kind]
		}
	}
	namespaces := map[string]bool{}
	for _, ns := range value.Namespaces {
		namespaces[ns] = true
	}
	for _, rows := range value.Resources {
		for _, row := range rows {
			if row.Namespace != "" {
				namespaces[row.Namespace] = true
			}
		}
	}
	value.Namespaces = nil
	for ns := range namespaces {
		value.Namespaces = append(value.Namespaces, ns)
	}
	sort.Strings(value.Namespaces)
	for _, namespace := range value.Namespaces {
		a.namespaceColor(namespace)
	}
	a.snapshot, a.loading = value, false
	a.rebuild()
	for i, row := range a.visible {
		if row.id() == id {
			a.tableSelected = i
			break
		}
	}
	for i, row := range a.tree() {
		if row.namespace == selectedTree.namespace && row.resource == selectedTree.resource {
			a.treeSelected = i
			break
		}
	}
	a.clampSelection()
}
func (a *app) tree() []treeRow {
	rows := []treeRow{{resource: "nodes", label: "◈ Nodes"}}
	namespaces := append([]string{""}, a.snapshot.Namespaces...)
	for _, ns := range namespaces {
		marker := "▸"
		if a.expanded[ns] {
			marker = "▾"
		}
		rows = append(rows, treeRow{namespace: ns, label: marker + " " + first(ns, "All namespaces")})
		if a.expanded[ns] {
			for _, kind := range resourceTypes {
				if kind.key == "nodes" {
					continue
				}
				rows = append(rows, treeRow{namespace: ns, resource: kind.key, label: "  " + kind.title})
			}
		}
	}
	return rows
}
func (a *app) chooseTree() {
	rows := a.tree()
	if len(rows) == 0 {
		return
	}
	a.treeSelected = clamp(a.treeSelected, 0, len(rows)-1)
	row := rows[a.treeSelected]
	if row.resource == "" {
		a.expanded[row.namespace] = !a.expanded[row.namespace]
		return
	}
	a.namespace, a.resource, a.tableSelected, a.tableTop, a.container, a.treeFocus = row.namespace, row.resource, 0, 0, 0, false
	a.rebuild()
}
func (a *app) rebuild() {
	a.visible = a.visible[:0]
	query := strings.ToLower(a.query)
	cutoff := time.Now().Add(-eventWindows[a.eventWindow].duration)
	for _, r := range a.snapshot.Resources[a.resource] {
		if a.resource == "events" && (a.eventWindow != 0 && r.Seen.Before(cutoff) || a.eventType != "" && (len(r.Cells) < 2 || r.Cells[1] != a.eventType)) {
			continue
		}
		if (a.namespace == "" || r.Namespace == a.namespace || a.resource == "nodes") && strings.Contains(r.Search, query) {
			a.visible = append(a.visible, r)
		}
	}
	a.clampSelection()
}
func (a *app) selected() (record, bool) {
	if a.tableSelected < 0 || a.tableSelected >= len(a.visible) {
		return record{}, false
	}
	return a.visible[a.tableSelected], true
}
func (a *app) tableCount() int { return len(a.visible) }
func (a *app) clampSelection() {
	a.treeSelected = clamp(a.treeSelected, 0, max(0, len(a.tree())-1))
	a.tableSelected = clamp(a.tableSelected, 0, max(0, len(a.visible)-1))
}
func (a *app) move(delta int) {
	if a.treeFocus {
		a.treeSelected = clamp(a.treeSelected+delta, 0, max(0, len(a.tree())-1))
	} else {
		a.tableSelected = clamp(a.tableSelected+delta, 0, max(0, len(a.visible)-1))
		a.container = 0
	}
}
func (a *app) handle(k key) (quit, refresh bool) {
	if a.shell != nil {
		left, top, w, h := a.shellBounds()
		if k.r == 29 || (k.code == keyMouse && !k.release && k.scroll == 0 && ((k.mouseY == top+1 && k.mouseX >= left+w-4 && k.mouseX < left+w) || (k.mouseY == top+h-1 && k.mouseX > left && k.mouseX <= left+w))) {
			a.action = "close-shell"
			return
		}
		var err error
		if k.code == keyMouse {
			if k.mouseX > left+1 && k.mouseX < left+w && k.mouseY > top+1 && k.mouseY < top+h-1 {
				err = a.shell.mouse(k)
			}
		} else {
			err = a.shell.handle(k)
		}
		if err != nil {
			a.status = err.Error()
		}
		return
	}
	if k.release {
		return
	}

	if a.switching != "" {
		return k.r == 3 || k.r == 'q', false
	}
	switch k.code {
	case keyQuit:
		return true, false
	case keyRefresh:
		return false, true
	case keySave:
		a.action = "save"
		return
	case keyHelp:
		a.action = "help"
		return
	case keySidebar:
		a.config.HideSidebar = !a.config.HideSidebar
		if a.config.HideSidebar {
			a.treeFocus = false
		}
		return
	}
	if k.alt {
		if k.r >= '1' && k.r <= '5' {
			a.pickCluster(int(k.r - '1'))
		}
		return
	}
	if k.code == keyMouse {
		return a.handleMouse(k)
	}
	if k.r == 6 { // Ctrl+F
		a.menu = nil
		a.searching = true
		return
	}
	if k.r == 3 {
		if a.view != nil {
			a.action = "close"
			return
		}
		return true, false
	}
	if a.menu != nil {
		return a.handleMenu(k)
	}
	if a.searching {
		query := &a.query
		if a.view != nil {
			query = &a.view.query
		}
		switch k.code {
		case keyEscape:
			a.searching = false
			*query = ""
		case keyEnter:
			a.searching = false
		case keyBackspace:
			runes := []rune(*query)
			if len(runes) > 0 {
				*query = string(runes[:len(runes)-1])
			}
		default:
			if k.r >= 32 {
				*query += string(k.r)
			}
		}
		if a.view == nil {
			a.tableSelected, a.tableTop = 0, 0
			a.rebuild()
		} else {
			a.view.top = 0
			if a.view.kind == "archive" && !a.searching {
				a.view.archiveFilter.Search = a.view.query
				a.view.archiveFilter.Before = 0
				a.view.pages = nil
				a.action = "reload"
			}
		}
		return
	}
	if k.code == keyResources || k.code == keyLogs || k.code == keyFlow || k.r == 'F' {
		switch k.code {
		case keyResources:
			a.view = nil
		case keyLogs:
			for i := len(a.outputs) - 1; i >= 0; i-- {
				if a.outputs[i].kind == "logs" {
					a.view, a.treeFocus = a.outputs[i], false
					break
				}
			}
		default:
			if a.flow == nil {
				a.flow = &viewer{kind: "flow", title: "Ingress → Service → Pods", done: true}
			}
			a.view, a.treeFocus = a.flow, false
		}
		return
	}
	if (k.r == 14 || k.r == 16) && (a.view == nil || a.view.isWorkspaceView()) {
		a.cycleTab(map[bool]int{true: -1, false: 1}[k.r == 16])
		return
	}
	if k.code == keyTab && len(a.outputs) > 0 && (a.view == nil || a.view.isWorkspaceView() && a.view.kind != "flow") {
		if a.view == nil {
			if !slices.Contains(a.outputs, a.lastOutput) {
				a.lastOutput = a.outputs[len(a.outputs)-1]
			}
			a.view, a.treeFocus = a.lastOutput, false
		} else {
			a.lastOutput, a.view = a.view, nil
		}
		return
	}
	if a.view != nil {
		v := a.view
		if v.kind == "values" && len(v.values) > 0 {
			switch {
			case k.r == 'c':
				a.action = "copy-value"
				return
			case k.code == keyTab:
				v.selected = (v.selected + 1) % len(v.values)
				v.top, v.left, v.query = v.values[v.selected].start, 0, ""
				v.follow = false
				return
			}
		}
		if v.kind == "archive" {
			switch {
			case k.code == keyEnter:
				a.action = "archive-detail"
				return
			case k.r == 'K':
				kinds := []string{"", "logs", "events", "deployments", "sessions", "collector"}
				for i, kind := range kinds {
					if v.archiveFilter.Kind == kind {
						v.archiveFilter.Kind = kinds[(i+1)%len(kinds)]
						break
					}
				}
				v.archiveFilter.Before = 0
				v.pages = nil
				a.action = "reload"
				return
			case k.r == 'n':
				if len(v.entries) == archivePageSize {
					v.pages = append(v.pages, v.archiveFilter.Before)
					v.archiveFilter.Before = v.entries[len(v.entries)-1].ID
					a.action = "reload"
				}
				return
			case k.r == 'N':
				if len(v.pages) > 0 {
					v.archiveFilter.Before = v.pages[len(v.pages)-1]
					v.pages = v.pages[:len(v.pages)-1]
					a.action = "reload"
				}
				return
			case k.r == 'j' || k.code == keyDown:
				v.selected = min(v.selected+1, max(0, len(v.entries)-1))
				return
			case k.r == 'k' || k.code == keyUp:
				v.selected = max(0, v.selected-1)
				return
			case k.code == keyPageDown:
				v.selected = min(v.selected+max(1, a.rows-5), max(0, len(v.entries)-1))
				return
			case k.code == keyPageUp:
				v.selected = max(0, v.selected-max(1, a.rows-5))
				return
			case k.r == 'g':
				v.selected = 0
				return
			case k.r == 'G':
				v.selected = max(0, len(v.entries)-1)
				return
			case k.r == 'r':
				v.archiveFilter.Before = 0
				v.pages = nil
				a.action = "reload"
				return
			}
		}
		switch {
		case v.kind == "flow" && k.r == 'r':
			return false, true
		case v.kind == "flow" && (k.code == keyEscape || k.r == 'q' || k.code == keyTab):
			a.view = nil
		case k.code == keyEscape || k.r == 'q':
			a.action = "close"
		case k.r == 'e' && (v.resource == "secrets" || v.resource == "configmaps"):
			a.action = "edit"
		case k.r == '/':
			a.searching = true
		case k.r == 'j' || k.code == keyDown:
			v.follow = false
			v.top++
		case k.r == 'k' || k.code == keyUp:
			v.follow = false
			v.top = max(0, v.top-1)
		case k.code == keyPageDown:
			v.follow = false
			v.top += max(1, a.rows-6)
		case k.code == keyPageUp:
			v.follow = false
			v.top = max(0, v.top-max(1, a.rows-6))
		case k.code == keyRight || k.r == 'l':
			v.left += 8
		case k.code == keyLeft || k.r == 'h':
			v.left = max(0, v.left-8)
		case k.r == 'g':
			v.follow = false
			v.top = 0
		case k.r == 'G' || k.r == 'f':
			v.follow = true
		case k.r == 'c' && v.kind == "logs" && v.resource != "jobs":
			v.container++
			a.action = "reload"
		case k.r == 'p' && v.kind == "logs" && v.resource != "jobs":
			v.previous = !v.previous
			a.action = "reload"
		case k.r == 'r' && v.kind != "help":
			a.action = "reload"
		}
		return
	}
	if a.resource == "events" && (k.r >= '1' && k.r <= '6' || k.r == 'T' || k.r == 'K' || k.r == 'X' || k.code == keyEventType) {
		switch {
		case k.r >= '1' && k.r <= '6':
			a.eventWindow = int(k.r - '1')
		case k.r == 'T':
			var items []footerAction
			for i, window := range eventWindows {
				items = append(items, footerAction{window.label, key{r: '1' + rune(i)}})
			}
			a.openMenu(2, a.rows-len(items)-2, a.eventWindow, items)
			return
		case k.r == 'K':
			items := []footerAction{{"All types", key{code: keyEventType, r: 0}}, {"Warning", key{code: keyEventType, r: 1}}, {"Normal", key{code: keyEventType, r: 2}}}
			selected := 0
			for i, item := range items {
				if item.label == a.eventType {
					selected = i
				}
			}
			a.openMenu(4+width(a.eventFilterActions()[0].label), a.rows-len(items)-2, selected, items)
			return
		case k.code == keyEventType && k.r >= 0 && k.r <= 2:
			a.eventType = []string{"", "Warning", "Normal"}[k.r]
		case k.r == 'X':
			a.eventWindow, a.eventType, a.query = 0, "", ""
		}
		a.tableSelected, a.tableTop = 0, 0
		a.rebuild()
		return
	}
	switch {
	case k.r == 'm' && a.resource == "pods":
		a.showPodMetrics = !a.showPodMetrics
		if a.showPodMetrics && a.podPanelWidth() == 0 {
			a.status = "Widen the window or hide the sidebar to show pod metrics"
		}
	case k.r == 'q':
		return true, false
	case k.r == 'r':
		return false, true
	case k.r == ' ':
		a.paused = !a.paused
		a.status = ""
		return false, !a.paused
	case k.r == '/':
		a.searching = true
	case k.r == 'A':
		a.action = "archive"
	case k.r == 'v':
		a.action = "resource-archive"
	case k.r == '?':
		a.action = "help"
	case k.r == 't':
		themes := []string{"kiwi", "ocean", "amber"}
		for i, theme := range themes {
			if a.config.Theme == theme {
				a.config.Theme = themes[(i+1)%len(themes)]
				break
			}
		}
		a.status = "Theme: " + a.config.Theme + " · w to save"
	case k.r == 'b':
		a.config.HideSidebar = !a.config.HideSidebar
		if a.config.HideSidebar {
			a.treeFocus = false
		}
	case k.r == '[':
		a.config.SidebarWidth = max(18, a.config.SidebarWidth-2)
	case k.r == ']':
		a.config.SidebarWidth = min(50, a.config.SidebarWidth+2)
	case k.r == '+' || k.r == '=':
		a.config.RefreshSeconds = min(300, a.config.RefreshSeconds+1)
	case k.r == '-':
		a.config.RefreshSeconds = max(2, a.config.RefreshSeconds-1)
	case k.r == 'w':
		a.action = "save"
	case k.r == 'a':
		a.namespace = ""
		a.rebuild()
	case k.r == 'c':
		a.container++
	case k.code == keyTab:
		a.treeFocus = !a.treeFocus
		a.config.HideSidebar = false
	case k.code == keyUp || k.r == 'k':
		a.move(-1)
	case k.code == keyDown || k.r == 'j':
		a.move(1)
	case k.code == keyPageUp:
		a.move(-max(1, a.rows-6))
	case k.code == keyPageDown:
		a.move(max(1, a.rows-6))
	case k.code == keyLeft || k.r == 'h':
		a.treeFocus = true
		a.config.HideSidebar = false
	case k.code == keyRight || k.r == 'l':
		if a.treeFocus {
			a.chooseTree()
		}
	case k.code == keyEnter:
		if a.treeFocus {
			a.chooseTree()
		} else if a.resource == "pods" || a.resource == "jobs" {
			a.action = "logs"
		} else if a.resource == "nodes" {
			a.action = "stats"
		} else if a.resource == "secrets" || a.resource == "configmaps" {
			a.action = "values"
		} else {
			a.action = "describe"
		}
	case k.r == 'd':
		a.action = "describe"
	case k.r == 'e' && (a.resource == "secrets" || a.resource == "configmaps"):
		a.action = "edit"
	case k.r == 'y':
		a.action = "yaml"
	case k.r == 'L':
		a.action = "logs"
	case k.r == 'H':
		a.action = "history"
	case k.r == 's':
		a.action = "shell"
	case k.r == 'S':
		a.action = "ssh"
	case k.code == keyEscape:
		a.query = ""
		a.tableSelected, a.tableTop = 0, 0
		a.rebuild()
	}
	return
}
func age(created, timeNow time.Time) string {
	if created.IsZero() {
		return "-"
	}
	d := timeNow.Sub(created)
	if d < time.Minute {
		return fmt.Sprintf("%ds", max(0, int(d.Seconds())))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 48*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
func clamp(value, low, high int) int { return min(max(value, low), high) }

func (a *app) pickCluster(slot int) {
	if slot >= 0 && slot < len(a.config.ClusterSlots) {
		if cluster := a.config.ClusterSlots[slot]; cluster != "" && cluster != a.context {
			a.action = "cluster:" + cluster
		}
	}
}

// Columns are one-based, as in SGR terminal mouse reports.
func (a *app) clusterButtons() (start, size int) {
	size = 3
	return a.cols - 5*size + 1, size
}

func (a *app) handleMouse(k key) (quit, refresh bool) {
	if a.rows < 10 || a.cols < 40 || k.mouseX < 1 || k.mouseX > a.cols || k.mouseY < 1 || k.mouseY > a.rows {
		return
	}
	if k.mouseY == 1 {
		a.menu = nil
		start, size := a.clusterButtons()
		if k.scroll == 0 && k.mouseX >= start {
			a.pickCluster((k.mouseX - start) / size)
		} else if k.scroll == 0 && !a.searching {
			a.topMenuAt(k.mouseX)
		}
		return
	}
	if a.searching {
		left, top, w := a.searchBounds()
		if k.scroll == 0 && k.mouseY == top+4 {
			if k.mouseX >= left+3 && k.mouseX < left+14 {
				return a.handle(key{code: keyEnter})
			}
			if k.mouseX >= left+16 && k.mouseX < min(left+25, left+w) {
				return a.handle(key{code: keyEscape})
			}
		}
		return
	}
	if m := a.menu; m != nil {
		if k.scroll == 0 {
			a.menu = nil
			index := k.mouseY - m.y - 1
			if k.mouseX > m.x && k.mouseX < m.x+m.width-1 && index >= 0 && index < len(m.items) {
				return a.handle(m.items[index].key)
			}
		}
		return
	}
	if a.view != nil && a.view.kind == "values" {
		left, top, w, h := a.valuesBounds()
		if k.mouseX <= left || k.mouseX > left+w {
			return
		}
		if k.scroll != 0 && k.mouseY >= top+3 && k.mouseY < top+h-1 {
			code := keyDown
			if k.scroll < 0 {
				code = keyUp
			}
			return a.handle(key{code: code})
		}
		if k.scroll == 0 && k.mouseY == top+2 {
			return a.handle(key{r: 6})
		}
		if k.scroll == 0 && k.mouseY >= top+3 && k.mouseY < top+h-1 {
			v := a.view
			index := v.top + k.mouseY - top - 3
			for lineIndex, line := range v.lines {
				if v.query != "" && !strings.Contains(strings.ToLower(line), strings.ToLower(v.query)) {
					continue
				}
				if index == 0 {
					for i, value := range v.values {
						if lineIndex >= value.start && lineIndex < value.end {
							v.selected = i
						}
					}
					break
				}
				index--
			}
			return
		}
		if k.scroll == 0 && k.mouseY == top+h-1 {
			if a.searching {
				a.handle(key{code: keyEnter})
			}
			return a.clickFooter(k.mouseX, left+3, left+w-1)
		}
		return
	}
	if k.mouseY == 2 && (a.view == nil || a.view.isWorkspaceView()) {
		if k.scroll != 0 {
			a.cycleTab(k.scroll)
			return
		}
		for _, tab := range a.workspaceTabs() {
			if k.mouseX < tab.x || k.mouseX >= tab.x+tab.w {
				continue
			}
			if tab.closeX > 0 && k.mouseX >= tab.closeX {
				if tab.view != a.flow {
					a.closeTab, a.action = tab.view, "close-output"
				} else {
					if a.view == a.flow {
						a.view = nil
					}
					a.flow = nil
				}
			} else {
				a.view, a.treeFocus = tab.view, false
			}
			break
		}
		return
	}
	if w := a.podPanelWidth(); w > 0 && k.mouseX > a.cols-w-1 {
		if k.mouseY == 3 && k.scroll == 0 && k.mouseX >= a.cols-3 {
			a.showPodMetrics = false
		}
		if k.mouseY >= 3 && k.mouseY <= a.rows-3 {
			return
		}
	}
	if k.mouseY == 3 && k.scroll == 0 && (a.view == nil || a.view.isWorkspaceView()) && k.mouseX > a.sidebarWidth()+1 {
		a.handle(key{r: 6})
		return
	}
	if a.searching {
		a.handle(key{code: keyEnter})
		if a.action == "reload" {
			return
		}
	}
	logSidebar := a.view.isWorkspaceView() && a.sidebarWidth() > 0 && k.mouseX <= a.sidebarWidth()+1
	if a.view != nil && !logSidebar {
		v := a.view
		firstRow, lastRow := 4, a.rows-2
		if v.isWorkspaceView() {
			firstRow, lastRow = 4, a.rows-3
		}
		if k.mouseY >= firstRow && k.mouseY <= lastRow {
			if k.scroll != 0 {
				code := keyDown
				if k.scroll < 0 {
					code = keyUp
				}
				a.handle(key{code: code})
			} else if index := v.top + k.mouseY - 4; v.kind == "archive" && index < len(v.entries) {
				if index == v.selected {
					a.handle(key{code: keyEnter})
				}
				v.selected = index
			}
		}
		if k.mouseY != a.rows {
			return
		}
	}
	if k.mouseY == a.rows && k.scroll == 0 {
		return a.clickFooter(k.mouseX, 2, a.cols)
	}
	if k.mouseY < 4 || k.mouseY > a.rows-3 {
		return
	}
	side := a.sidebarWidth()
	if side > 0 && k.mouseX == side+1 {
		return
	}
	tree := side > 0 && k.mouseX <= side || side == 0 && a.treeFocus
	if k.scroll != 0 {
		a.treeFocus = tree
		a.move(k.scroll)
	} else if tree {
		if index := a.treeTop + k.mouseY - 4; index < len(a.tree()) {
			if logSidebar {
				a.view = nil
			}
			a.treeSelected, a.treeFocus = index, true
			a.chooseTree()
		}
	} else if index := a.tableTop + k.mouseY - 4; index < len(a.visible) {
		if !a.treeFocus && index == a.tableSelected {
			a.handle(key{code: keyEnter})
		} else {
			a.container = 0
		}
		a.tableSelected, a.treeFocus = index, false
	}
	return
}

func (a *app) clickFooter(column, start, limit int) (quit, refresh bool) {
	return a.clickActions(column, start, limit, a.footerActions())
}

func (a *app) clickActions(column, start, limit int, actions []footerAction) (quit, refresh bool) {
	for _, action := range actions {
		end := start + width(action.label)
		if column >= start && column < end && column < limit {
			return a.handle(action.key)
		}
		start = end + 2
	}
	return
}

func (v *viewer) stop() {
	if v != nil && v.cancel != nil {
		v.cancel()
		<-v.workerDone
		v.cancel = nil
	}
}

func (a *app) closeViewer(v *viewer) {
	if v == nil {
		return
	}
	v.stop()
	if i := slices.Index(a.outputs, v); i >= 0 {
		a.outputs = slices.Delete(a.outputs, i, i+1)
	}
	if a.view == v {
		a.view = v.parent
	}
	if a.lastOutput == v {
		a.lastOutput = nil
	}
	a.searching = false
}

func (a *app) cycleTab(delta int) {
	views := append([]*viewer{nil}, a.outputs...)
	if a.flow != nil {
		views = append(views, a.flow)
	}
	i := slices.Index(views, a.view)
	a.view, a.treeFocus = views[(max(0, i)+delta+len(views))%len(views)], false
}
