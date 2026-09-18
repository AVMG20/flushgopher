// Package watch watches Magento modules and themes and cleans the affected
// caches.
//
// Unlike the original cache-clean.js, file events are never handled one by
// one. They are collected into a batch that is processed once the file system
// has been quiet for a moment. Mass changes (git checkout, composer install,
// setup:upgrade) are detected and waited out, so they result in a single clean
// with the union of all affected cache types. Events for files whose size and
// mtime did not change since they were last processed are ignored, which kills
// the duplicate event storms FSEvents and IDE safe-writes produce.
package watch

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"flushgopher/internal/components"
	"flushgopher/internal/logx"
	"flushgopher/internal/magento"
	"flushgopher/internal/rules"
)

type Options struct {
	KeepGenerated bool
	// DryRun logs what would be cleaned without deleting any files.
	DryRun bool
	// Quiet is how long the file system must be silent before a batch is processed.
	Quiet time.Duration
	// StormQuiet replaces Quiet once a batch looks like a mass change.
	StormQuiet time.Duration
	// StormFiles is the number of pending files that marks a mass change.
	StormFiles int
	// MaxHold forces processing of a batch that never settles.
	MaxHold time.Duration
}

func DefaultOptions() Options {
	return Options{Quiet: 300 * time.Millisecond, StormQuiet: 2 * time.Second, StormFiles: 40, MaxHold: 90 * time.Second}
}

// Cleaner cleans cache types and ids; implemented by *cache.Cleaner.
type Cleaner interface {
	CleanTypes(types []string) error
	CleanIDs(ids []string) error
}

type sig struct {
	exists bool
	size   int64
	mtime  int64
}

type Watcher struct {
	app     *magento.App
	cleaner Cleaner
	opts    Options

	events  chan string
	rescan  chan struct{}
	gitDir  string
	fs      *fsWatches
	compDir map[string]components.Component // component dir → component

	mu          sync.Mutex // guards the batch state below and controllers
	pending     map[string]struct{}
	first, last time.Time
	reload      bool                // the module list may have changed
	configFile  bool                // app/etc/config.php or env.php changed
	static      map[string]struct{} // compiled requirejs-config.js files with events
	gitActive   bool
	stormNoted  bool
	controllers map[string]bool
	seen        map[string]sig
}

func New(app *magento.App, cleaner Cleaner, opts Options) *Watcher {
	w := &Watcher{
		app:         app,
		cleaner:     cleaner,
		opts:        opts,
		events:      make(chan string, 1<<16),
		rescan:      make(chan struct{}, 1),
		pending:     map[string]struct{}{},
		static:      map[string]struct{}{},
		gitDir:      gitDir(app.Base),
		controllers: map[string]bool{},
		seen:        map[string]sig{},
	}
	w.fs = newFSWatches(w.events)
	return w
}

func (w *Watcher) p(rel string) string { return filepath.Join(w.app.Base, rel) }

// loadComponents (re)discovers modules and themes and reports whether the set
// of component dirs changed.
func (w *Watcher) loadComponents() ([]components.Component, bool) {
	start := time.Now()
	comps, err := w.app.Components()
	if err != nil {
		logx.Event(logx.Error, logx.CErr, "ERROR", "Reading module list: %v", err)
	}
	dirs := map[string]components.Component{}
	for _, c := range comps {
		if c.Type == components.Module || c.Type == components.Theme || c.Type == components.Language {
			dirs[c.Dir] = c
		}
	}
	w.mu.Lock()
	old := w.compDir
	w.compDir = dirs
	w.mu.Unlock()
	var added, removed []string
	if old != nil {
		for d, c := range dirs {
			if _, ok := old[d]; !ok {
				added = append(added, c.Name)
			}
		}
		for d, c := range old {
			if _, ok := dirs[d]; !ok {
				removed = append(removed, c.Name)
			}
		}
	}
	logComponentChanges("+", added)
	logComponentChanges("-", removed)
	changed := len(added)+len(removed) > 0
	logx.Debugf("Discovered %d components in %s", len(comps), time.Since(start).Round(time.Millisecond))
	return comps, changed
}

