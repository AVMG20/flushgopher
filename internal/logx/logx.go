// Package logx is a tiny leveled logger matching the verbosity levels of the
// original cache-clean.js: -s silent, default notice, -v info, -vv debug.
package logx

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

type Level int

const (
	Always Level = -1
	Error  Level = 0
	Notice Level = 1
	Info   Level = 2
	Debug  Level = 3
)

var (
	mu        sync.Mutex
	verbosity           = Notice
	out       io.Writer = os.Stdout
	errOut    io.Writer = os.Stderr
	// RawMode is set while the terminal is in raw mode (hotkeys), where
	// newlines need an explicit carriage return.
	RawMode bool
)

func SetVerbosity(l Level) { mu.Lock(); verbosity = l; mu.Unlock() }
func Verbosity() Level     { mu.Lock(); defer mu.Unlock(); return verbosity }
func Enabled(l Level) bool { return Verbosity() >= l }

func write(l Level, withTime bool, format string, args ...any) {
	if !Enabled(l) {
		return
	}
	msg := fmt.Sprintf(format, args...)
	if withTime {
		msg = Dim(time.Now().Format("15:04:05")) + " " + msg
	}
	msg = strings.TrimRight(msg, "\n") + "\n"
	mu.Lock()
	defer mu.Unlock()
	if RawMode {
		msg = strings.ReplaceAll(msg, "\n", "\r\n")
	}
	w := out
	if l == Error {
		w = errOut
	}
	io.WriteString(w, msg)
}

func Debugf(f string, a ...any) { write(Debug, true, f, a...) }
func Errorf(f string, a ...any) { write(Error, true, "%s", Paint(CErr, fmt.Sprintf(f, a...))) }

// Plain variants print without a timestamp.
func PlainNotice(f string, a ...any) { write(Notice, false, f, a...) }
func PlainAlways(f string, a ...any) { write(Always, false, f, a...) }
