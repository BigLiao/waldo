// Package termfilter removes dangerous terminal escape sequences from a byte
// stream coming from B before it reaches the user's terminal.
//
// Passed: plain text, C0 controls, CSI sequences (except title/report
// queries that echo attacker data back as keyboard input), simple ESC
// sequences, and an allow-list of OSC commands (titles, hyperlinks, colors,
// notifications, shell integration).
// Dropped: OSC 52 (clipboard) and every other OSC not on the list, DCS, APC,
// PM and SOS strings (tmux passthrough, kitty graphics, sixel, ...).
package termfilter

import (
	"bytes"
	"io"
	"strconv"
)

type state int

const (
	ground state = iota
	esc
	escInter
	csi
	osc
	oscEsc
	str
	strEsc
)

const (
	maxCSI = 512
	maxOSC = 256 << 10
)

// allowedOSC lists OSC numbers that are passed through.
var allowedOSC = map[int]bool{
	0: true, 1: true, 2: true, // icon/window title
	4:   true,                                  // palette
	7:   true,                                  // cwd report (shell integration)
	8:   true,                                  // hyperlinks
	9:   true,                                  // desktop notification (iTerm2/ConEmu progress)
	10:  true,                                  // fg color
	11:  true,                                  // bg color
	12:  true,                                  // cursor color
	104: true, 110: true, 111: true, 112: true, // color resets
	133: true, // semantic prompts
	777: true, // notifications (rxvt/urxvt)
}

// Filter is an io.Writer that writes filtered output to W.
type Filter struct {
	W     io.Writer
	st    state
	buf   []byte
	out   []byte
	strN  int
	Drops int
}

func New(w io.Writer) *Filter { return &Filter{W: w} }

func (f *Filter) Write(p []byte) (int, error) {
	f.out = f.out[:0]
	for _, c := range p {
		f.step(c)
	}
	if len(f.out) > 0 {
		if _, err := f.W.Write(f.out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (f *Filter) emit(b ...byte) { f.out = append(f.out, b...) }

func (f *Filter) drop() {
	f.Drops++
	f.buf = f.buf[:0]
	f.st = ground
}

func (f *Filter) step(c byte) {
	switch f.st {
	case ground:
		if c == 0x1b {
			f.st = esc
			f.buf = append(f.buf[:0], c)
			return
		}
		f.emit(c)
	case esc:
		switch {
		case c == '[':
			f.st = csi
			f.buf = append(f.buf, c)
		case c == ']':
			f.st = osc
			f.buf = append(f.buf, c)
		case c == 'P' || c == '_' || c == '^' || c == 'X':
			f.st = str
			f.strN = 0
			f.buf = f.buf[:0]
		case c >= 0x20 && c <= 0x2f:
			f.st = escInter
			f.buf = append(f.buf, c)
		case c == 0x1b:
			// ESC ESC: emit the first, restart.
			f.emit(0x1b)
		case c == 0x18 || c == 0x1a:
			f.drop()
		default:
			f.emit(0x1b, c)
			f.st = ground
			f.buf = f.buf[:0]
		}
	case escInter:
		f.buf = append(f.buf, c)
		if c >= 0x30 && c <= 0x7e {
			f.emit(f.buf...)
			f.buf = f.buf[:0]
			f.st = ground
		} else if c < 0x20 || len(f.buf) > 16 {
			f.drop()
		}
	case csi:
		if c == 0x18 || c == 0x1a {
			f.drop()
			return
		}
		f.buf = append(f.buf, c)
		if c >= 0x40 && c <= 0x7e {
			if f.csiAllowed(c) {
				f.emit(f.buf...)
				f.buf = f.buf[:0]
				f.st = ground
			} else {
				f.drop()
			}
			return
		}
		if len(f.buf) > maxCSI {
			f.drop()
		}
	case osc:
		switch c {
		case 0x07:
			f.finishOSC([]byte{0x07})
		case 0x1b:
			f.st = oscEsc
		case 0x18, 0x1a:
			f.drop()
		default:
			f.buf = append(f.buf, c)
			if len(f.buf) > maxOSC {
				f.drop()
			}
		}
	case oscEsc:
		if c == '\\' {
			f.finishOSC([]byte{0x1b, '\\'})
			return
		}
		// Malformed: abandon the OSC, treat this ESC as a new sequence start.
		f.drop()
		f.st = esc
		f.buf = append(f.buf[:0], 0x1b)
		f.step(c)
	case str:
		switch c {
		case 0x1b:
			f.st = strEsc
		case 0x18, 0x1a:
			f.drop()
		default:
			f.strN++
			if f.strN > maxOSC*4 {
				f.drop()
			}
		}
	case strEsc:
		if c == '\\' {
			f.drop()
			return
		}
		f.st = str
	}
}

func (f *Filter) csiAllowed(final byte) bool {
	params := f.buf[2 : len(f.buf)-1]
	if final == 't' {
		// Window manipulation: 20/21 report the icon label / title back as input.
		first, _, _ := bytes.Cut(params, []byte{';'})
		n, err := strconv.Atoi(string(first))
		if err != nil || n == 20 || n == 21 {
			return false
		}
	}
	return true
}

func (f *Filter) finishOSC(term []byte) {
	body := f.buf[2:]
	num, _, _ := bytes.Cut(body, []byte{';'})
	n, err := strconv.Atoi(string(num))
	if err == nil && allowedOSC[n] {
		f.emit(f.buf...)
		f.emit(term...)
		f.buf = f.buf[:0]
		f.st = ground
		return
	}
	f.drop()
}

// Sanitize makes a short string from B safe to print: every escape sequence
// (7-bit or C1) is removed entirely and every control character except
// newline and tab is dropped.
func Sanitize(s string) string {
	rs := []rune(s)
	out := make([]rune, 0, len(rs))
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		var kind rune // '[' CSI, ']' string until BEL/ST, 0 none
		switch {
		case r == 0x1b && i+1 < len(rs):
			i++
			switch rs[i] {
			case '[':
				kind = '['
			case ']', 'P', '_', '^', 'X':
				kind = ']'
			}
		case r == 0x9b:
			kind = '['
		case r == 0x9d || r == 0x90 || r == 0x9f || r == 0x9e || r == 0x98:
			kind = ']'
		case r == '\n' || r == '\t' || (r >= 0x20 && r != 0x7f && !(r >= 0x80 && r <= 0x9f)):
			out = append(out, r)
		}
		switch kind {
		case '[':
			for i+1 < len(rs) && !(rs[i+1] >= 0x40 && rs[i+1] <= 0x7e) {
				i++
			}
			i++
		case ']':
			for i+1 < len(rs) {
				i++
				if rs[i] == 0x07 || rs[i] == 0x9c || (rs[i] == 0x1b && i+1 < len(rs) && rs[i+1] == '\\') {
					if rs[i] == 0x1b {
						i++
					}
					break
				}
			}
		}
	}
	return string(out)
}
