package logx

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// Color output is enabled for terminals unless NO_COLOR is set.
var Color = os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" && term.IsTerminal(int(os.Stdout.Fd()))

// 256-color palette.
const (
	CGopher   = 45  // gopher cyan
	CType     = 81  // cache types
	CID       = 177 // cache ids
	CGen      = 214 // generated code / files removed
	CWatch    = 111 // watching / modules
	CStorm    = 208 // mass change
	COK       = 114 // done
	CDim      = 244
	CKey      = 220 // hotkey letters
	CErr      = 203
	CFlushAll = 197
)

func fg(c int, s string) string {
	if !Color {
		return s
	}
	return fmt.Sprintf("\033[38;5;%dm%s\033[0m", c, s)
}

// Paint colors s with a 256-color code.
func Paint(c int, s string) string { return fg(c, s) }

func Bold(s string) string {
	if !Color {
		return s
	}
	return "\033[1m" + s + "\033[0m"
}

func Dim(s string) string { return fg(CDim, s) }

// Badge renders a fixed width, colored label such as "CLEAN ".
func Badge(c int, label string) string {
	l := fmt.Sprintf("%-6s", label)
	if !Color {
		return l
	}
	return fmt.Sprintf("\033[1;38;5;%dm%s\033[0m", c, l)
}

// Chips renders items as colored, space separated words.
func Chips(c int, items []string) string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = fg(c, s)
	}
	return strings.Join(out, Dim(" · "))
}

// Key renders a hotkey label like "[c]onfig" with the letter highlighted.
func Key(key, rest string) string {
	if !Color {
		return "[" + key + "]" + rest
	}
	return Dim("[") + "\033[1;38;5;" + fmt.Sprint(CKey) + "m" + key + "\033[0m" + Dim("]") + rest
}

// Event logs a timestamped line with a colored badge.
func Event(l Level, badgeColor int, badge, format string, args ...any) {
	if !Enabled(l) {
		return
	}
	write(l, true, Badge(badgeColor, badge)+" "+format, args...)
}
