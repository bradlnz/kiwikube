package main

// Adapted from Kiwicode’s libvterm/PTY terminal, shared behavior for embedded shells.

/*
#cgo pkg-config: vterm
#cgo LDFLAGS: -lutil
#include <vterm.h>
#include <pty.h>
#include <unistd.h>
#include <fcntl.h>
#include <string.h>

// ponytail: history is bounded and retains original column widths; add reflow if needed.
#define HISTORY 1000
typedef struct { VTermScreenCell *cells; int cols; } HistoryLine;
typedef struct {
  VTerm *vt;
  VTermScreen *screen;
  int visible, alternate, mouse, start, count;
  HistoryLine history[HISTORY];
} Terminal;
typedef struct {
  uint32_t chars[VTERM_MAX_CHARS_PER_CELL];
  uint32_t fg, bg;
  int width, attrs;
} TerminalCell;

static int term_property(VTermProp prop, VTermValue *value, void *user) {
  Terminal *t = user;
  if(prop == VTERM_PROP_CURSORVISIBLE) t->visible = value->boolean;
  if(prop == VTERM_PROP_ALTSCREEN) t->alternate = value->boolean;
  if(prop == VTERM_PROP_MOUSE) t->mouse = value->number;
  return 1;
}
static int term_history(int cols, const VTermScreenCell *cells, void *user) {
  Terminal *t = user;
  VTermScreenCell *copy = malloc(cols * sizeof(*copy));
  if(!copy) return 0;
  memcpy(copy, cells, cols * sizeof(*copy));
  int index = (t->start + t->count) % HISTORY;
  if(t->count == HISTORY) { free(t->history[index].cells); t->start = (t->start+1)%HISTORY; }
  else t->count++;
  t->history[index] = (HistoryLine){copy, cols};
  return 1;
}
static int term_clear_history(void *user) {
  Terminal *t = user;
  for(int i=0; i<HISTORY; i++) { free(t->history[i].cells); t->history[i] = (HistoryLine){0}; }
  t->start = t->count = 0;
  return 1;
}
static const VTermScreenCallbacks term_callbacks = {
  .settermprop = term_property, .sb_pushline = term_history, .sb_clear = term_clear_history
};
static Terminal *term_new(int rows, int cols) {
  Terminal *t = calloc(1, sizeof(*t));
  if(!t) return NULL;
  t->vt = vterm_new(rows, cols);
  if(!t->vt) { free(t); return NULL; }
  vterm_set_utf8(t->vt, 1);
  t->screen = vterm_obtain_screen(t->vt);
  t->visible = 1;
  vterm_screen_set_callbacks(t->screen, &term_callbacks, t);
  vterm_screen_enable_altscreen(t->screen, 1);
  vterm_screen_reset(t->screen, 1);
  return t;
}
static void term_free(Terminal *t) {
  term_clear_history(t); vterm_free(t->vt); free(t);
}
static uint32_t term_color(Terminal *t, VTermColor color) {
  if(color.type & VTERM_COLOR_DEFAULT_MASK) return 0x1000000;
  vterm_screen_convert_color_to_rgb(t->screen, &color);
  return (color.rgb.red<<16) | (color.rgb.green<<8) | color.rgb.blue;
}
static void term_snapshot(Terminal *t, TerminalCell *out, int rows, int cols, int scroll) {
  for(int row=0; row<rows; row++) for(int col=0; col<cols; col++) {
    VTermScreenCell cell = {0};
    int line = t->count-scroll+row;
    if(line < t->count) {
      HistoryLine *history = &t->history[(t->start+line)%HISTORY];
      if(col < history->cols) cell = history->cells[col];
      else { cell.fg.type = VTERM_COLOR_DEFAULT_FG; cell.bg.type = VTERM_COLOR_DEFAULT_BG; }
    } else vterm_screen_get_cell(t->screen, (VTermPos){row-scroll,col}, &cell);
    TerminalCell *dest = &out[row*cols+col];
    memcpy(dest->chars, cell.chars, sizeof(dest->chars));
    dest->width = cell.width;
    dest->fg = term_color(t,cell.fg); dest->bg = term_color(t,cell.bg);
    dest->attrs = cell.attrs.bold | (cell.attrs.underline ? 2:0) | (cell.attrs.italic<<2) |
      (cell.attrs.reverse<<3) | (cell.attrs.strike<<4) | (cell.attrs.conceal<<5);
  }
}
static int term_openpty(int *master, int *slave, int rows, int cols) {
  struct winsize size = {.ws_row=rows, .ws_col=cols};
  if(openpty(master,slave,NULL,NULL,&size) < 0) return -1;
  fcntl(*master,F_SETFD,FD_CLOEXEC); fcntl(*slave,F_SETFD,FD_CLOEXEC);
  return 0;
}
static int term_resize(int fd, int rows, int cols) {
  struct winsize size = {.ws_row=rows, .ws_col=cols};
  return ioctl(fd,TIOCSWINSZ,&size);
}
*/
import "C"

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"unsafe"
)

