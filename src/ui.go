package main

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const reset = "\x1b[0m"

type terminal struct {
	state      string
	stop, done chan struct{}
}

func enterTerminal() { fmt.Print("\x1b[?1049h\x1b[?25l\x1b[?7l\x1b[?1000h\x1b[?1006h\x1b[2J") }
func leaveTerminal() { fmt.Print(reset + "\x1b[?1006l\x1b[?1000l\x1b[?7h\x1b[?25h\x1b[?1049l") }

func (t *terminal) stopSwitching() {
	if t.stop != nil {
		close(t.stop)
		<-t.done
		t.stop = nil
	}
}
func (t *terminal) close() {
	t.stopSwitching()
	if t.state != "" {
		leaveTerminal()
		stty(t.state)
	}
}
func (t *terminal) startSwitching(a *app, cluster string) {
	t.stopSwitching()
	frame, cols, theme := a.frame(), a.cols, a.config.Theme
	renderer := &app{}
	renderer.drawFrame(switchingFrame(frame, cols, theme, cluster, 0))
	t.stop, t.done = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(t.done)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for step := 1; ; step++ {
			select {
			case <-t.stop:
				return
			case <-ticker.C:
				renderer.drawFrame(switchingFrame(frame, cols, theme, cluster, step))
			}
		}
	}()
}

type palette struct{ base, top, header, selected, muted, accent, warn, bad string }