func logComponentChanges(sign string, names []string) {
	if len(names) == 0 {
		return
	}
	slices.Sort(names)
	verb := map[string]string{"+": "added", "-": "removed"}[sign]
	if len(names) <= 3 || logx.Enabled(logx.Info) {
		logx.Event(logx.Notice, logx.CWatch, "MODS", "%s %s", logx.Paint(logx.CWatch, sign), logx.Chips(logx.CWatch, names))
		return
	}
	logx.Event(logx.Notice, logx.CWatch, "MODS", "%s modules/themes %s %s", logx.Bold(strconv.Itoa(len(names))), verb,
		logx.Dim("("+strings.Join(names[:2], ", ")+", …)"))
}

// wantedWatches computes the paths to watch; true means recursive.
func (w *Watcher) wantedWatches() map[string]bool {
	want := map[string]bool{}
	add := func(p string, rec bool) {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			want[p] = want[p] || rec
		}
	}
	w.mu.Lock()
	dirs := make([]string, 0, len(w.compDir))
	for d := range w.compDir {
		dirs = append(dirs, d)
	}
	w.mu.Unlock()
	// FSEvents streams are cheap per tree, so watch the top level dirs that
	// contain components (app, vendor, ...) and filter events by component.
	base := w.app.Base + string(filepath.Separator)
	for _, d := range dirs {
		if strings.HasPrefix(d, base) {
			top := strings.SplitN(strings.TrimPrefix(d, base), string(filepath.Separator), 2)[0]
			add(w.p(top), true)
		} else {
			add(d, true)
		}
	}
	add(w.p("app/i18n"), true)
	add(w.p("app/etc"), false)
	add(w.p("vendor/composer"), false)
	add(w.gitDir, false)
	add(w.p("pub/static/frontend"), true)
	// Drop entries covered by a recursive parent.
	for p := range want {
		for d := filepath.Dir(p); d != filepath.Dir(d); d = filepath.Dir(d) {
			if want[d] {
				delete(want, p)
				break
			}
		}
	}
	return want
}

func (w *Watcher) syncWatches() {
	added, err := w.fs.sync(w.wantedWatches(), 1<<15)
	if err != nil {
		logx.Event(logx.Info, logx.CErr, "WATCH", "Some paths could not be watched: %v", err)
	}
	if added > 0 {
		logx.Debugf("Added %d watches (%d total)", added, w.fs.count())
	}
}

func (w *Watcher) scanControllers() {
	w.mu.Lock()
	dirs := make([]string, 0, len(w.compDir))
	for d, c := range w.compDir {
		if c.Type == components.Module {
			dirs = append(dirs, d)
		}
	}
	w.mu.Unlock()
	found := map[string]bool{}
	for _, d := range dirs {
		filepath.WalkDir(filepath.Join(d, "Controller"), func(p string, e os.DirEntry, err error) error {
			if err == nil && !e.IsDir() && strings.HasSuffix(p, ".php") {
				found[p] = true
			}
			return nil
		})
	}
	w.mu.Lock()
	for p := range found {
		w.controllers[p] = true
	}
	w.mu.Unlock()
}

// Start begins watching and processes batches until stop is closed.
func (w *Watcher) Run(stop <-chan struct{}) {
	comps, _ := w.loadComponents()
	w.syncWatches()
	go w.scanControllers()
	logx.Event(logx.Notice, logx.CWatch, "WATCH", "%s", w.summary(comps))

	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			w.fs.closeAll()
			return
		case <-w.rescan:
			w.doRescan()
		case p := <-w.events:
			w.event(p)
		case <-tick.C:
			if w.ready() {
				w.flush()
			}
		}
	}
}

