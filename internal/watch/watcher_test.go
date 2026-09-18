package watch

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"flushgopher/internal/magento"
)

type call struct {
	kind string // "types" or "ids"
	args []string
}

type recorder struct {
	mu    sync.Mutex
	calls []call
}

func (r *recorder) CleanTypes(t []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call{"types", slices.Clone(t)})
	return nil
}

func (r *recorder) CleanIDs(ids []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call{"ids", slices.Clone(ids)})
	return nil
}

func (r *recorder) take() []call {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.calls
	r.calls = nil
	return c
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeMagento builds a minimal install with one module and one theme.
func fakeMagento(t *testing.T) (base, mod string) {
	base, _ = filepath.EvalSymlinks(t.TempDir())
	mod = filepath.Join(base, "app/code/Acme/Shop")
	theme := filepath.Join(base, "app/design/frontend/Acme/default")
	write(t, filepath.Join(base, ".git/HEAD"), "ref: refs/heads/main\n")
	write(t, filepath.Join(base, "app/etc/env.php"), "<?php return ['cache_types' => ['config' => 1, 'layout' => 1]];")
	write(t, filepath.Join(base, "vendor/composer/autoload_files.php"), `<?php
$vendorDir = dirname(__DIR__);
$baseDir = dirname($vendorDir);
return array(
    'a1' => $baseDir . '/app/code/Acme/Shop/registration.php',
    'a2' => $baseDir . '/app/design/frontend/Acme/default/registration.php',
);
`)
	write(t, filepath.Join(mod, "registration.php"), `<?php \Magento\Framework\Component\ComponentRegistrar::register(\Magento\Framework\Component\ComponentRegistrar::MODULE, 'Acme_Shop', __DIR__);`)
	write(t, filepath.Join(theme, "registration.php"), `<?php \Magento\Framework\Component\ComponentRegistrar::register(\Magento\Framework\Component\ComponentRegistrar::THEME, 'frontend/Acme/default', __DIR__);`)
	write(t, filepath.Join(mod, "Controller/Index/Index.php"), "<?php\nnamespace Acme\\Shop\\Controller\\Index;\nclass Index {}\n")
	return base, mod
}

func newTestWatcher(t *testing.T) (*Watcher, *recorder, string, string) {
	base, mod := fakeMagento(t)
	rec := &recorder{}
	opts := DefaultOptions()
	opts.Quiet = 30 * time.Millisecond
	opts.StormQuiet = 120 * time.Millisecond
	opts.StormFiles = 10
	w := New(magento.New(base), rec, opts)
	w.loadComponents()
	w.scanControllers()
	return w, rec, base, mod
}

// settle waits for the batch to become ready and processes it.
func settle(t *testing.T, w *Watcher) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !w.ready() {
		if time.Now().After(deadline) {
			t.Fatal("batch never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	w.flush()
}

func TestSingleLayoutEdit(t *testing.T) {
	w, rec, _, mod := newTestWatcher(t)
	f := filepath.Join(mod, "view/frontend/layout/default.xml")
	write(t, f, "<page/>")
	w.event(f)
	settle(t, w)
	got := rec.take()
	want := []call{{"types", []string{"full_page", "layout"}}, {"ids", []string{"resolvers"}}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDuplicateEventsAreIgnored(t *testing.T) {
	w, rec, _, mod := newTestWatcher(t)
	f := filepath.Join(mod, "etc/config.xml")
	write(t, f, "<config/>")
	w.event(f)
	settle(t, w)
	rec.take()

	// FSEvents re-reports, IDE touches without change: same size + mtime.
	for range 5 {
		w.event(f)
	}
	settle(t, w)
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("expected no clean for unchanged file, got %v", got)
	}

	// A real change is processed again.
	time.Sleep(5 * time.Millisecond)
	write(t, f, "<config><x/></config>")
	w.event(f)
	settle(t, w)
	if got := rec.take(); len(got) != 1 || !slices.Equal(got[0].args, []string{"config"}) {
		t.Fatalf("got %v", got)
	}
}

func TestMassChangeIsOneBatch(t *testing.T) {
	w, rec, base, mod := newTestWatcher(t)
	// A branch switch: hundreds of files across many kinds, with git active.
	w.event(filepath.Join(base, ".git/HEAD"))
	for i := range 300 {
		var f string
		switch i % 4 {
		case 0:
			f = filepath.Join(mod, fmt.Sprintf("view/frontend/templates/t%d.phtml", i))
		case 1:
			f = filepath.Join(mod, fmt.Sprintf("view/frontend/layout/l%d.xml", i))
		case 2:
			f = filepath.Join(mod, fmt.Sprintf("Model/M%d.php", i))
		case 3:
			f = filepath.Join(mod, "etc/di.xml")
		}
		write(t, f, "x")
		w.event(f)
		w.event(f) // duplicate
	}
	// Mid-storm the batch must not be processed, even after Quiet.
	time.Sleep(60 * time.Millisecond)
	if w.ready() {
		t.Fatal("mass change processed before StormQuiet elapsed")
	}
	settle(t, w)
	got := rec.take()
	want := []call{
		{"types", []string{"block_html", "config", "full_page", "layout"}},
		{"ids", []string{"resolvers"}},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestWaitsForGitLock(t *testing.T) {
	w, rec, base, mod := newTestWatcher(t)
	lock := filepath.Join(base, ".git/index.lock")
	write(t, lock, "")
	f := filepath.Join(mod, "etc/config.xml")
	write(t, f, "<config/>")
	w.event(f)
	time.Sleep(200 * time.Millisecond)
	if w.ready() {
		t.Fatal("processed while git holds index.lock")
	}
	os.Remove(lock)
	settle(t, w)
	if got := rec.take(); len(got) != 1 {
		t.Fatalf("got %v", got)
	}
}

func TestGitWorktree(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	main := filepath.Join(base, "main/.git/worktrees/wt")
	write(t, filepath.Join(main, "HEAD"), "x")
	write(t, filepath.Join(base, "wt/.git"), "gitdir: "+main+"\n")
	if got := gitDir(filepath.Join(base, "wt")); got != main {
		t.Fatalf("gitDir = %q, want %q", got, main)
	}
	write(t, filepath.Join(main, "index.lock"), "")
	if !gitBusy(main) {
		t.Fatal("worktree index.lock not detected")
	}
}

func TestIrrelevantPathsIgnored(t *testing.T) {
	w, rec, base, mod := newTestWatcher(t)
	for _, f := range []string{
		filepath.Join(base, "var/cache/mage--0/x"),
		filepath.Join(base, "generated/code/Acme/Shop/Foo/Interceptor.php"),
		filepath.Join(base, "pub/static/frontend/Acme/default/en_US/js/x.js"),
		filepath.Join(mod, "view/frontend/layout/default.xml___jb_tmp___"),
		filepath.Join(mod, "etc/.config.xml.swp"),
		filepath.Join(mod, "node_modules/x/etc/config.xml"),
	} {
		w.event(f)
	}
	if !w.first.IsZero() {
		t.Fatal("irrelevant events opened a batch")
	}
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestNewControllerCleansActionList(t *testing.T) {
	w, rec, _, mod := newTestWatcher(t)
	existing := filepath.Join(mod, "Controller/Index/Index.php")
	write(t, existing, "<?php\nnamespace Acme\\Shop\\Controller\\Index;\nclass Index { }\n")
	w.event(existing)
	settle(t, w)
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("editing a known controller should not clean, got %v", got)
	}
	f := filepath.Join(mod, "Controller/Foo/Bar.php")
	write(t, f, "<?php\nnamespace Acme\\Shop\\Controller\\Foo;\nclass Bar {}\n")
	w.event(f)
	settle(t, w)
	got := rec.take()
	want := []call{{"types", []string{"full_page"}}, {"ids", []string{"app_action_list"}}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestGeneratedCodeRemoved(t *testing.T) {
	w, _, base, mod := newTestWatcher(t)
	gen := filepath.Join(base, "generated/code/Acme/Shop/Model")
	interceptor := filepath.Join(gen, "Thing/Interceptor.php")
	proxy := filepath.Join(gen, "Thing/Proxy.php")
	factory := filepath.Join(gen, "ThingFactory.php")
	for _, f := range []string{interceptor, proxy, factory} {
		write(t, f, "<?php")
	}
	src := filepath.Join(mod, "Model/Thing.php")
	write(t, src, "<?php\nnamespace Acme\\Shop\\Model;\n\nclass Thing {}\n")
	w.event(src)
	settle(t, w)
	for _, f := range []string{interceptor, proxy} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s not removed", f)
		}
	}
	if _, err := os.Stat(factory); err != nil {
		t.Error("factory should be kept")
	}
}

func TestConfigPhpChangeReloadsModules(t *testing.T) {
	w, rec, base, _ := newTestWatcher(t)
	// A new module appears (e.g. after a branch switch) and config.php changes.
	newMod := filepath.Join(base, "app/code/Acme/New")
	write(t, filepath.Join(newMod, "registration.php"), `<?php \Magento\Framework\Component\ComponentRegistrar::register(\Magento\Framework\Component\ComponentRegistrar::MODULE, 'Acme_New', __DIR__);`)
	af := filepath.Join(base, "vendor/composer/autoload_files.php")
	b, _ := os.ReadFile(af)
	write(t, af, string(b[:len(b)-3])+"    'a3' => $baseDir . '/app/code/Acme/New/registration.php',\n);\n")
	w.event(filepath.Join(base, "app/etc/config.php"))
	settle(t, w)
	if got := rec.take(); len(got) != 1 || !slices.Equal(got[0].args, []string{"config"}) {
		t.Fatalf("got %v", got)
	}
	f := filepath.Join(newMod, "etc/di.xml")
	write(t, f, "<config/>")
	w.event(f)
	settle(t, w)
	if got := rec.take(); len(got) == 0 {
		t.Fatal("new module not watched after reload")
	}
}

// TestRealFilesystem runs the complete watcher against the OS file watcher.
func TestRealFilesystem(t *testing.T) {
	w, rec, _, mod := newTestWatcher(t)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { w.Run(stop); close(done) }()
	defer func() { close(stop); <-done }()
	time.Sleep(300 * time.Millisecond) // let FSEvents/inotify start

	write(t, filepath.Join(mod, "view/frontend/templates/a.phtml"), "<div/>")
	write(t, filepath.Join(mod, "etc/frontend/events.xml"), "<config/>")
	deadline := time.Now().Add(5 * time.Second)
	var got []call
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		got = append(got, rec.take()...)
		if len(got) >= 2 {
			break
		}
	}
	want := []call{{"types", []string{"block_html", "full_page"}}, {"ids", []string{"frontend__event_config_cache"}}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