func colors(theme string) palette {
	accent := "150"
	selected := "22"
	if theme == "ocean" {
		accent = "117"
		selected = "24"
	}
	if theme == "amber" {
		accent = "222"
		selected = "58"
	}
	return palette{
		base: "\x1b[48;5;234;38;5;252m", top: "\x1b[48;5;235;38;5;" + accent + ";1m",
		header: "\x1b[48;5;236;38;5;" + accent + ";1m", selected: "\x1b[48;5;" + selected + ";38;5;255;1m",
		muted: "\x1b[48;5;234;38;5;245m", accent: "\x1b[48;5;234;38;5;" + accent + "m",
		warn: "\x1b[48;5;234;38;5;222m", bad: "\x1b[48;5;234;38;5;210m",
	}
}
func terminalSize() (int, int) {
	out, err := stty("size")
	if err != nil {
		return 24, 80
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return 24, 80
	}
	rows, _ := strconv.Atoi(fields[0])
	cols, _ := strconv.Atoi(fields[1])
	return max(rows, 1), max(cols, 1)
}
func (a *app) draw() {
	if a.shell != nil {
		// Keep the covered dashboard frozen. Resize and close invalidate it.
		if a.previousFrame == nil {
			a.drawFrame(a.frame())
		}
		a.drawShell()
		return
	}
	fmt.Print("\x1b[?25l")
	frame := a.frame()
	if a.switching != "" {
		frame = switchingFrame(frame, a.cols, a.config.Theme, a.switching, a.spinner)
	} else if a.searching {
		frame = a.searchFrame(frame)
	}
	a.drawFrame(frame)
	if a.menu != nil && !a.searching && a.switching == "" {
		a.drawMenu()
	}
}
func (a *app) drawFrame(frame []string) {
	var out strings.Builder
	for i, line := range frame {
		if i < len(a.previousFrame) && a.previousFrame[i] == line {
			continue
		}
		fmt.Fprintf(&out, "\x1b[%d;1H%s%s\x1b[K", i+1, line, reset)
	}
	fmt.Fprint(os.Stdout, out.String())
	a.previousFrame = frame
}
func (a *app) frame() []string {
	p := colors(a.config.Theme)
	line := func(text, style string) string { return style + fit(text, a.cols) + reset }
	if a.rows < 10 || a.cols < 40 {
		frame := make([]string, a.rows)
		for i := range frame {
			frame[i] = line("", p.base)
		}
		frame[0] = line(" KiwiKube · resize to at least 40×10", p.accent)
		return frame
	}
	state := fmt.Sprintf("● LIVE · %ds", a.config.RefreshSeconds)
	if a.loading {
		state = "◌ SYNCING"
	}
	if a.paused {
		state = "Ⅱ PAUSED"
	}
	if len(a.snapshot.Errors) > 0 {
		if len(a.snapshot.Errors) == len(resourceTypes)+1 {
			state = "○ OFFLINE"
		} else {
			state += " · PARTIAL"
		}
	}
	title := " KiwiKube / " + first(a.context, "default context")
	start, size := a.clusterButtons()
	header := ""
	menuWidth := 0
	for _, menu := range a.topMenus() {
		label := " " + menu.label + " "
		header += p.header + label
		menuWidth += width(label)
	}
	header += p.top + fit(title, max(0, start-1-menuWidth))
	for i, name := range a.config.ClusterSlots {
		style := p.header
		if name == a.context {
			style = p.selected
		}
		if name == "" {
			style = p.muted
		}
		label := fmt.Sprintf(" %d ", i+1)
		header += style + fit(label, size)
	}
	frame := []string{header + reset}
	if a.view != nil && !a.view.isWorkspaceView() {
		return a.viewerFrame(frame, p)
	}
	total := func(kind string) int { return len(a.snapshot.Resources[kind]) }
	summary := fmt.Sprintf("%d pods · %d deployments · %d services · %d nodes · %d events", total("pods"), total("deployments"), total("services"), total("nodes"), total("events"))
	frame = append(frame, a.tabsFrame(p))
	side := a.sidebarWidth()
	rightWidth := a.cols
	if side > 0 {
		rightWidth -= side + 1
	}
	panelWidth := a.podPanelWidth()
	if panelWidth > 0 {
		rightWidth -= panelWidth + 1
	}
	var panelLines []string
	if panelWidth > 0 {
		panelLines = a.podPanelLines(panelWidth, p)
	}
	treeOnly := side == 0 && a.treeFocus && a.view == nil && panelWidth == 0
	tableHeader := a.formatRecord(record{}, rightWidth-2, true)
	var logLines []string
	var statLines []string
	if v := a.view; v != nil {
		tableHeader = first(containerName(v.target, v.container), "default container") + " · " + v.status
		if v.resource == "jobs" {
			tableHeader = "All job pods / containers · " + v.status
		}
		if v.kind == "flow" {
			v.lines = a.flowLines(rightWidth - 2)
			tableHeader = "Live routes · Ingress → Service → Pods · F opens this tab"
		}
		if v.follow {
			tableHeader += " · FOLLOW"
		}
		if v.previous {
			tableHeader += " · PREVIOUS"
		}
		logLines = v.filteredLines()
		if v.kind == "stats" || v.kind == "describe" {
			tableHeader = v.status
			if v.kind == "stats" {
				statLines = v.nodeStatsLines(rightWidth, p)
			} else {
				statLines = prettyDescribe(v.lines, v.query, rightWidth, p)
			}
			logLines = make([]string, len(statLines))
		}
		if a.searching || v.query != "" {
			tableHeader += "  /" + v.query
			if a.searching {
				tableHeader += "▏"
			}
		}
		if v.follow {
			v.top = max(0, len(logLines)-(a.rows-6))
		}
		v.top = clamp(v.top, 0, max(0, len(logLines)-(a.rows-6)))
	}
	if treeOnly {
		tableHeader = "NAMESPACES · Enter open · Tab resources"
	}
	heading := p.header + fit(" "+tableHeader, rightWidth)
	if side > 0 {
		heading = p.header + fit(" NAMESPACES", side) + p.muted + "│" + heading
	}
	if panelWidth > 0 {
		heading += p.muted + "│" + p.header + fit(" POD METRICS", panelWidth-4) + " ×  "
	}
	frame = append(frame, heading+reset)

	height := a.rows - 6
	tree := a.tree()
	a.treeTop = scrollTop(a.treeSelected, a.treeTop, height, len(tree))
	a.tableTop = scrollTop(a.tableSelected, a.tableTop, height, len(a.visible))
	for i := 0; i < height; i++ {
		left, right := "", ""
		leftStyle, rightStyle := p.base, p.base
		ti := a.treeTop + i
		if ti < len(tree) {
			left = " " + tree[ti].label
			if tree[ti].resource == a.resource && tree[ti].namespace == a.namespace {
				leftStyle = p.accent + "\x1b[1m"
			}
			if a.treeFocus && ti == a.treeSelected {
				leftStyle = p.selected
			}
			if tree[ti].resource == "" {
				leftStyle += a.namespaceColor(tree[ti].namespace) + "\x1b[1m"
			}
		}
		ri := a.tableTop + i
		if ri < len(a.visible) {
			r := a.visible[ri]
			right = " " + a.formatRecord(r, rightWidth-2, false)
			if r.Tone == "warn" {
				rightStyle = p.warn
			}
			if r.Tone == "bad" {
				rightStyle = p.bad
			}
			if !a.treeFocus && ri == a.tableSelected {
				rightStyle = p.selected
			}
		} else if i == 0 {
			right = " No matching resources"
			if a.snapshot.Errors[a.resource] != "" {
				right = " Unavailable · see status below"
			}
			if a.loading {
				right = " Loading resources…"
			}
			rightStyle = p.muted
		}
		if treeOnly {
			right, rightStyle = left, leftStyle
		}
		if v := a.view; v != nil {
			right, rightStyle = "", p.base
			if index := v.top + i; index < len(logLines) {
				right = " " + dropCells(logLines[index], v.left)
				rightStyle = logColor(logLines[index], p)
				if v.kind == "flow" {
					rightStyle = p.accent
				}
			} else if i == 0 {
				right, rightStyle = " Waiting for output…", p.muted
				if v.done {
					right = " No output"
				}
				if v.query != "" {
					right = " No matching lines"
				}
			}
		}
		content := rightStyle + fit(right, rightWidth)
		if a.view == nil && !treeOnly && ri < len(a.visible) && a.visible[ri].Namespace != "" {
			// Tint the identity column while preserving health colors and selection backgrounds.
			nameWidth := 1 + clamp((rightWidth-2)/3, 14, 34)
			plain := fit(right, rightWidth)
			// Split by terminal cells, never by byte offsets (resource names can be Unicode).
			prefix := strings.TrimSuffix(plain, dropCells(plain, nameWidth))
			content = rightStyle + a.namespaceColor(a.visible[ri].Namespace) + prefix + rightStyle + dropCells(plain, nameWidth)
		}
		if v := a.view; v != nil && (v.kind == "stats" || v.kind == "describe") {
			content = p.base + fit("", rightWidth)
			if index := v.top + i; index < len(statLines) {
				content = statLines[index]
			}
		}
		if panelWidth > 0 {
			metricLine := p.base + fit("", panelWidth)
			if i < len(panelLines) {
				metricLine = panelLines[i]
			}
			content += p.muted + "│" + metricLine
		}
		if side > 0 {
			frame = append(frame, leftStyle+fit(left, side)+p.muted+"│"+content+reset)
		} else {
			frame = append(frame, content+reset)
		}
	}
	detail := " Select a resource · Enter for pod / Job logs or resource details"
	if r, ok := a.selected(); ok {
		detail = " " + first(r.Namespace, "cluster") + " / " + r.Name
		if a.resource == "pods" {
			detail += "   container: " + first(containerName(r, a.container), "default") + "  [c]"
		}
	}
	if v := a.view; v != nil {
		detail = " " + v.title
	}
	frame = append(frame, line(" "+state+" · "+summary+" · "+strings.TrimSpace(detail), p.header))
	status := a.status
	style := p.muted
	if message := a.snapshot.Errors[a.resource]; message != "" {
		status = "STALE / UNAVAILABLE · " + message
		style = p.warn
	} else if message := a.snapshot.Errors["namespaces"]; message != "" {
		status = "Namespaces unavailable · " + message
		style = p.warn
	}
	if status == "" || status == "Loading cluster…" && !a.snapshot.Updated.IsZero() {
		status = "Updated " + a.snapshot.Updated.Local().Format("15:04:05") + "  ·  " + a.archiveStatus
	}
	if (a.status == "" || a.status == "Loading cluster…") && (strings.Contains(a.archiveStatus, "FAILED") || strings.Contains(a.archiveStatus, "capture issues")) {
		status = a.archiveStatus
		style = p.warn
	}
	if v := a.view; v != nil {
		status = fmt.Sprintf("%d lines · showing %d–%d · %s", len(logLines), min(v.top+1, len(logLines)), min(v.top+height, len(logLines)), v.status)
	}
	if a.view == nil {
		prefix := fmt.Sprintf("%s · %d", a.resource, len(a.visible))
		if a.searching || a.query != "" {
			prefix += "  /" + a.query
			if a.searching {
				prefix += "▏"
			}
		}
		status = prefix + " · " + status
	}
	frame = append(frame, line(" "+status, style), a.footerFrame(a.cols, p))
	return frame
}
func (a *app) sidebarWidth() int {
	if a.config.HideSidebar || a.cols < 65 {
		return 0
	}
	return min(a.config.SidebarWidth, a.cols/3)
}
func (a *app) formatRecord(r record, target int, header bool) string {
	var columns []string
	for _, kind := range resourceTypes {
		if kind.key == a.resource {
			columns = kind.columns
			break
		}
	}
	values := r.Cells
	if header {
		values = columns
	}
	if len(values) == 0 {
		return ""
	}
	// Keep name and health readable on small terminals; extra columns appear as space allows.
	widths := map[string]int{"READY": 7, "STATUS": 18, "RESTARTS": 8, "UPDATED": 8, "AVAILABLE": 10, "REVISION": 10, "TYPE": 12, "COUNT": 5, "COMPLETE": 9, "ACTIVE": 6, "FAILED": 6, "SCHEDULE": 17, "LAST RUN": 20, "CLASS": 15, "CAPACITY": 10, "CPU CAP": 7, "MEM CAP": 12, "VERSION": 16, "CLUSTER-IP": 16, "EXTERNAL-IP": 16, "IP": 16, "REASON": 20}
	available := target - 7
	var fields []string
	for i, value := range values {
		w := 18
		if i == 0 {
			w = clamp(target/3, 14, 34)
		} else if size, ok := widths[columns[i]]; ok {
			w = size
		}
		if columns[i] == "MESSAGE" {
			w = available
		}
		if i > 0 && w > available {
			break
		}
		w = min(w, max(0, available))
		if w == 0 {
			break
		}
		fields = append(fields, fit(value, w))
		available -= w + 1
	}
	if a.namespace == "" && a.resource != "nodes" && available >= 16 {
		ns := r.Namespace
		if header {
			ns = "NAMESPACE"
		}
		fields = append(fields, fit(ns, available))
		available = 0
	}
	label := age(r.Seen, time.Now())
	if header {
		label = "AGE"
		if a.resource == "events" {
			label = "LAST"
		}
	}
	return fit(strings.Join(fields, " "), max(0, target-7)) + " " + fit(label, 6)
}
func (a *app) viewerFrame(frame []string, p palette) []string {
	v := a.view
	if v.kind == "values" {
		return a.valuesFrame(p)
	}
	line := func(text, style string) string { return style + fit(text, a.cols) + reset }
	frame = append(frame, line(" "+v.title, p.accent))
	state := v.status
	if v.kind == "archive" {
		state += " · " + first(v.archiveFilter.Kind, "all kinds") + " · K change kind"
	}
	if v.kind == "logs" {
		state = first(containerName(v.target, v.container), "default container") + " · " + state
		if v.previous {
			state += " · PREVIOUS"
		}
		if v.follow {
			state += " · FOLLOW"
		}
	}
	if v.query != "" || a.searching {
		state += "   /" + v.query
		if a.searching {
			state += "▏"
		}
	}
	frame = append(frame, line(" "+state, p.header))
	lines := v.filteredLines()
	height := a.rows - 5
	if v.kind == "archive" {
		v.top = scrollTop(v.selected, v.top, height, len(lines))
	}
	if v.follow {
		v.top = max(0, len(lines)-height)
	}
	v.top = clamp(v.top, 0, max(0, len(lines)-height))
	for i := 0; i < height; i++ {
		text := ""
		style := p.base
		if index := v.top + i; index < len(lines) {
			text = lines[index]
			if v.kind == "archive" && index == v.selected {
				style = p.selected
			}
		} else if i == 0 {
			text = "Waiting for output…"
			if v.done {
				text = "No output"
			}
			if v.query != "" {
				text = "No matching lines"
			}
			style = p.muted
		}
		text = dropCells(text, v.left)
		if v.kind == "yaml" {
			frame = append(frame, syntaxLine(" "+text, a.cols, p)+reset)
		} else {
			frame = append(frame, line(" "+text, style))
		}
	}
	frame = append(frame, line(fmt.Sprintf(" %d lines · showing %d–%d · horizontal offset %d", len(lines), min(v.top+1, len(lines)), min(v.top+height, len(lines)), v.left), p.muted), a.footerFrame(a.cols, p))
	return frame
}