func ignored(p string) bool {
	b := filepath.Base(p)
	switch {
	case strings.HasSuffix(b, "~"), strings.HasSuffix(b, ".swp"), strings.HasSuffix(b, ".swx"),
		strings.HasSuffix(b, ".tmp"), strings.HasSuffix(b, ".unison.tmp"), b == "4913", b == ".DS_Store",
		strings.Contains(b, "___jb_"), strings.HasPrefix(b, ".#"):
		return true
	}
	s := filepath.ToSlash(p)
	return strings.Contains(s, "/.mutagen-temporary") || strings.Contains(s, "/node_modules/")
}

// inComponent reports whether p lies inside a watched component dir.
func (w *Watcher) inComponent(p string) bool {
	for d := filepath.Dir(p); d != filepath.Dir(d); d = filepath.Dir(d) {
		if _, ok := w.compDir[d]; ok {
			return true
		}
		if d == w.app.Base {
			return false
		}
	}
	return false
}

func (w *Watcher) event(p string) {
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	rel := strings.TrimPrefix(p, w.app.Base+string(filepath.Separator))
	switch {
	case w.gitDir != "" && strings.HasPrefix(p, w.gitDir+string(filepath.Separator)):
		// Checkouts, rebases, stashes: keep the batch open while git works.
		w.gitActive = true
	case strings.Contains(filepath.ToSlash(p), "/.git/"), ignored(p):
		return
	case rel == "app/etc/config.php" || rel == "app/etc/env.php" || rel == magento.DumpFile:
		w.reload, w.configFile = true, true
	case rel == "vendor/composer/autoload_files.php" || filepath.Base(p) == "registration.php":
		w.reload = true
		if filepath.Base(p) == "registration.php" && w.inComponent(p) {
			w.pending[p] = struct{}{}
		}
	case strings.HasPrefix(rel, "pub/static/frontend/"):
		if filepath.Base(p) != "requirejs-config.js" {
			return
		}
		w.static[p] = struct{}{}
	case strings.HasPrefix(rel, "app/i18n/") || w.inComponent(p):
		w.pending[p] = struct{}{}
	default:
		return
	}
	if w.first.IsZero() {
		w.first = now
	}
	w.last = now
}

// gitDir returns the git directory of base, following the ".git" file of a
// worktree, or "" if base is not a git checkout.
func gitDir(base string) string {
	dot := filepath.Join(base, ".git")
	st, err := os.Stat(dot)
	if err != nil {
		return ""
	}
	if st.IsDir() {
		return dot
	}
	b, err := os.ReadFile(dot)
	if err != nil {
		return ""
	}
	d, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return ""
	}
	d = strings.TrimSpace(d)
	if !filepath.IsAbs(d) {
		d = filepath.Join(base, d)
	}
	if r, err := filepath.EvalSymlinks(d); err == nil {
		d = r
	}
	return d
}

func gitBusy(dir string) bool {
	if dir == "" {
		return false
	}
	for _, f := range []string{"index.lock", "HEAD.lock"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			return true
		}
	}
	return false
}

// ready decides whether the pending batch should be processed now.
func (w *Watcher) ready() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.first.IsZero() {
		return false
	}
	storm := len(w.pending) >= w.opts.StormFiles || w.gitActive || w.reload
	quiet := w.opts.Quiet
	if storm {
		quiet = w.opts.StormQuiet
		if !w.stormNoted && len(w.pending) >= w.opts.StormFiles {
			w.stormNoted = true
			logx.Event(logx.Notice, logx.CStorm, "STORM", "Mass change detected (branch switch, composer, setup:upgrade?) – waiting for it to settle…")
		}
	}
	since := time.Since(w.last)
	if time.Since(w.first) > w.opts.MaxHold {
		return true
	}
	if since < quiet {
		return false
	}
	return !gitBusy(w.gitDir)
}

type fileResult struct {
	file   string
	exists bool
	types  []string
	ids    []string
	jsTr   bool
}

