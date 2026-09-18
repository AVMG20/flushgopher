// Package hotkeys reads single key presses from the terminal.
package hotkeys

import (
	"os"

	"golang.org/x/term"

	"flushgopher/internal/logx"
)

const CtrlC = 3

// Start puts stdin in raw mode and calls handle for every key pressed.
// It returns a restore func, or ok=false if stdin is not a terminal.
func Start(handle func(key byte)) (restore func(), ok bool) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return func() {}, false
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		logx.Errorf("Error initializing hotkey support: %v", err)
		return func() {}, false
	}
	logx.RawMode = true
	restore = func() {
		term.Restore(fd, state)
		logx.RawMode = false
	}
	go func() {
		buf := make([]byte, 16)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				return
			}
			for _, b := range buf[:n] {
				handle(b)
			}
		}
	}()
	return restore, true
}