type footerAction struct {
	label string
	key   key
}

func (a *app) footerActions() []footerAction {
	if a.view == nil {
		if a.resource == "events" {
			return append(a.eventFilterActions(), footerAction{"Enter info", key{code: keyEnter}}, footerAction{"? help", key{r: '?'}}, footerAction{"q quit", key{r: 'q'}})
		}
		enter := "Enter info"
		if a.resource == "pods" || a.resource == "jobs" {
			enter = "Enter logs"
		} else if a.resource == "nodes" {
			enter = "Enter stats"
		} else if a.resource == "secrets" || a.resource == "configmaps" {
			enter = "Enter values"
			return []footerAction{{"↑", key{code: keyUp}}, {"↓", key{code: keyDown}}, {enter, key{code: keyEnter}}, {"e Edit", key{r: 'e'}}, {"y YAML", key{r: 'y'}}, {"Ctrl+F search", key{r: 6}}, {"? help", key{r: '?'}}, {"q quit", key{r: 'q'}}}
		}
		actions := []footerAction{{"↑", key{code: keyUp}}, {"↓", key{code: keyDown}}, {enter, key{code: keyEnter}}, {"Ctrl+F search", key{r: 6}}, {"s shell", key{r: 's'}}}
		if a.resource == "pods" {
			actions = append(actions, footerAction{"m metrics", key{r: 'm'}})
		}
		return append(actions, footerAction{"A archive", key{r: 'A'}}, footerAction{"? help", key{r: '?'}}, footerAction{"q quit", key{r: 'q'}})
	}
	if a.view.kind == "archive" {
		return []footerAction{{"Esc back", key{code: keyEscape}}, {"↑", key{code: keyUp}}, {"↓", key{code: keyDown}}, {"Enter detail", key{code: keyEnter}}, {"Ctrl+F search", key{r: 6}}, {"n older", key{r: 'n'}}, {"N newer", key{r: 'N'}}}
	}
	if a.view.kind == "values" {
		return []footerAction{{"Esc close", key{code: keyEscape}}, {"c Copy", key{r: 'c'}}, {"e Edit", key{r: 'e'}}, {"Tab key", key{code: keyTab}}, {"↑", key{code: keyUp}}, {"↓", key{code: keyDown}}, {"←", key{code: keyLeft}}, {"→", key{code: keyRight}}, {"Ctrl+F", key{r: 6}}}
	}
	if a.view.kind == "logs" {
		actions := []footerAction{{"Tab resources", key{code: keyTab}}, {"Esc close", key{code: keyEscape}}, {"↑", key{code: keyUp}}, {"↓", key{code: keyDown}}, {"Ctrl+F", key{r: 6}}, {"f follow", key{r: 'f'}}}
		if a.view.resource != "jobs" {
			actions = append(actions, footerAction{"c container", key{r: 'c'}}, footerAction{"p previous", key{r: 'p'}})
		}
		return actions
	}
	if a.view.kind == "describe" {
		return []footerAction{{"Tab resources", key{code: keyTab}}, {"Esc back", key{code: keyEscape}}, {"↑", key{code: keyUp}}, {"↓", key{code: keyDown}}, {"Ctrl+F search", key{r: 6}}, {"r refresh", key{r: 'r'}}}
	}
	if a.view.kind == "stats" {
		return []footerAction{{"Tab resources", key{code: keyTab}}, {"Esc close", key{code: keyEscape}}, {"↑", key{code: keyUp}}, {"↓", key{code: keyDown}}, {"Ctrl+F", key{r: 6}}, {"r refresh", key{r: 'r'}}}
	}
	if a.view.kind == "flow" {
		return []footerAction{{"Tab resources", key{code: keyResources}}, {"↑", key{code: keyUp}}, {"↓", key{code: keyDown}}, {"←", key{code: keyLeft}}, {"→", key{code: keyRight}}, {"Ctrl+F search", key{r: 6}}, {"r refresh", key{r: 'r'}}}
	}
	return []footerAction{{"Esc back", key{code: keyEscape}}, {"↑", key{code: keyUp}}, {"↓", key{code: keyDown}}, {"←", key{code: keyLeft}}, {"→", key{code: keyRight}}, {"Ctrl+F filter", key{r: 6}}, {"f follow", key{r: 'f'}}, {"c container", key{r: 'c'}}, {"p previous", key{r: 'p'}}}
}

