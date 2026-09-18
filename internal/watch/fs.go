package watch

import (
	"sync"

	"github.com/syncthing/notify"
)

// fsWatches manages one notify channel per watched path, so single paths can
// be dropped and re-added (notify can only stop watches per channel). All
// events are funnelled into out.
type fsWatches struct {
	mu    sync.Mutex
	out   chan string
	paths map[string]*fsWatch
}

type fsWatch struct {
	ch        chan notify.EventInfo
	ino       uint64
	recursive bool
	done      chan struct{}
}

func newFSWatches(out chan string) *fsWatches {
	return &fsWatches{out: out, paths: map[string]*fsWatch{}}
}

// sync makes the set of watched paths equal to want (path → recursive).
// Paths whose directory was replaced (different inode) are re-watched.
// It returns the number of newly added watches and the first error.
func (w *fsWatches) sync(want map[string]bool, buf int) (added int, firstErr error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for p, fw := range w.paths {
		rec, ok := want[p]
		if !ok || rec != fw.recursive || inode(p) != fw.ino {
			w.stop(p, fw)
		}
	}
	for p, rec := range want {
		if _, ok := w.paths[p]; ok {
			continue
		}
		ino := inode(p)
		if ino == 0 {
			continue
		}
		fw := &fsWatch{ch: make(chan notify.EventInfo, buf), ino: ino, recursive: rec, done: make(chan struct{})}
		target := p
		if rec {
			target = p + "/..."
		}
		if err := notify.Watch(target, fw.ch, notify.All); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		go func() {
			for {
				select {
				case ei := <-fw.ch:
					select {
					case w.out <- ei.Path():
					case <-fw.done:
						return
					}
				case <-fw.done:
					return
				}
			}
		}()
		w.paths[p] = fw
		added++
	}
	return added, firstErr
}

func (w *fsWatches) stop(p string, fw *fsWatch) {
	notify.Stop(fw.ch)
	close(fw.done)
	delete(w.paths, p)
}

func (w *fsWatches) closeAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for p, fw := range w.paths {
		w.stop(p, fw)
	}
}

func (w *fsWatches) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.paths)
}
