package main

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestEmbeddedShell(t *testing.T) {
	cmd := exec.Command("/bin/bash", "--noprofile", "--norc", "-i")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "PS1=kiwi> ")
	term, err := newInteractiveTerminal(cmd, 12, 80)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(term.close)
	a := newApp()
	a.rows, a.cols, a.shell = 30, 100, term
	frame := func() string {
		var out strings.Builder
		term.draw(&out, 4, 3, term.cols, term.rows)
		return out.String()
	}
	wait := func(want string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for !strings.Contains(frame(), want) {
			select {
			case event := <-term.events:
				if err := term.receive(event); err != nil {
					t.Fatal(err)
				}
			case <-deadline:
				t.Fatalf("missing %q in %q", want, frame())
			}
		}
	}
	send := func(command string) {
		t.Helper()
		if err := term.send([]byte(command + "\r")); err != nil {
			t.Fatal(err)
		}
	}
	wait("kiwi>")
	captureDraw := func() string {
		t.Helper()
		file, err := os.CreateTemp(t.TempDir(), "frame")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		saved := os.Stdout
		func() { defer func() { os.Stdout = saved }(); os.Stdout = file; a.draw() }()
		if _, err := file.Seek(0, 0); err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(file)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if !strings.Contains(captureDraw(), "NAMESPACES") {
		t.Fatal("initial modal background missing")
	}
	a.status = "background refresh while typing"
	term.receive(terminalEvent{data: []byte("typed text")})
	repaint := captureDraw()
	if strings.Contains(repaint, "NAMESPACES") || strings.Contains(repaint, a.status) || a.previousFrame == nil {
		t.Fatal("typing repainted the dashboard over the shell")
	}
	a.cols = 110
	a.previousFrame = nil // Same invalidation as SIGWINCH.
	if !strings.Contains(captureDraw(), "NAMESPACES") {
		t.Fatal("resize did not restore modal background")
	}
	if strings.Contains(captureDraw(), "NAMESPACES") {
		t.Fatal("resize left the background invalidated")
	}
	a.shell = nil
	a.previousFrame = nil // Same invalidation as closeShell.
	if !strings.Contains(captureDraw(), a.status) {
		t.Fatal("closing modal did not restore current dashboard")
	}
	a.shell = term
	send("test -t 0 && printf '\\124\\124\\131\\137\\122\\105\\101\\104\\131\\n'")
	wait("TTY_READY")
	send("sleep 30")
	time.Sleep(50 * time.Millisecond)
	a.handle(key{r: 3})
	if a.shell == nil || a.action != "" {
		t.Fatal("Ctrl+C escaped shell modal")
	}
	var fresh strings.Builder
	deadline := time.After(5 * time.Second)
	for !strings.Contains(fresh.String(), "kiwi> ") {
		select {
		case event := <-term.events:
			fresh.Write(event.data)
			term.receive(event)
		case <-deadline:
			t.Fatal("Ctrl+C did not interrupt foreground job")
		}
	}
	var resized strings.Builder
	term.draw(&resized, 4, 3, 70, 10)
	send("stty size")
	wait("10 70")
	send("for i in {1..30}; do printf 'history_%s\\n' \"$i\"; done")
	wait("history_30")
	term.mouse(key{scroll: -1})
	if term.scroll != 3 {
		t.Fatal("missing scrollback")
	}
	term.scroll = 0
	before := frame()
	term.receive(terminalEvent{data: []byte("\x1b[?1049h\x1b[2J\x1b[2;5H\x1b[31mMODAL_TUI\x1b[?25l")})
	var full strings.Builder
	row, col := term.draw(&full, 4, 3, 70, 10)
	if !strings.Contains(full.String(), "MODAL_TUI") || row != 0 || col != 0 || strings.Contains(full.String(), "\x1b[2J") {
		t.Fatal("terminal emulator did not contain TUI output")
	}
	term.receive(terminalEvent{data: []byte("\x1b[?1049l\x1b[?25h")})
	if frame() != before {
		t.Fatal("alternate screen did not restore shell")
	}
	a.handle(key{r: 29})
	if a.action != "close-shell" {
		t.Fatal("Ctrl+] did not close modal")
	}
}
func TestTerminalRawKeys(t *testing.T) {
	for _, sequence := range []string{"\x1bOP", "\x1b[1;5A", "\x1b[Z", "\x1b[200~"} {
		var decoder inputDecoder
		var keys []key
		for _, b := range []byte(sequence) {
			decoder.feed([]byte{b}, func(k key) { keys = append(keys, k) })
		}
		if len(keys) != 1 || keys[0].raw != sequence {
			t.Fatalf("lost terminal sequence %q: %+v", sequence, keys)
		}
	}
}
