package watch

import (
	"os"
	"syscall"
)

func inode(p string) uint64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return uint64(s.Ino)
	}
	return 1
}