func (a *app) eventFilterActions() []footerAction {
	return []footerAction{{"[T Time: " + eventWindows[a.eventWindow].label + " ▾]", key{r: 'T'}}, {"[K Type: " + first(a.eventType, "All") + " ▾]", key{r: 'K'}}, {"[Ctrl+F Search]", key{r: 6}}, {"[X Reset]", key{r: 'X'}}}
}

func (a *app) valuesBounds() (left, top, w, h int) {
	w, h = min(a.cols-4, 100), min(a.rows-4, 24)
	return (a.cols - w) / 2, (a.rows - h) / 2, w, h
}

func (a *app) valuesFrame(p palette) []string {
	v := a.view
	frame := make([]string, a.rows)
	for i := range frame {
		frame[i] = p.base + fit("", a.cols) + reset
		if len(v.background) == a.rows && v.backgroundCols == a.cols {
			frame[i] = v.background[i]
		}
	}
	left, top, w, h := a.valuesBounds()
	lines := v.filteredLines()
	if v.follow {
		v.top = max(0, len(lines)-(h-4))
	}
	v.top = clamp(v.top, 0, max(0, len(lines)-(h-4)))
	state := v.status
	if len(v.values) > 0 {
		state += " · key: " + v.values[v.selected].name
	}
	if a.searching || v.query != "" {
		state += "  /" + v.query
		if a.searching {
			state += "▏"
		}
	}
	box := []string{p.header + "╭─ " + fit(v.title, w-5) + "─╮", p.header + "│" + fit(" "+state, w-2) + "│"}
	for i := 0; i < h-4; i++ {
		text := ""
		if index := v.top + i; index < len(lines) {
			text = lines[index]
		} else if i == 0 {
			text = "Loading values…"
			if v.done {
				text = "No values"
			}
			if v.query != "" {
				text = "No matching lines"
			}
		}
		content := syntaxLine(" "+dropCells(text, v.left), w-2, p)
		if len(v.values) > 0 && text == safeText(v.values[v.selected].name)+": |" {
			content = p.selected + fit(" "+dropCells(text, v.left), w-2)
		}
		box = append(box, p.header+"│"+content+p.header+"│")
	}
	box = append(box, p.header+"│"+a.footerFrame(w-2, p)+p.header+"│", p.header+"╰"+strings.Repeat("─", w-2)+"╯")
	for i, line := range box {
		frame[top+i] = p.base + strings.Repeat(" ", left) + line + p.base + strings.Repeat(" ", a.cols-left-w) + reset
	}
	return frame
}