type terminalEvent struct {
	data []byte
	err  error
}

type interactiveTerminal struct {
	native                   *C.Terminal
	pty                      *os.File
	cmd                      *exec.Cmd
	events                   chan terminalEvent
	writes                   chan []byte
	closed                   chan struct{}
	queued                   atomic.Int64
	rows, cols, x, y, scroll int
	ended                    bool
	cells                    []C.TerminalCell
}

func newInteractiveTerminal(cmd *exec.Cmd, rows, cols int) (*interactiveTerminal, error) {
	rows, cols = max(1, rows), max(1, cols)
	var master, slave C.int
	syscall.ForkLock.Lock()
	result, openErr := C.term_openpty(&master, &slave, C.int(rows), C.int(cols))
	syscall.ForkLock.Unlock()
	if result < 0 {
		return nil, fmt.Errorf("open terminal: %w", openErr)
	}
	if err := syscall.SetNonblock(int(master), true); err != nil {
		syscall.Close(int(master))
		syscall.Close(int(slave))
		return nil, err
	}
	pty, tty := os.NewFile(uintptr(master), "pty"), os.NewFile(uintptr(slave), "tty")
	defer tty.Close()
	native := C.term_new(C.int(rows), C.int(cols))
	if native == nil {
		pty.Close()
		return nil, fmt.Errorf("allocate terminal screen")
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.Env = append(cmd.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		pty.Close()
		C.term_free(native)
		return nil, err
	}
	t := &interactiveTerminal{native: native, pty: pty, cmd: cmd, rows: rows, cols: cols, events: make(chan terminalEvent, 32), writes: make(chan []byte, 256), closed: make(chan struct{})}
	events := t.events
	go func() {
		for {
			data := make([]byte, 16384)
			n, err := pty.Read(data)
			if n > 0 {
				select {
				case events <- terminalEvent{data: data[:n]}:
				case <-t.closed:
					_ = cmd.Wait()
					return
				}
			}
			if err != nil {
				break
			}
		}
		err := cmd.Wait()
		select {
		case events <- terminalEvent{err: err}:
		case <-t.closed:
		}
	}()
	go func() {
		for {
			select {
			case data := <-t.writes:
				_, err := pty.Write(data)
				t.queued.Add(-int64(len(data)))
				if err != nil {
					select {
					case events <- terminalEvent{err: fmt.Errorf("terminal input: %w", err)}:
					case <-t.closed:
					}
					return
				}
			case <-t.closed:
				return
			}
		}
	}()
	return t, nil
}

func (t *interactiveTerminal) close() {
	if t == nil || t.native == nil {
		return
	}
	close(t.closed)
	_ = t.pty.Close() // Hang up the foreground job as well as its controlling shell.
	_ = t.cmd.Process.Kill()
	C.term_free(t.native)
	t.native = nil
}

func (t *interactiveTerminal) send(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if t.ended {
		return fmt.Errorf("shell exited; close and reopen Terminal")
	}
	if t.queued.Add(int64(len(data))) <= (2<<20)+32 {
		select {
		case t.writes <- data:
			return nil
		default:
		}
	}
	t.queued.Add(-int64(len(data)))
	return fmt.Errorf("terminal input queue full; wait for the program to read input")
}

func (t *interactiveTerminal) output() []byte {
	data := make([]byte, 4096)
	n := C.vterm_output_read(t.native.vt, (*C.char)(unsafe.Pointer(&data[0])), C.size_t(len(data)))
	return data[:int(n)]
}

func (a *app) shellEvents() <-chan terminalEvent {
	if a.shell == nil {
		return nil
	}
	return a.shell.events
}
func (t *interactiveTerminal) receive(event terminalEvent) error {
	if len(event.data) == 0 {
		t.ended = true
		return event.err
	}
	C.vterm_input_write(t.native.vt, (*C.char)(unsafe.Pointer(&event.data[0])), C.size_t(len(event.data)))
	C.vterm_screen_flush_damage(t.native.screen)
	return t.send(t.output())
}
func (t *interactiveTerminal) handle(k key) error {
	t.scroll = 0
	if k.raw != "" {
		return t.send([]byte(k.raw))
	}
	mod := C.VTermModifier(C.VTERM_MOD_NONE)
	if k.alt {
		mod = C.VTERM_MOD_ALT
	}
	keys := map[int]C.VTermKey{keyEscape: C.VTERM_KEY_ESCAPE, keyUp: C.VTERM_KEY_UP, keyDown: C.VTERM_KEY_DOWN, keyLeft: C.VTERM_KEY_LEFT, keyRight: C.VTERM_KEY_RIGHT, keyPageUp: C.VTERM_KEY_PAGEUP, keyPageDown: C.VTERM_KEY_PAGEDOWN, keyEnter: C.VTERM_KEY_ENTER, keyBackspace: C.VTERM_KEY_BACKSPACE, keyTab: C.VTERM_KEY_TAB}
	if k.r != 0 || k.code == 0 {
		C.vterm_keyboard_unichar(t.native.vt, C.uint32_t(k.r), mod)
	} else if code, ok := keys[k.code]; ok {
		C.vterm_keyboard_key(t.native.vt, code, mod)
	}
	return t.send(t.output())
}
func (t *interactiveTerminal) mouse(k key) error {
	if t.native.mouse == 0 {
		if t.native.alternate == 0 {
			if k.scroll < 0 {
				t.scroll = min(int(t.native.count), t.scroll+3)
			}
			if k.scroll > 0 {
				t.scroll = max(0, t.scroll-3)
			}
		}
		return nil
	}
	C.vterm_mouse_move(t.native.vt, C.int(k.mouseY-t.y), C.int(k.mouseX-t.x), C.VTERM_MOD_NONE)
	button := 1
	if k.scroll < 0 {
		button = 4
	} else if k.scroll > 0 {
		button = 5
	}
	C.vterm_mouse_button(t.native.vt, C.int(button), C.bool(!k.release), C.VTERM_MOD_NONE)
	if button >= 4 {
		C.vterm_mouse_button(t.native.vt, C.int(button), false, C.VTERM_MOD_NONE)
	}

	return t.send(t.output())
}

func (t *interactiveTerminal) draw(out *strings.Builder, x, y, cols, rows int) (int, int) {
	t.x, t.y = x, y
	if t.rows != rows || t.cols != cols {
		t.rows, t.cols = rows, cols
		C.vterm_set_size(t.native.vt, C.int(rows), C.int(cols))
		C.term_resize(C.int(t.pty.Fd()), C.int(rows), C.int(cols))
	}
	t.scroll = min(t.scroll, int(t.native.count))
	if t.native.alternate != 0 {
		t.scroll = 0
	}
	if len(t.cells) != rows*cols {
		t.cells = make([]C.TerminalCell, rows*cols)
	}
	C.term_snapshot(t.native, &t.cells[0], C.int(rows), C.int(cols), C.int(t.scroll))
	for row := 0; row < rows; row++ {
		fmt.Fprintf(out, "\x1b[%d;%dH", y+row, x)
		var previous C.TerminalCell
		for col := 0; col < cols; col++ {
			cell := t.cells[row*cols+col]
			if cell.chars[0] == 0xffffffff {
				continue
			} // Second cell of a wide glyph.
			if col == 0 || cell.fg != previous.fg || cell.bg != previous.bg || cell.attrs != previous.attrs {
				out.WriteString("\x1b[0m")
				for _, color := range []struct {
					value C.uint32_t
					code  int
				}{{cell.fg, 38}, {cell.bg, 48}} {
					if color.value&0x1000000 == 0 {
						fmt.Fprintf(out, "\x1b[%d;2;%d;%d;%dm", color.code, color.value>>16, (color.value>>8)&255, color.value&255)
					}
				}
				for bit, attr := range []int{1, 4, 3, 7, 9, 8} {
					if int(cell.attrs)&(1<<bit) != 0 {
						fmt.Fprintf(out, "\x1b[%dm", attr)
					}
				}
				previous = cell
			}
			if cell.chars[0] == 0 || col+int(cell.width) > cols {
				out.WriteByte(' ')
			} else {
				for _, r := range cell.chars {
					if r == 0 {
						break
					}
					out.WriteRune(rune(r))
				}
			}
		}
	}
	out.WriteString("\x1b[0m")
	if t.native.visible == 0 || t.scroll > 0 || t.ended {
		return 0, 0
	}
	var cursor C.VTermPos
	C.vterm_state_get_cursorpos(C.vterm_obtain_state(t.native.vt), &cursor)
	return y + int(cursor.row), x + int(cursor.col)
}
