package main

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type key struct {
	raw            string
	release        bool
	alt            bool
	mouseX, mouseY int
	scroll         int
	code           int
	r              rune
}

const (
	keyUp = iota + 1
	keyDown
	keyLeft
	keyRight
	keyEnter
	keyBackspace
	keyEscape
	keyTab
	keyPageUp
	keyPageDown
	keyMouse
	keyResources
	keyLogs
	keyFlow
	keyEventType
	keyQuit
	keySave
	keyRefresh
	keySidebar
	keyHelp
)

type inputDecoder struct{ pending []byte }

func (d *inputDecoder) feed(data []byte, emit func(key)) {
	d.pending = append(d.pending, data...)
	for len(d.pending) > 0 {
		b := d.pending[0]
		if b == 27 {
			if len(d.pending) == 1 {
				return
			}
			if d.pending[1] == '[' {
				end := 2
				for end < len(d.pending) && (d.pending[end] < 0x40 || d.pending[end] > 0x7e) {
					end++
				}
				if end == len(d.pending) {
					return
				}
				codes := map[string]int{"A": keyUp, "B": keyDown, "C": keyRight, "D": keyLeft, "5~": keyPageUp, "6~": keyPageDown}
				sequence := string(d.pending[2 : end+1])
				if code := codes[sequence]; code != 0 {
					emit(key{code: code})
				} else if !strings.HasPrefix(sequence, "<") {
					emit(key{raw: string(d.pending[:end+1])})
				}
				if strings.HasPrefix(sequence, "<") && (strings.HasSuffix(sequence, "M") || strings.HasSuffix(sequence, "m")) {
					parts := strings.Split(strings.TrimRight(strings.TrimPrefix(sequence, "<"), "Mm"), ";")
					if len(parts) == 3 {
						button, eb := strconv.Atoi(parts[0])
						x, ex := strconv.Atoi(parts[1])
						y, ey := strconv.Atoi(parts[2])
						if eb == nil && ex == nil && ey == nil && x > 0 && y > 0 && (button == 0 || button == 64 || button == 65) {
							k := key{code: keyMouse, mouseX: x, mouseY: y, release: strings.HasSuffix(sequence, "m")}
							if button >= 64 {
								k.scroll = (button-64)*2 - 1
							}
							emit(k)
						}
					}
				}
				d.pending = d.pending[end+1:]
				continue
			}
			if d.pending[1] == 'O' {
				if len(d.pending) < 3 {
					return
				}
				emit(key{raw: string(d.pending[:3])})
				d.pending = d.pending[3:]
				continue
			}
			if d.pending[1] >= 32 && d.pending[1] < 127 {
				emit(key{r: rune(d.pending[1]), alt: true})
				d.pending = d.pending[2:]
				continue
			}
			emit(key{code: keyEscape})
			d.pending = d.pending[1:]
			continue
		}
		switch b {
		case '\r', '\n':
			emit(key{code: keyEnter})
			d.pending = d.pending[1:]
			continue
		case '\t':
			emit(key{code: keyTab})
			d.pending = d.pending[1:]
			continue
		case 8, 127:
			emit(key{code: keyBackspace})
			d.pending = d.pending[1:]
			continue
		}
		if !utf8.FullRune(d.pending) {
			return
		}
		r, n := utf8.DecodeRune(d.pending)
		emit(key{r: r})
		d.pending = d.pending[n:]
	}
}

func readInput(ctx context.Context) <-chan key {
	keys := make(chan key, 128)
	go func() {
		defer close(keys)
		var decoder inputDecoder
		var buffer [256]byte
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			n, err := os.Stdin.Read(buffer[:])
			if n > 0 {
				decoder.feed(buffer[:n], func(k key) {
					select {
					case keys <- k:
					case <-ctx.Done():
					}
				})
			}
			if err != nil && err != io.EOF {
				return
			}
			if n == 0 {
				if len(decoder.pending) == 1 && decoder.pending[0] == 27 {
					decoder.pending = nil
					select {
					case keys <- key{code: keyEscape}:
					case <-ctx.Done():
						return
					}
				}
				select {
				case <-ctx.Done():
					return
				default:
					time.Sleep(time.Millisecond)
				}
			}
		}
	}()
	return keys
}