var syntaxTokens = regexp.MustCompile(`"(?:[^"\\]|\\.)*"|'(?:[^']|'')*'|#[^\r\n]*|[-+]?[0-9]+(?:\.[0-9]+)?(?:[eE][-+]?[0-9]+)?|[A-Za-z_][A-Za-z_0-9./-]*|[{}\[\],:=|>]`)

func syntaxLine(text string, target int, p palette) string {
	text = fit(text, target)
	var out strings.Builder
	out.WriteString(p.base)
	end := 0
	for _, span := range syntaxTokens.FindAllStringIndex(text, -1) {
		out.WriteString(text[end:span[0]])
		token := text[span[0]:span[1]]
		tail := strings.TrimLeft(text[span[1]:], " \t")
		style := p.base
		switch {
		case strings.HasPrefix(token, "#") && (span[0] == 0 || text[span[0]-1] == ' '):
			style = p.muted
		case strings.HasPrefix(tail, ":") || strings.HasPrefix(tail, "="):
			style = p.accent
		case token[0] == '"' || token[0] == '\'':
			style = "\x1b[48;5;234;38;5;114m"
		case token == "true" || token == "false" || token == "null" || token == "yes" || token == "no":
			style = p.warn
		default:
			if _, err := strconv.ParseFloat(token, 64); err == nil {
				style = p.warn
			}
		}
		out.WriteString(style)
		out.WriteString(token)
		out.WriteString(p.base)
		end = span[1]
	}
	out.WriteString(text[end:])
	return out.String()
}