func (w *Watcher) takeBatch() (files []string, reload, configFile, staticFPC bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for p := range w.pending {
		files = append(files, p)
	}
	// Only a removed compiled requirejs-config.js invalidates the page cache.
	for p := range w.static {
		if _, err := os.Stat(p); err != nil {
			staticFPC = true
		}
	}
	reload, configFile = w.reload, w.configFile
	w.pending = map[string]struct{}{}
	w.static = map[string]struct{}{}
	w.first, w.last = time.Time{}, time.Time{}
	w.reload, w.configFile, w.gitActive, w.stormNoted = false, false, false, false
	return
}

// changed filters out files whose state did not change since they were last processed.
func (w *Watcher) changed(files []string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, f := range files {
		var s sig
		if st, err := os.Stat(f); err == nil {
			if st.IsDir() {
				continue
			}
			s = sig{true, st.Size(), st.ModTime().UnixNano()}
		}
		if prev, ok := w.seen[f]; ok && prev == s {
			continue
		}
		w.seen[f] = s
		out = append(out, f)
	}
	return out
}

func analyze(files []string) []fileResult {
	res := make([]fileResult, len(files))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.GOMAXPROCS(0)*2)
	for i, f := range files {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			_, err := os.Stat(f)
			res[i] = fileResult{
				file:   f,
				exists: err == nil,
				types:  rules.CacheTypes(f),
				ids:    rules.CacheIDs(f),
				jsTr:   err == nil && rules.ContainsJSTranslation(f),
			}
		}()
	}
	wg.Wait()
	return res
}

// ProcessFiles cleans caches for an explicit list of changed files (--file-list).
func (w *Watcher) ProcessFiles(files []string) {
	w.loadComponents()
	w.scanControllers()
	w.process(files, false, false)
}

func (w *Watcher) flush() {
	files, reload, configFile, staticFPC := w.takeBatch()
	cleanConfig := configFile
	if reload {
		_, changed := w.loadComponents()
		cleanConfig = cleanConfig || changed
		w.syncWatches()
		go w.scanControllers()
	}
	w.process(files, cleanConfig, staticFPC)
}

func (w *Watcher) process(files []string, cleanConfig, staticFPC bool) {
	start := time.Now()
	total := len(files)
	files = w.changed(files)
	if len(files) == 0 && !cleanConfig && !staticFPC {
		if total > 0 {
			logx.Debugf("Ignored %d duplicate event(s)", total)
		}
		return
	}
	sort.Strings(files)
	results := analyze(files)

	types := map[string]bool{}
	ids := map[string]bool{}
	var phpFiles, diFiles []string
	extAttr, jsTr := false, false
	w.mu.Lock()
	for _, r := range results {
		for _, t := range r.types {
			types[t] = true
		}
		for _, id := range r.ids {
			ids[id] = true
		}
		if r.exists && rules.IsController(r.file) && !w.controllers[r.file] {
			w.controllers[r.file] = true
			ids["app_action_list"] = true
			types["full_page"] = true
		}
		if strings.HasSuffix(r.file, ".php") && r.exists {
			phpFiles = append(phpFiles, r.file)
		}
		if rules.IsDiXML(r.file) {
			diFiles = append(diFiles, r.file)
		}
		if filepath.Base(r.file) == "extension_attributes.xml" {
			extAttr = true
		}
		jsTr = jsTr || r.jsTr
	}
	w.mu.Unlock()
	if cleanConfig {
		types["config"] = true
	}
	if staticFPC {
		types["full_page"] = true
	}

	removeGenerated := !w.opts.KeepGenerated && !w.opts.DryRun
	touchesFiles := !w.opts.DryRun && len(diFiles) > 0 || removeGenerated && (len(phpFiles) > 0 || extAttr || jsTr)
	if len(types) == 0 && len(ids) == 0 && !touchesFiles {
		logx.Debugf("Nothing to clean for %d changed file(s)", len(files))
		return
	}
	w.logBatch(files, total)

	if touchesFiles {
		for _, base := range w.app.AllBaseDirs() {
			for _, di := range diFiles {
				magento.CleanCreatuityCache(base, rules.DiArea(di))
				magento.RemoveInterceptorsForDiXML(base, di)
			}
			if removeGenerated {
				magento.RemoveGeneratedForPHP(base, phpFiles)
				if extAttr {
					magento.RemoveGeneratedExtensionAttributes(base)
				}
			}
		}
		if removeGenerated && jsTr {
			magento.RemoveJSTranslations(w.app.Base, "frontend")
		}
	}

	cleaned := false
	if len(types) > 0 {
		if err := w.cleaner.CleanTypes(keys(types)); err != nil {
			logx.Event(logx.Error, logx.CErr, "ERROR", "%v", err)
		}
		cleaned = true
	}
	if len(ids) > 0 {
		if err := w.cleaner.CleanIDs(keys(ids)); err != nil {
			logx.Event(logx.Error, logx.CErr, "ERROR", "%v", err)
		}
		cleaned = true
	}
	if cleaned {
		logx.Debugf("Batch done in %s", time.Since(start).Round(time.Millisecond))
		WarnIfAllCachesDisabled(w.app)
	}
}

