package main

import (
	"regexp"
	"strings"
	"testing"
)

func TestPrettyDescribe(t *testing.T) {
	raw := []string{"Name: node-a", "Labels: app=api", "        team=platform", "Conditions:", "  Type    Status  Reason", "  ----    ------  ------", "  Ready   True    KubeletReady", "System Info:", "  OS Image: Fixture Linux", "Containers:", "  api:", "    Image: example.test/api:v1", "Annotations:", "  long: " + strings.Repeat("value-", 40), "  command: https://example.test:8443/api?name=世界", "Events: <none>", "  injected: \x1b[2J"}
	p := colors("kiwi")
	ansi := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	for _, cols := range []int{20, 40, 70, 140} {
		rendered := prettyDescribe(raw, "", cols, p)
		plain := []string{}
		for _, line := range rendered {
			text := ansi.ReplaceAllString(line, "")
			if width(text) != cols || strings.ContainsRune(text, '\x1b') {
				t.Fatalf("bad description width/control: %q", text)
			}
			plain = append(plain, text)
		}
		joined := strings.Join(plain, "\n")
		if !strings.Contains(joined, "Overview") || !strings.Contains(joined, "Conditions") || !strings.Contains(joined, "Containers") || !strings.Contains(joined, "Fixture Linux") {
			t.Fatal(joined)
		}
		compact := strings.NewReplacer(" ", "", "\n", "", "│", "", "╭", "", "╮", "", "╰", "", "╯", "", "─", "").Replace(joined)
		if !strings.Contains(compact, strings.Repeat("value-", 40)) {
			t.Fatal("long annotation was truncated")
		}
	}
	found := strings.Join(prettyDescribe(raw, "image", 80, p), "\n")
	if !strings.Contains(found, "Fixture Linux") || !strings.Contains(found, "example.test/api:v1") || strings.Contains(found, "KubeletReady") {
		t.Fatal("describe search lost matching fields or retained unrelated lines")
	}
	v := &viewer{kind: "stats", status: "Stats unavailable: Forbidden", details: raw, detailStatus: "Description"}
	for _, cols := range []int{40, 80, 115} {
		lines := v.nodeStatsLines(cols, p)
		for _, line := range lines {
			if width(ansi.ReplaceAllString(line, "")) != cols {
				t.Fatal("node details layout width")
			}
		}
		if !strings.Contains(strings.Join(lines, ""), "Fixture Linux") || !strings.Contains(strings.Join(lines, ""), "Forbidden") {
			t.Fatal("denied gauges hid description")
		}
	}
}

func TestDescribeWorkspace(t *testing.T) {
	a := newApp()
	a.cols, a.rows = 140, 32
	a.applySnapshot(fixture(t))
	v := &viewer{kind: "describe", resource: "pods", target: a.visible[0], title: "Describe · api-1", lines: []string{"Name: api-1", "Conditions:"}, done: true}
	for i := 0; i < 40; i++ {
		v.lines = append(v.lines, "  Ready: True")
	}
	a.view, a.outputs = v, []*viewer{v}
	frame := a.frame()
	if !strings.Contains(frame[0], "File") || !strings.Contains(frame[1], "Describe: api-1") || !strings.Contains(frame[2], "NAMESPACES") || !strings.Contains(frame[len(frame)-1], "Tab resources") {
		t.Fatal("Describe lost workspace navigation")
	}
	v.top = 4
	tabs := a.workspaceTabs()
	a.handle(key{code: keyMouse, mouseX: tabs[0].x + 2, mouseY: 2})
	if a.view != nil || a.outputs[0] != v {
		t.Fatal("resource tab discarded description")
	}
	a.handle(key{code: keyMouse, mouseX: tabs[1].x + 2, mouseY: 2})
	if a.view != v || v.top != 4 {
		t.Fatal("returning to Describe lost scroll position")
	}
	a.handle(key{code: keyTab})
	a.handle(key{code: keyTab})
	if a.view != v {
		t.Fatal("keyboard tab did not return to description")
	}
	a.handle(key{code: keyMouse, mouseX: tabs[1].closeX, mouseY: 2})
	if a.action != "close-output" {
		t.Fatal("Describe close button did not target its output")
	}
}