func (v *viewer) filteredLines() []string {
	if v.query == "" || v.kind == "archive" {
		return v.lines
	}
	var lines []string
	query := strings.ToLower(v.query)
	for _, line := range v.lines {
		if strings.Contains(strings.ToLower(line), query) {
			lines = append(lines, line)
		}
	}
	return lines
}

func logColor(line string, p palette) string {
	// ponytail: severity words cover plain text and JSON logs; parse structured levels if message text causes false positives.
	severity := 0
	for _, word := range strings.FieldsFunc(strings.ToLower(line), func(r rune) bool { return !unicode.IsLetter(r) }) {
		switch word {
		case "error", "errors", "err", "fatal", "panic", "critical":
			return p.bad
		case "warn", "warning":
			severity = max(severity, 3)
		case "info":
			severity = max(severity, 2)
		case "debug", "trace":
			severity = max(severity, 1)
		}
	}
	return []string{p.base, p.muted, p.accent, p.warn}[severity]
}

func (a *app) footerText() string {
	labels := []string{}
	for _, action := range a.footerActions() {
		labels = append(labels, action.label)
	}
	return " " + strings.Join(labels, "  ")
}

func (a *app) footerFrame(cols int, p palette) string {
	return actionsFrame(a.footerActions(), cols, p)
}

