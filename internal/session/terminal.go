package session

import (
	"fmt"
	"io"

	"waldo/internal/transport"
)

// term.Restore only restores the kernel's tty attributes. The terminal
// emulator also needs to leave the modes the agent may have enabled: after
// a broken connection the agent cannot send its own cleanup sequences.
func resetAgentTerminal(out io.Writer) {
	io.WriteString(out,
		"\x1b[?2026l"+ // finish synchronized output before restoring the screen
			"\x1b[?9l\x1b[?1000l\x1b[?1001l\x1b[?1002l\x1b[?1003l"+ // mouse tracking
			"\x1b[?1004l\x1b[?1005l\x1b[?1006l\x1b[?1015l\x1b[?1016l"+ // focus and mouse encodings
			"\x1b[?2004l"+ // bracketed paste
			"\x1b[?1049l\x1b[0m\x1b[?25h") // normal screen, attributes and visible cursor
}

// reconnectNotice draws a link state notice on the bottom row without moving
// the cursor or scrolling, so the agent's full-screen layout stays intact.
// Recovery clears the row; the caller then asks the agent to repaint.
func reconnectNotice(rows, cols int, up bool) string {
	text := ""
	if !up {
		text = truncateCells(" waldo: "+transport.ReconnectMessage(false)+" ", cols)
		text = "\x1b[7m" + text + "\x1b[0m" // reverse video
	}
	return fmt.Sprintf("\x1b7\x1b[%d;1H\x1b[2K%s\x1b8", rows, text)
}

// truncateCells keeps s within n terminal cells, counting CJK as two.
func truncateCells(s string, n int) string {
	w := 0
	for i, r := range s {
		rw := 1
		if r >= 0x1100 && (r <= 0x115f || (r >= 0x2e80 && r <= 0xa4cf) || (r >= 0xac00 && r <= 0xd7a3) || (r >= 0xf900 && r <= 0xfaff) || (r >= 0xfe30 && r <= 0xfe4f) || (r >= 0xff00 && r <= 0xff60) || (r >= 0xffe0 && r <= 0xffe6)) {
			rw = 2
		}
		if w+rw > n-1 { // never write the last cell: some terminals wrap there
			return s[:i]
		}
		w += rw
	}
	return s
}
