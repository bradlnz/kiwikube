package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) snapshot {
	t.Helper()
	data, err := os.ReadFile("testdata/cluster.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeSnapshot(data)
	if err != nil {
		t.Fatal(err)
	}
	return got
}
func TestDecodeAndFilter(t *testing.T) {
	got := fixture(t)
	for _, kind := range resourceTypes {
		if len(got.Resources[kind.key]) == 0 {
			t.Fatalf("missing %s", kind.key)
		}
	}
	p := got.Resources["pods"][0]
	if p.Cells[1] != "2/2" || p.Cells[3] != "2" || len(p.Containers) != 2 || got.Resources["services"][0].Cells[4] != "80/TCP" {
		t.Fatalf("unexpected pod or service: %+v", p)
	}
	if got.Resources["deployments"][0].Cells[2] != "Progressing" || got.Resources["deployments"][0].Cells[5] != "7" {
		t.Fatal("deployment rollout not decoded")
	}
	if got.Resources["cronjobs"][0].Cells[3] != "1" || got.Resources["jobs"][0].Cells[2] != "Complete" {
		t.Fatal("job active field not decoded")
	}
	event := got.Resources["events"][0]
	if event.Name != "worker-backoff" || event.Cells[3] != "9" || event.Tone != "warn" {
		t.Fatalf("events not sorted by most recent occurrence: %+v", event)
	}
	if got.Resources["nodes"][0].Host != "192.0.2.10" {
		t.Fatal("expected external SSH address")
	}
	a := newApp()
	a.snapshot, a.namespace, a.query = got, "default", "NODE-A"
	a.rebuild()
	if len(a.visible) != 1 {
		t.Fatal("case-insensitive search across fields")
	}
	a.namespace = ""
	a.query = ""
	a.rebuild()
	if len(a.visible) != 2 {
		t.Fatal("all namespaces")
	}
	if value := age(time.Now().Add(-25*time.Hour), time.Now()); value != "25h" {
		t.Fatalf("age = %q", value)
	}
}
func TestRefreshRetainsSelectionAndDeniedResources(t *testing.T) {
	a := newApp()
	a.applySnapshot(fixture(t))
	a.tableSelected = 1
	value := fixture(t)
	value.Resources["pods"] = append([]record{{metadata: metadata{Name: "aaa", UID: "new"}}}, value.Resources["pods"]...)
	a.applySnapshot(value)
	if selected, _ := a.selected(); selected.Name != "worker-1" {
		t.Fatal("refresh moved selected object")
	}
	value = fixture(t)
	value.Resources["pods"] = nil
	value.Errors = map[string]string{"pods": "Forbidden"}
	a.applySnapshot(value)
	if len(a.visible) != 3 {
		t.Fatal("denied refresh discarded cached resources")
	}
	a.handle(key{r: '/'})
	for _, c := range "nonexistent" {
		a.handle(key{r: c})
	}
	a.handle(key{code: keyEnter})
	if _, ok := a.selected(); ok {
		t.Fatal("empty search retained action target")
	}
	a.handle(key{code: keyEscape})
	if len(a.visible) != 3 {
		t.Fatal("escape should clear filter")
	}
}
func TestActionsAndConfig(t *testing.T) {
	a := newApp()
	a.context = "test-context"
	a.applySnapshot(fixture(t))
	r := a.visible[0]
	cmd, err := a.command(context.Background(), "shell", a.resource, r, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"kubectl", "--context", "test-context", "-n", "default", "exec", "-it", "api-1", "-c", "sidecar", "--", "/bin/sh"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("shell arguments = %q", cmd.Args)
	}
	cmd, err = a.command(context.Background(), "logs", a.resource, r, 0, true)
	if err != nil || !strings.Contains(strings.Join(cmd.Args, " "), "--previous") || strings.Contains(strings.Join(cmd.Args, " "), "--follow") {
		t.Fatal("previous logs must not follow")
	}
	a.config.SSHUser = "admin"
	cmd, err = a.command(context.Background(), "ssh", a.resource, r, 0, false)
	if err != nil || !reflect.DeepEqual(cmd.Args, []string{"ssh", "-t", "-l", "admin", "--", "192.0.2.10"}) {
		t.Fatalf("SSH: %v %v", cmd, err)
	}
	for _, host := range []string{"-oProxyCommand=bad", "user@host", "host;sh", "host\ncommand", ""} {
		if validSSHHost(host) {
			t.Fatalf("unsafe host accepted: %q", host)
		}
	}
	a.resource = "services"
	if _, err = a.command(context.Background(), "shell", a.resource, r, 0, false); err == nil {
		t.Fatal("service shell accepted")
	}
	path := filepath.Join(t.TempDir(), "settings", "config.json")
	a.config.Theme = "ocean"
	if err = a.config.save(path); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(path)
	if err != nil || c != a.config {
		t.Fatalf("config roundtrip: %+v %v", c, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0600 {
		t.Fatal("settings should be private")
	}
	c.RefreshSeconds = 0
	if err = c.save(path); err == nil {
		t.Fatal("invalid config saved")
	}
}
func TestFrameAndBoundedLogs(t *testing.T) {
	a := newApp()
	a.applySnapshot(fixture(t))
	ansi := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	for _, size := range [][2]int{{1, 1}, {8, 30}, {10, 40}, {24, 80}, {32, 140}} {
		a.rows, a.cols = size[0], size[1]
		for _, kind := range resourceTypes {
			a.resource = kind.key
			a.rebuild()
			for _, view := range []*viewer{nil, {kind: "stats", details: []string{"Name: node-a", "Conditions:", "  Ready: True"}}, {kind: "flow"}, {kind: "describe", lines: []string{"Name: api-1"}}, {kind: "logs", title: "Logs", lines: []string{"hello 世界\x1b[2J"}}, {kind: "values", title: "Values", lines: []string{`enabled: true`, `name: "世界"`}}, {kind: "yaml", lines: []string{`apiVersion: "v1"`}}} {
				a.view = view
				frame := a.frame()
				if len(frame) != a.rows {
					t.Fatalf("frame height %d, want %d", len(frame), a.rows)
				}
				for _, line := range frame {
					plain := ansi.ReplaceAllString(line, "")
					if width(plain) != a.cols || strings.ContainsRune(plain, '\x1b') {
						t.Fatalf("bad frame width/control characters: %q", plain)
					}
				}
			}
		}
	}
	v := viewer{top: 1}
	for _, line := range []string{"a", "b", "c", "d"} {
		v.appendLine(line, 3)
	}
	if strings.Join(v.lines, "") != "bcd" || v.top != 0 {
		t.Fatal("log retention or scroll anchoring")
	}
	a.view = &viewer{kind: "logs", follow: true}
	a.handle(key{r: 'k'})
	if a.view.follow {
		t.Fatal("scrolling should stop autoscroll")
	}
}
func TestInputSplitSequences(t *testing.T) {
	var d inputDecoder
	var got []key
	emit := func(k key) { got = append(got, k) }
	for _, chunk := range []string{"\x1b[", "6", "~", "\x1b[A", "\xc3", "\xa9"} {
		d.feed([]byte(chunk), emit)
	}
	if !reflect.DeepEqual(got, []key{{code: keyPageDown}, {code: keyUp}, {r: 'é'}}) {
		t.Fatalf("decoded: %+v", got)
	}
}

func TestStartupSelectionAndDeploymentAvailability(t *testing.T) {
	a := newApp()
	a.expanded = map[string]bool{"production": true}
	a.chooseTree() // A saved namespace may not have loaded yet.
	data := []byte(`{"kind":"DeploymentList","items":[{"metadata":{"name":"api","namespace":"default"},"spec":{"replicas":2},"status":{"readyReplicas":2,"updatedReplicas":2,"availableReplicas":1}}]}`)
	s, err := decodeSnapshot(data)
	if err != nil || len(s.Resources["deployments"]) != 1 {
		t.Fatalf("typed list without item kinds: %+v %v", s, err)
	}
	if s.Resources["deployments"][0].Cells[2] != "Progressing" {
		t.Fatal("ready pods still need to become available before rollout completes")
	}
}
func BenchmarkVisibleFrame(b *testing.B) {
	a := newApp()
	a.rows, a.cols = 40, 160
	for i := 0; i < 10000; i++ {
		a.visible = append(a.visible, record{Cells: []string{"api-123", "1/1", "Running", "0", "node-a", "10.0.0.1"}})
	}
	b.ResetTimer()
	for b.Loop() {
		a.frame()
	}
}

func TestClusterQuickPickKeyboardAndMouse(t *testing.T) {
	a := newApp()
	a.context = "dev"
	a.config.ClusterSlots = [5]string{"dev", "staging", "production"}
	var decoder inputDecoder
	decoder.feed([]byte("\x1b2"), func(k key) { a.handle(k) })
	if a.action != "cluster:staging" {
		t.Fatalf("Alt+2: %q", a.action)
	}
	a.action = ""
	a.view = &viewer{kind: "logs"}
	a.searching = true
	a.handle(key{r: '3', alt: true})
	if a.action != "cluster:production" {
		t.Fatal("cluster shortcuts must work inside viewers and search")
	}
	for _, cols := range []int{40, 80, 140} {
		a.cols = cols
		header := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(a.frame()[0], "")
		if !strings.HasSuffix(header, " 1  2  3  4  5 ") || strings.Contains(header, "staging") || strings.Contains(header, "production") {
			t.Fatalf("expected numeric slots: %q", header)
		}
		a.action = ""
		start, size := a.clusterButtons()
		packet := fmt.Sprintf("\x1b[<0;%d;1M", start+size)
		decoder.feed([]byte(packet), func(k key) { a.handle(k) })
		if a.action != "cluster:staging" {
			t.Fatalf("mouse hit at %d columns: %q", cols, a.action)
		}
		a.action = ""
		a.handle(key{code: keyMouse, mouseX: start + size, mouseY: 2})
		a.handle(key{r: '5', alt: true})
		a.handle(key{r: '1', alt: true})
		if a.action != "" {
			t.Fatal("unassigned/current slots or clicks below header should not switch")
		}
	}
}

func TestMouseNavigationAndSearch(t *testing.T) {
	for _, cols := range []int{50, 80, 140} {
		a := newApp()
		a.cols = cols
		a.applySnapshot(fixture(t))
		var decoder inputDecoder
		send := func(packet string) {
			decoder.feed([]byte(packet), func(k key) { a.handle(k) })
			a.frame()
		}
		send("\x1b[<0;2;5M") // Collapse all namespaces.
		if a.expanded[""] {
			t.Fatal("namespace click did not collapse")
		}
		send("\x1b[<0;2;5M")
	send("\x1b[<0;2;6M") // Open pods, including the full-width tree.
		if a.treeFocus || a.resource != "pods" {
			t.Fatal("tree click did not open pods")
		}
		if cols >= 65 && !strings.HasPrefix(a.frame()[3+a.treeSelected-a.treeTop], colors(a.config.Theme).accent) {
			t.Fatal("active resource label should keep the accent after focus moves to the table")
		}
		x := max(2, a.sidebarWidth()+2)
		send(fmt.Sprintf("\x1b[<0;%d;5M", x))
		if a.tableSelected != 1 || a.action != "" {
			t.Fatal("click must select a different resource without opening")
		}
		send(fmt.Sprintf("\x1b[<0;%d;5M", x))
		if a.action != "logs" {
			t.Fatal("clicking selected pod should open logs")
		}
		a.action = ""
		send(fmt.Sprintf("\x1b[<64;%d;5M", x))
		if a.tableSelected != 0 {
			t.Fatal("wheel up did not move selection")
		}
		send(fmt.Sprintf("\x1b[<65;%d;5M", x))
		if a.tableSelected != 1 {
			t.Fatal("wheel down did not move selection")
		}
		send("\x06api\r")
		if a.query != "api" || a.searching || len(a.visible) != 1 {
			t.Fatal("Ctrl+F should filter resources")
		}
		send("\r")
		if a.action != "logs" {
			t.Fatal("Enter should open pod logs")
		}
		a.action = ""
		send(fmt.Sprintf("\x1b[<0;%d;3M", x))
		if !a.searching {
			t.Fatal("click search row should open search")
		}
		a.handle(key{code: keyEscape})
		a.resource = "nodes"
		a.rebuild()
		send("\r")
		if a.action != "stats" {
			t.Fatal("Enter on a node should open stats")
		}
	}

	a := newApp()
	a.view = &viewer{kind: "logs", follow: true, lines: make([]string, 50)}
	a.frame()
	a.handle(key{code: keyMouse, mouseX: 30, mouseY: 5, scroll: -1})
	if a.view.follow || a.view.top != 31 {
		t.Fatal("wheel should scroll logs and stop following")
	}
	a.handle(key{r: 6})
	a.handle(key{r: 'x'})
	a.handle(key{code: keyEnter})
	if a.view.query != "x" || a.query != "" || a.searching {
		t.Fatal("Ctrl+F should edit viewer search")
	}
	a.view = &viewer{kind: "archive", top: 10, entries: make([]archiveEntry, 40), lines: make([]string, 40)}
	a.handle(key{code: keyMouse, mouseX: 2, mouseY: 6})
	if a.view.selected != 12 || a.action != "" {
		t.Fatal("archive click should account for scroll offset")
	}
	a.handle(key{code: keyMouse, mouseX: 2, mouseY: 6})
	if a.action != "archive-detail" {
		t.Fatal("click selected archive row should open detail")
	}
	a.action = ""
	a.handle(key{code: keyMouse, mouseX: 2, mouseY: a.rows})
	if a.action != "close" {
		t.Fatal("click Esc back should close viewer")
	}
}

func TestFooterClicksMatchKeyboard(t *testing.T) {
	for _, kind := range []string{"", "logs", "archive", "help"} {
		makeApp := func() *app {
			a := newApp()
			a.cols, a.treeFocus = 140, false
			a.applySnapshot(fixture(t))
			if kind != "" {
				a.view = &viewer{kind: kind, entries: make([]archiveEntry, archivePageSize), lines: make([]string, 40), top: 10, selected: 1}
				if kind == "logs" {
					a.outputs = []*viewer{a.view}
				}
			}
			return a
		}
		x := 2
		for _, action := range makeApp().footerActions() {
			clicked, typed := makeApp(), makeApp()
			cq, cr := clicked.handle(key{code: keyMouse, mouseX: x, mouseY: clicked.rows})
			tq, tr := typed.handle(action.key)
			if cq != tq || cr != tr || !reflect.DeepEqual(clicked, typed) {
				t.Fatalf("%s footer %q differs from keyboard", kind, action.label)
			}
			x += width(action.label) + 2
		}
	}
	a := newApp()
	a.cols = 40
	a.handle(key{code: keyMouse, mouseX: 55, mouseY: a.rows})
	if a.action != "" {
		t.Fatal("hidden footer labels must not respond to clicks")
	}
}

func TestLogsTabsAndSeverity(t *testing.T) {
	a := newApp()
	a.cols = 140
	a.applySnapshot(fixture(t))
	a.view = &viewer{kind: "logs", target: a.visible[0], lines: []string{"ERROR failed", "WARN retry", "INFO ready", "DEBUG request", "plain output"}}
	a.outputs = []*viewer{a.view}
	p := colors(a.config.Theme)
	frame := a.frame()
	if !strings.Contains(frame[1], "Resources") || !strings.Contains(frame[1], "Logs") || !strings.Contains(frame[2], "NAMESPACES") {
		t.Fatal("logs must stay beside the sidebar with resource/log tabs")
	}
	for i, style := range []string{p.bad, p.warn, p.accent, p.muted, p.base} {
		if !strings.Contains(frame[3+i], "│"+style+" "+a.outputs[0].lines[i]) {
			t.Fatalf("log severity color missing on line %d", i)
		}
	}
	a.handle(key{code: keyMouse, mouseX: a.workspaceTabs()[0].x + 2, mouseY: 2})
	if a.view != nil || len(a.outputs) == 0 {
		t.Fatal("resource tab should retain logs")
	}
	a.outputs[0].appendLine("ERROR arrived in background", 100)
	a.handle(key{code: keyMouse, mouseX: a.workspaceTabs()[1].x + 2, mouseY: 2})
	if a.view != a.outputs[0] || len(a.view.lines) != 6 {
		t.Fatal("logs tab lost background output")
	}
	a.handle(key{code: keyMouse, mouseX: 2, mouseY: 4})
	if a.resource != "nodes" || a.view != nil || len(a.outputs) == 0 {
		t.Fatal("sidebar should open resources while retaining the logs tab")
	}
	a.handle(key{code: keyMouse, mouseX: a.workspaceTabs()[1].closeX, mouseY: 2})
	if a.action != "close-output" || a.view != nil {
		t.Fatal("tab close should target the log stream")
	}
	for _, line := range []string{`{"level":"error","message":"failed"}`, "FATAL failed", "panic: failed", "INFO request ERROR failed"} {
		if logColor(line, p) != p.bad {
			t.Fatalf("missing error color: %s", line)
		}
	}
	if logColor("terror warningless informative", p) != p.base {
		t.Fatal("severity must match whole words")
	}
}

func TestSwitchingModal(t *testing.T) {
	ansi := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	for _, size := range [][2]int{{1, 1}, {8, 30}, {10, 40}, {24, 80}, {32, 140}} {
		a := newApp()
		a.rows, a.cols = size[0], size[1]
		base := a.frame()
		first := switchingFrame(base, a.cols, a.config.Theme, "production", 0)
		next := switchingFrame(base, a.cols, a.config.Theme, "production", 1)
		if len(first) != a.rows || !reflect.DeepEqual(base, a.frame()) {
			t.Fatal("modal changed frame size or source")
		}
		for _, line := range first {
			if width(ansi.ReplaceAllString(line, "")) != a.cols {
				t.Fatal("modal exceeds terminal width")
			}
		}
		if a.cols >= 30 && (reflect.DeepEqual(first, next) || !strings.Contains(strings.Join(first, ""), "Switching cluster")) {
			t.Fatal("switching modal must show animated progress")
		}
	}
	a := newApp()
	a.switching = "production"
	a.handle(key{r: 's'})
	if a.action != "" {
		t.Fatal("modal must block resource actions during switching")
	}
	if quit, _ := a.handle(key{r: 3}); !quit {
		t.Fatal("Ctrl+C should still quit while loading")
	}
}

func TestJobLogsAndValuesActions(t *testing.T) {
	a := newApp()
	a.context, a.treeFocus = "test-context", false
	a.applySnapshot(fixture(t))
	for resource, action := range map[string]string{"jobs": "logs", "secrets": "values", "configmaps": "values"} {
		a.resource, a.action = resource, ""
		a.rebuild()
		a.handle(key{code: keyEnter})
		if a.action != action {
			t.Fatalf("Enter on %s: got %s", resource, a.action)
		}
		r, _ := a.selected()
		cmd, err := a.command(context.Background(), action, resource, r, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		args := strings.Join(cmd.Args, " ")
		if resource == "jobs" {
			for _, flag := range []string{"--context test-context", "-n default", "logs job/migration", "--all-pods=true", "--all-containers=true", "--timestamps", "--follow"} {
				if !strings.Contains(args, flag) {
					t.Fatalf("job logs missing %s: %s", flag, args)
				}
			}
			if _, err := a.command(context.Background(), "shell", resource, r, 0, false); err == nil {
				t.Fatal("Job must not be treated as a pod shell")
			}
		} else if !strings.Contains(args, "get "+resource+"/"+r.Name+" -o json") {
			t.Fatalf("values command: %s", args)
		}
	}
	a.action = ""
	a.view = &viewer{kind: "logs", resource: "jobs"}
	a.handle(key{r: 'c'})
	a.handle(key{r: 'p'})
	if a.action != "" || strings.Contains(a.footerText(), "container") || strings.Contains(a.footerText(), "previous") {
		t.Fatal("Job logs use all containers without pod-only controls")
	}
}

func TestEventFilters(t *testing.T) {
	a := newApp()
	a.cols, a.resource = 140, "events"
	s := fixture(t)
	s.Resources["events"][0].Seen = time.Now().Add(-2 * time.Minute)
	s.Resources["events"][1].Seen = time.Now().Add(-2 * time.Hour)
	a.applySnapshot(s)
	a.handle(key{r: '2'}) // 5 minutes, measured from the last occurrence.
	if len(a.visible) != 1 || a.visible[0].Name != "worker-backoff" {
		t.Fatal("event time window should keep only recent occurrences")
	}
	a.handle(key{r: 'K'}) // Warnings.
	a.handle(key{code: keyDown})
	a.handle(key{code: keyEnter})
	if len(a.visible) != 1 || a.eventType != "Warning" {
		t.Fatal("warning filter")
	}
	a.handle(key{r: 'K'}) // Normal events outside this time window.
	a.handle(key{code: keyDown})
	a.handle(key{code: keyEnter})
	if len(a.visible) != 0 {
		t.Fatal("time and type filters must combine")
	}
	a.handle(key{r: 'X'})
	a.handle(key{r: 6})
	for _, r := range "backoff" {
		a.handle(key{r: r})
	}
	a.handle(key{code: keyEnter})
	if len(a.visible) != 1 {
		t.Fatal("event text search")
	}
	column := 2
	for _, control := range a.eventFilterActions() {
		if control.key.r == 'X' {
			a.handle(key{code: keyMouse, mouseX: column, mouseY: a.rows})
			break
		}
		column += width(control.label) + 2
	}
	if len(a.visible) != 2 || a.query != "" || a.eventType != "" || a.eventWindow != 0 {
		t.Fatal("click Reset should clear all event filters")
	}
	a.namespace = "production"
	a.handle(key{r: 'T'})
	a.handle(key{code: keyDown})
	a.handle(key{code: keyEnter})
	if len(a.visible) != 1 {
		t.Fatal("event filters should retain namespace selection")
	}
	a.namespace = "default"
	a.rebuild()
	if len(a.visible) != 0 {
		t.Fatal("event filter ignored namespace")
	}
	a.resource, a.namespace = "pods", ""
	a.rebuild()
	if len(a.visible) != 2 {
		t.Fatal("event time filter leaked into other resources")
	}
}

func TestValuesPopupAndSyntax(t *testing.T) {
	lines, _, err := valueLines([]byte(`{"data":{"token":"ZGVtby1vbmx5LXRva2Vu","binary":"AP8="}}`), "secrets")
	if err != nil || !strings.Contains(strings.Join(lines, "\n"), "demo-only-token") || !strings.Contains(strings.Join(lines, "\n"), "base64: AP8=") {
		t.Fatalf("decoded values: %v %v", lines, err)
	}
	if _, _, err := valueLines([]byte(`{"data":{"token":"not base64!"}}`), "secrets"); err == nil {
		t.Fatal("malformed secret encoding accepted")
	}
	lines, _, err = valueLines([]byte(`{"data":{"app.json":"{\"enabled\":true,\"name\":\"api\"}","empty":""}}`), "configmaps")
	if err != nil || !strings.Contains(strings.Join(lines, "\n"), `"enabled": true`) || !strings.Contains(strings.Join(lines, "\n"), `  ""`) {
		t.Fatalf("ConfigMap formatting: %v %v", lines, err)
	}
	a := newApp()
	a.cols, a.resource, a.treeFocus = 140, "secrets", false
	a.applySnapshot(fixture(t))
	background := a.frame()
	a.view = &viewer{kind: "values", resource: "secrets", title: "Secret values", lines: lines, background: background, backgroundCols: a.cols, done: true}
	frame := a.frame()
	if frame[0] != background[0] || !strings.Contains(strings.Join(frame, ""), "╭─ Secret values") {
		t.Fatal("values should open in a popup over the dashboard")
	}
	left, top, _, h := a.valuesBounds()
	a.handle(key{code: keyMouse, mouseX: left + 3, mouseY: top + h - 1})
	if a.action != "close" {
		t.Fatal("popup Esc label should be clickable")
	}
	p := colors(a.config.Theme)
	text := `{"name": "api", "enabled": true, "port": 8080} # comment`
	colored := syntaxLine(text, 100, p)
	for _, token := range []string{p.accent + `"name"`, "\x1b[48;5;234;38;5;114m\"api\"", p.warn + "true", p.warn + "8080", p.muted + "# comment"} {
		if !strings.Contains(colored, token) {
			t.Fatalf("syntax token missing: %q", token)
		}
	}
	ansi := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	if ansi.ReplaceAllString(colored, "") != fit(text, 100) {
		t.Fatal("syntax coloring changed the values")
	}
	if strings.Contains(syntaxLine("value: \x1b[2J", 40, p), "\x1b[2J") {
		t.Fatal("values must not inject terminal commands")
	}
}

func TestMenusSearchValuesAndFlow(t *testing.T) {
	a := newApp()
	a.cols, a.rows = 140, 32
	a.applySnapshot(fixture(t))
	a.handle(key{code: keyMouse, mouseX: 14, mouseY: 1}) // View menu.
	if a.menu == nil {
		t.Fatal("View menu did not open")
	}
	a.handle(key{code: keyDown})
	a.handle(key{code: keyDown})
	a.handle(key{code: keyEnter})
	if a.view == nil || a.view.kind != "flow" || a.menu != nil {
		t.Fatal("Flow menu action")
	}
	flow := strings.Join(a.flowLines(100), "\n")
	if !strings.Contains(flow, "api.example.test/") || !strings.Contains(flow, "api:80 → api-1 [Running]") || strings.Contains(flow, "worker-1") {
		t.Fatal(flow)
	}
	a.namespace = "production"
	if !strings.Contains(strings.Join(a.flowLines(40), ""), "No Ingress routes") {
		t.Fatal("flow namespace filter")
	}
	a.namespace = ""
	a.snapshot.Resources["services"][0].Selector["tier"] = "web"
	if !strings.Contains(strings.Join(a.flowLines(40), ""), "No matching pods") {
		t.Fatal("flow must match all selector labels")
	}
	a.snapshot.Resources["services"] = nil
	if !strings.Contains(strings.Join(a.flowLines(40), ""), "Service unavailable") {
		t.Fatal("missing backend")
	}
	a.handle(key{r: 6})
	modal := strings.Join(a.searchFrame(a.frame()), "\n")
	if !a.searching || !strings.Contains(modal, "╭─ Search · flow") {
		t.Fatal("search modal missing")
	}
	a.handle(key{code: keyEscape})
	lines, values, err := valueLines([]byte(`{"data":{"a":"bGluZTEKbGluZTIK","b":"AP8="}}`), "secrets")
	if err != nil || values[0].raw != "line1\nline2\n" || values[1].raw != "\x00\xff" {
		t.Fatalf("raw values: %v %v", values, err)
	}
	a.view = &viewer{kind: "values", resource: "secrets", lines: lines, values: values}
	a.handle(key{code: keyTab})
	a.handle(key{r: 'c'})
	if a.view.selected != 1 || a.action != "copy-value" {
		t.Fatal("select and copy binary value")
	}
	a.view.top = 0
	left, top, _, _ := a.valuesBounds()
	a.handle(key{code: keyMouse, mouseX: left + 4, mouseY: top + 4})
	if a.view.selected != 0 {
		t.Fatal("click selected wrong value")
	}
	a.handle(key{r: 'e'})
	if a.action != "edit" {
		t.Fatal("edit popup action")
	}
	a.context = "test-cluster"
	r := record{metadata: metadata{Name: "app-secret", Namespace: "default"}}
	cmd, err := a.command(context.Background(), "edit", "secrets", r, 0, false)
	if err != nil || !strings.Contains(strings.Join(cmd.Args, " "), "--context test-cluster -n default edit secrets/app-secret") {
		t.Fatalf("native edit: %v %v", cmd, err)
	}
	if _, err := a.command(context.Background(), "edit", "pods", r, 0, false); err == nil {
		t.Fatal("unrequested pod editing")
	}
	a.view = &viewer{kind: "describe", lines: []string{"Name: api-1", "Status: Running"}}
	if !strings.Contains(strings.Join(a.frame(), ""), colors(a.config.Theme).accent+"Name") {
		t.Fatal("describe keys not colored")
	}
}

func TestWorkspaceTabs(t *testing.T) {
	a := newApp()
	a.rows, a.cols = 32, 140
	a.outputs = []*viewer{{kind: "logs", target: record{metadata: metadata{Name: "api-1"}}}}
	a.handle(key{r: 'F'})
	a.flow.query = "api"
	a.handle(key{code: keyResources})
	a.handle(key{r: 'F'})
	if a.view.query != "api" {
		t.Fatal("switching tabs discarded flow state")
	}
	ansi := regexp.MustCompile(`\[[0-9;]*m`)
	for _, name := range []string{"api-1", strings.Repeat("long-name-", 20), "世界-api"} {
		a.outputs[0].target.Name = name
		for _, cols := range []int{40, 50, 80, 140} {
			a.cols = cols
			for _, hide := range []bool{false, true} {
				a.config.HideSidebar = hide
				for _, active := range []*viewer{nil, a.outputs[0], a.flow} {
					a.view = active
					frame := a.tabsFrame(colors(a.config.Theme))
					if width(ansi.ReplaceAllString(frame, "")) != cols {
						t.Fatal("tabs overflow viewport")
					}
					visible := false
					for _, tab := range a.workspaceTabs() {
						if tab.view == active {
							visible = true
						}
						a.handle(key{code: keyMouse, mouseX: tab.x + 2, mouseY: 2})
						if a.view != tab.view {
							t.Fatal("tab click and drawing disagree")
						}
						a.view = active
					}
					if !visible {
						t.Fatal("active tab hidden")
					}
				}
			}
		}
	}
	a.cols, a.config.HideSidebar, a.outputs[0].target.Name = 140, false, "api-1"
	a.view = a.outputs[0]
	tabs := a.workspaceTabs()
	a.handle(key{code: keyMouse, mouseX: tabs[2].closeX, mouseY: 2})
	if a.flow != nil || a.view != a.outputs[0] || a.action != "" {
		t.Fatal("closing Flow changed Logs")
	}
	a.handle(key{r: 'F'})
	tabs = a.workspaceTabs()
	a.handle(key{code: keyMouse, mouseX: tabs[1].closeX, mouseY: 2})
	if a.action != "close-output" || a.view != a.flow {
		t.Fatal("closing inactive logs changed the selected tab")
	}
}

func TestNamespaceColorsAndPodPanel(t *testing.T) {
	a := newApp()
	a.cols, a.rows = 140, 32
	a.applySnapshot(fixture(t))
	before := a.namespaceColor("default")
	if before == a.namespaceColor("production") || a.namespaceColor("") != "" {
		t.Fatal("namespace colors collide")
	}
	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		color := a.namespaceColor(fmt.Sprintf("namespace-%d", i))
		if seen[color] {
			t.Fatal("namespace colors reused")
		}
		seen[color] = true
	}
	s := fixture(t)
	s.Namespaces = append(s.Namespaces, "aaa-new")
	a.applySnapshot(s)
	if a.namespaceColor("default") != before {
		t.Fatal("refresh changed existing namespace color")
	}
	a.treeFocus = false
	a.handle(key{r: 'm'})
	if !a.showPodMetrics || a.podPanelWidth() == 0 {
		t.Fatal("pod metrics toggle")
	}
	a.podMetrics = &podMetrics{gauges: []nodeGauge{{label: "CPU", value: .5, limit: 1, detail: "0.5 cores"}, {label: "Memory", value: 1024, limit: 2048, detail: "1 KiB"}}}
	ansi := regexp.MustCompile(`\[[0-9;]*m`)
	for _, cols := range []int{40, 80, 100, 140} {
		a.cols = cols
		for _, line := range a.frame() {
			if width(ansi.ReplaceAllString(line, "")) != cols {
				t.Fatal("pod panel width")
			}
		}
	}
	a.cols = 140
	frame := a.frame()
	if !strings.Contains(frame[2], "POD METRICS") || !strings.Contains(frame[3], before+" api-1") {
		t.Fatal("panel or namespace identity color missing")
	}
	a.handle(key{code: keyMouse, mouseX: a.cols - 2, mouseY: 3})
	if a.showPodMetrics {
		t.Fatal("close panel")
	}
	a.resource = "nodes"
	a.rebuild()
	a.handle(key{code: keyEnter})
	if a.action != "stats" {
		t.Fatal("Enter on node must open stats")
	}
}