func actionsFrame(actions []footerAction, cols int, p palette) string {
	line := p.base + " "
	remaining := max(0, cols-1)
	for _, action := range actions {
		size := min(remaining, width(action.label))
		line += p.header + fit(action.label, size)
		remaining -= size
		gap := min(remaining, 2)
		line += p.base + strings.Repeat(" ", gap)
		remaining -= gap
	}
	return line + p.base + strings.Repeat(" ", remaining) + reset
}

func switchingFrame(frame []string, cols int, theme, cluster string, step int) []string {
	frame = append([]string(nil), frame...)
	p := colors(theme)
	spinner := []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")
	if len(frame) < 5 || cols < 30 {
		frame[len(frame)/2] = p.header + fit(" "+string(spinner[step%len(spinner)])+" Switching cluster…", cols) + reset
		return frame
	}
	w := min(52, cols-4)
	left := (cols - w) / 2
	lines := []string{"╭" + strings.Repeat("─", w-2) + "╮", "│" + fit(" "+string(spinner[step%len(spinner)])+" Switching cluster…", w-2) + "│", "│" + fit(" "+cluster, w-2) + "│", "│" + fit(" Please wait", w-2) + "│", "╰" + strings.Repeat("─", w-2) + "╯"}
	for i, line := range lines {
		frame[(len(frame)-len(lines))/2+i] = p.muted + strings.Repeat(" ", left) + p.header + line + p.muted + strings.Repeat(" ", cols-left-w) + reset
	}
	return frame
}
func scrollTop(selected, top, height, total int) int {
	if selected < top {
		top = selected
	}
	if selected >= top+height {
		top = selected - height + 1
	}
	return clamp(top, 0, max(0, total-height))
}
func safeText(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return ' '
		}
		return r
	}, value)
}
func runeWidth(r rune) int {
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) {
		return 0
	}
	// ponytail: common CJK/emoji cell widths; use a grapheme library if complex emoji alignment is needed.
	if r >= 0x1100 && (r <= 0x115f || r >= 0x2e80 && r <= 0xa4cf || r >= 0xac00 && r <= 0xd7a3 || r >= 0xf900 && r <= 0xfaff || r >= 0xfe10 && r <= 0xfe6f || r >= 0xff01 && r <= 0xff60 || r >= 0xffe0 && r <= 0xffe6 || r >= 0x1f300 && r <= 0x1faff || r >= 0x20000 && r <= 0x3ffff) {
		return 2
	}
	return 1
}
func width(value string) int {
	n := 0
	for _, r := range value {
		n += runeWidth(r)
	}
	return n
}
func fit(value string, target int) string {
	if target <= 0 {
		return ""
	}
	value = safeText(value)
	if n := width(value); n <= target {
		return value + strings.Repeat(" ", target-n)
	}
	var out strings.Builder
	n := 0
	for _, r := range value {
		w := runeWidth(r)
		if n+w > target-1 {
			break
		}
		out.WriteRune(r)
		n += w
	}
	return out.String() + "…" + strings.Repeat(" ", target-n-1)
}
func dropCells(value string, n int) string {
	for i, r := range value {
		if n <= 0 {
			return value[i:]
		}
		n -= runeWidth(r)
	}
	return ""
}

// Use the same geometry for drawing and mouse hit testing.
type workspaceTab struct {
	label        string
	view         *viewer
	x, w, closeX int
}

