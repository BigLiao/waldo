package session

import "io"

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
