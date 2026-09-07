package main

import (
	"strings"
)

type describeSection struct {
	title string
	lines []string
}

func describeField(line string) (key, value string, ok bool) {
	key, value, ok = strings.Cut(strings.TrimSpace(line), ":")
	if !ok || key == "" || (value != "" && value[0] != ' ' && value[0] != '\t') {
		return "", "", false
	}
	return key, strings.TrimSpace(value), true
}
func describeSections(raw []string) []describeSection {
	sections := []describeSection{{title: "Overview"}}
	for i, line := range raw {
		line = safeText(strings.ReplaceAll(line, "\t", "    "))
		key, value, field := describeField(line)
		top := line != "" && line[0] != ' '
		nested := i+1 < len(raw) && (strings.HasPrefix(raw[i+1], " ") || strings.HasPrefix(raw[i+1], "\t")) && strings.TrimSpace(raw[i+1]) != ""
		if top && field && (value == "" || nested) {
			sections = append(sections, describeSection{title: key})
			if value != "" {
				sections[len(sections)-1].lines = append(sections[len(sections)-1].lines, value)
			}
			continue
		}
		if top && field && sections[len(sections)-1].title != "Overview" && sections[len(sections)-1].title != "Details" {
			sections = append(sections, describeSection{title: "Details"})
		}
		sections[len(sections)-1].lines = append(sections[len(sections)-1].lines, line)
	}
	return sections
}

// Wrap rather than truncate so commands, annotations and table cells remain readable.
func describeWrap(text string, cols int) []string {
	cols = max(1, cols)
	var lines []string
	for width(text) > cols {
		end, cells, space := 0, 0, -1
		for i, r := range text {
			if cells+runeWidth(r) > cols {
				end = i
				break
			}
			cells += runeWidth(r)
			if r == ' ' {
				space = i
			}
		}
		if space > 0 && width(text[:space]) >= cols/2 {
			end = space
		}
		if end == 0 { // A wide rune in a one-cell column still has to make progress.
			for i := range text {
				if i > 0 {
					end = i
					break
				}
			}
			if end == 0 {
				end = len(text)
			}
		}
		lines = append(lines, text[:end])
		text = strings.TrimLeft(text[end:], " ")
	}
	return append(lines, text)
}
func prettyDescribe(raw []string, query string, cols int, p palette) []string {
	var rendered []string
	inner := max(1, cols-4)
	query = strings.ToLower(query)
	for _, section := range describeSections(raw) {
		var body []string
		for _, line := range section.lines {
			if strings.TrimSpace(line) == "" {
				continue
			}
			if query == "" || strings.Contains(strings.ToLower(section.title), query) || strings.Contains(strings.ToLower(line), query) {
				body = append(body, line)
			}
		}
		if len(body) == 0 {
			continue
		}
		title := " " + section.title + " "
		rendered = append(rendered, p.accent+"\x1b[1m"+" ╭"+fit(title, cols-4)+"╮ "+"\x1b[22m")
		add := func(content string) { rendered = append(rendered, p.muted+" │"+content+p.muted+"│ ") }
		for row, line := range body {
			key, value, field := describeField(line)
			indent := len(line) - len(strings.TrimLeft(line, " "))
			if field && value != "" && width(key)+min(indent, 8) < inner/2 {
				key = strings.Repeat(" ", min(indent, 8)) + key
				keyWidth := min(max(18, width(key)), inner/2)
				parts := describeWrap(value, inner-keyWidth-2)
				for i, part := range parts {
					label := key
					if i > 0 {
						label = ""
					}
					style := p.base
					switch strings.ToLower(value) {
					case "running", "ready", "bound", "complete":
						style = "\x1b[48;5;234;38;5;114m"
					case "failed", "notready", "crashloopbackoff", "error":
						style = p.bad
					case "pending", "warning", "unknown":
						style = p.warn
					}
					if strings.EqualFold(key, "Ready") {
						if strings.EqualFold(value, "true") {
							style = "\x1b[48;5;234;38;5;114m"
						} else if strings.EqualFold(value, "false") {
							style = p.bad
						}
					}
					if strings.HasSuffix(strings.TrimSpace(key), "Pressure") && strings.EqualFold(value, "true") {
						style = p.bad
					}
					add(p.accent + fit(label, keyWidth) + p.muted + "  " + style + fit(part, inner-keyWidth-2))
				}
			} else {
				style := p.base
				if row+1 < len(body) && strings.TrimSpace(body[row+1]) != "" && strings.Trim(body[row+1], " -") == "" {
					style = p.header
				}
				if field && value == "" {
					style = p.accent + "\x1b[1m"
				}
				if strings.Contains(line, "Warning") || strings.Contains(line, "NotReady") {
					style = p.warn
				}
				if strings.TrimFunc(strings.TrimSpace(line), func(r rune) bool { return r == '-' || r == ' ' }) == "" {
					style = p.muted
				}
				for _, part := range describeWrap(line, inner) {
					add(style + fit(part, inner) + "\x1b[22m")
				}
			}
		}
		rendered = append(rendered, p.muted+" ╰"+strings.Repeat("─", cols-4)+"╯ ", p.base+fit("", cols))
	}
	if len(rendered) == 0 {
		rendered = []string{p.muted + fit(" No matching details", cols)}
	}
	return rendered
}