func (a *app) workspaceTabs() []workspaceTab {
	tabs := []workspaceTab{{label: "Resources"}}
	for _, v := range a.outputs {
		label := map[string]string{"logs": "Logs", "describe": "Describe", "stats": "Node"}[v.kind]
		tabs = append(tabs, workspaceTab{label: label + ": " + first(v.target.Name, v.resource), view: v})
	}
	if a.flow != nil {
		tabs = append(tabs, workspaceTab{label: "Flow", view: a.flow})
	}
	available, x := a.cols, 1
	if side := a.sidebarWidth(); side > 0 {
		available -= side + 1
		x += side + 1
	}
	active := 0
	for i := range tabs {
		tabs[i].w = width(tabs[i].label) + 4
		if tabs[i].view != nil {
			tabs[i].w += 3
		}
		if tabs[i].view == a.view {
			active = i
		}
	}
	start, end, used := active, active+1, min(available, tabs[active].w)
	for start > 0 && used+tabs[start-1].w <= available {
		start--
		used += tabs[start].w
	}
	for end < len(tabs) && used+tabs[end].w <= available {
		used += tabs[end].w
		end++
	}
	tabs = tabs[start:end]
	for i := range tabs {
		tab := &tabs[i]
		tab.x, tab.w = x, min(tab.w, available)
		if tab.view != nil && tab.w >= 7 {
			tab.closeX = x + tab.w - 3
		}
		x += tab.w
	}
	return tabs
}

func (a *app) tabsFrame(p palette) string {
	line, used := "", 0
	if side := a.sidebarWidth(); side > 0 {
		line = p.muted + "\x1b[22m" + a.namespaceColor(a.namespace) + fit(" "+first(a.namespace, "All namespaces"), side) + p.muted + "│"
		used = side + 1
	}
	for _, tab := range a.workspaceTabs() {
		style := p.muted + "\x1b[22m"
		if tab.view == a.view {
			style = p.accent + "\x1b[1m"
		}
		label := fit("  "+tab.label, tab.w-2) + "  "
		if tab.closeX > 0 {
			label = fit("  "+tab.label, tab.w-5) + "  ×  "
		}
		line += style + label
		used += tab.w
	}
	return line + p.base + "\x1b[22m" + fit("", a.cols-used) + reset
}

// Keep assigned colors through refreshes and namespace additions. Golden-angle
// hue spacing gives nearby namespace entries visibly different colors.
func (a *app) namespaceColor(namespace string) string {
	if namespace == "" {
		return ""
	}
	if a.namespaceColors == nil {
		a.namespaceColors = map[string]string{}
	}
	if color := a.namespaceColors[namespace]; color != "" {
		return color
	}
	hue := float64(len(a.namespaceColors))*2.399963229728653 + 2*math.Pi/3
	channel := func(offset float64) int { return int(175 + 65*math.Cos(hue+offset)) }
	color := fmt.Sprintf("\x1b[38;2;%d;%d;%dm", channel(0), channel(-2*math.Pi/3), channel(2*math.Pi/3))
	a.namespaceColors[namespace] = color
	return color
}

func (a *app) shellBounds() (left, top, w, h int) {
	w, h = max(4, a.cols-6), max(5, a.rows-4)
	return max(0, (a.cols-w)/2), max(0, (a.rows-h)/2), w, h
}
func (a *app) drawShell() {
	if a.rows < 10 || a.cols < 40 {
		fmt.Print("\x1b[?25l")
		return
	}
	left, top, w, h := a.shellBounds()
	p := colors(a.config.Theme)
	var out strings.Builder
	out.WriteString("\x1b[?25l")
	header := p.header + "╭" + fit(" "+a.shellTitle, w-5) + " × ╮"
	fmt.Fprintf(&out, "\x1b[%d;%dH%s", top+1, left+1, header)
	for i := 1; i < h-2; i++ {
		fmt.Fprintf(&out, "\x1b[%d;%dH%s│\x1b[%d;%dH│", top+i+1, left+1, p.header, top+i+1, left+w)
	}
	fmt.Fprintf(&out, "\x1b[%d;%dH%s│%s│", top+h-1, left+1, p.header, fit(" Ctrl+] close · exit returns · wheel scrolls", w-2))
	fmt.Fprintf(&out, "\x1b[%d;%dH%s╰%s╯", top+h, left+1, p.header, strings.Repeat("─", w-2))
	row, col := a.shell.draw(&out, left+2, top+2, w-2, h-3)
	if row > 0 && col > 0 {
		fmt.Fprintf(&out, "\x1b[%d;%dH\x1b[?25h", row, col)
	}
	fmt.Fprint(os.Stdout, out.String())
}