func (w *Watcher) logBatch(files []string, total int) {
	if len(files) == 0 {
		return
	}
	if len(files) <= 3 {
		for _, f := range files {
			rel, err := filepath.Rel(w.app.Base, f)
			if err != nil || strings.HasPrefix(rel, "..") {
				rel = f
			}
			logx.Event(logx.Notice, logx.CDim, "FILE", "%s", logx.Dim(rel))
		}
		return
	}
	dup := ""
	if total > len(files) {
		dup = logx.Dim(" (" + strconv.Itoa(total-len(files)) + " unchanged skipped)")
	}
	logx.Event(logx.Notice, logx.CStorm, "BATCH", "%s changed files%s", logx.Bold(strconv.Itoa(len(files))), dup)
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// WarnIfAllCachesDisabled prints a hint when env.php disables every cache.
func WarnIfAllCachesDisabled(app *magento.App) {
	if _, all := app.DisabledCaches(); all {
		logx.Event(logx.Notice, logx.CStorm, "NOTE", "All caches are currently disabled – consider enabling them while running flushgopher.")
	}
}

func (w *Watcher) summary(comps []components.Component) string {
	counts := map[components.Type]int{}
	for _, c := range comps {
		counts[c.Type]++
	}
	n := func(i int) string { return logx.Bold(strconv.Itoa(i)) }
	return fmt.Sprintf("%s modules · %s themes · %s language packs · %s watches",
		n(counts[components.Module]), n(counts[components.Theme]), n(counts[components.Language]), n(w.fs.count()))
}

// Rescan asks the running watcher to re-discover modules and themes.
func (w *Watcher) Rescan() {
	select {
	case w.rescan <- struct{}{}:
	default: // one is already queued
	}
}

// doRescan re-discovers modules and themes, adds watches for new ones and
// cleans the config cache if the module list changed.
func (w *Watcher) doRescan() {
	logx.Event(logx.Notice, logx.CWatch, "WATCH", "Rescanning modules and themes…")
	start := time.Now()
	comps, changed := w.loadComponents()
	w.syncWatches()
	w.scanControllers()
	if changed {
		if err := w.cleaner.CleanTypes([]string{"config"}); err != nil {
			logx.Event(logx.Error, logx.CErr, "ERROR", "%v", err)
		}
	}
	result := "no changes"
	if changed {
		result = "module list changed"
	}
	logx.Event(logx.Notice, logx.COK, "DONE", "%s · %s %s", w.summary(comps), result,
		logx.Dim("("+time.Since(start).Round(time.Millisecond).String()+")"))
}
