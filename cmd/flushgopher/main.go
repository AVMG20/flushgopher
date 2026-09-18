// flushgopher cleans Magento 2 caches, and with --watch keeps them clean while
// you edit modules and themes. A Go take on mage-os/magento-cache-clean.
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"flushgopher/internal/cache"
	"flushgopher/internal/hotkeys"
	"flushgopher/internal/logx"
	"flushgopher/internal/magento"
	"flushgopher/internal/watch"
)

var version = "dev"

const usage = `Usage: flushgopher [options] [cache-types...]
Clean the given cache types. If none are given, clean all cache types.

  -d, --directory <dir>   Magento base directory (default: search upwards from cwd)
  -w, --watch             Watch for file changes and clean affected caches
  -f, --file-list <file>  Clean caches based on a list of changed files (one per line)
  -k, --keep-generated    Don't remove generated code / js-translation.json
      --debounce <ms>     Quiet time before a batch of changes is processed (default 300)
      --dry-run           Only log what would be cleaned
  -v, --verbose           Display more information
  -vv, --debug            Display too much information
  -s, --silent            Display less information
      --version           Display the version
  -h, --help              This help message
`

type opts struct {
	dir, fileList     string
	watch, keepGen    bool
	dryRun            bool
	help, showVersion bool
	debounce          time.Duration
	verbosity         logx.Level
	types             []string
}

func parseArgs(in []string) (opts, error) {
	o := opts{verbosity: logx.Notice}
	// Accept --directory=/path as well as --directory /path.
	var args []string
	for _, a := range in {
		if k, v, ok := strings.Cut(a, "="); ok && strings.HasPrefix(k, "--") {
			args = append(args, k, v)
		} else {
			args = append(args, a)
		}
	}
	next := func(i *int, name string) (string, error) {
		if *i+1 >= len(args) {
			return "", fmt.Errorf("%s needs a value", name)
		}
		*i++
		return args[*i], nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		var err error
		switch a {
		case "-d", "--directory":
			o.dir, err = next(&i, a)
		case "-f", "--file-list":
			o.fileList, err = next(&i, a)
		case "-w", "--watch", "-nw", "-wn":
			o.watch = true
		case "-n", "--no-flood-guard":
			// Accepted for compatibility: batching replaces the flood guard.
		case "-k", "--keep-generated":
			o.keepGen = true
		case "--dry-run":
			o.dryRun = true
		case "--debounce":
			var v string
			if v, err = next(&i, a); err == nil {
				var ms int
				ms, err = strconv.Atoi(v)
				o.debounce = time.Duration(ms) * time.Millisecond
			}
		case "--verbosity":
			var v string
			if v, err = next(&i, a); err == nil {
				var n int
				n, err = strconv.Atoi(v)
				o.verbosity = logx.Level(n)
			}
		case "-v", "--verbose":
			o.verbosity++
		case "-vv", "--debug":
			o.verbosity += 2
		case "-s", "--silent":
			o.verbosity--
		case "-h", "--help":
			o.help = true
		case "--version":
			o.showVersion = true
		default:
			if strings.HasPrefix(a, "-") {
				return o, fmt.Errorf("unknown option %s", a)
			}
			o.types = append(o.types, a)
		}
		if err != nil {
			return o, err
		}
	}
	return o, nil
}

func main() {
	o, err := parseArgs(os.Args[1:])
	if err != nil {
		fail(err)
	}
	switch {
	case o.help:
		fmt.Print(usage)
		return
	case o.showVersion:
		fmt.Println(version)
		return
	}
	logx.SetVerbosity(o.verbosity)

	base, err := magento.FindBaseDir(o.dir)
	if err != nil {
		fail(err)
	}
	if r, err := filepath.EvalSymlinks(base); err == nil {
		base = r
	}
	app := magento.New(base)
	cleaner := cache.New(app)
	cleaner.DryRun = o.dryRun
	defer cleaner.Close()

	wopts := watch.DefaultOptions()
	wopts.KeepGenerated = o.keepGen
	wopts.DryRun = o.dryRun
	if o.debounce > 0 {
		wopts.Quiet = o.debounce
	}

	switch {
	case o.fileList != "":
		files, err := readLines(o.fileList)
		if err != nil {
			fail(err)
		}
		for i, f := range files {
			if !filepath.IsAbs(f) {
				files[i] = filepath.Join(base, f)
			}
		}
		watch.New(app, cleaner, wopts).ProcessFiles(files)
	case o.watch:
		runWatch(app, cleaner, wopts)
	default:
		if err := cleaner.CleanTypes(o.types); err != nil {
			cleaner.Close()
			fail(err)
		}
	}
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			out = append(out, l)
		}
	}
	return out, sc.Err()
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, logx.Paint(logx.CErr, "[ERROR] "+err.Error()))
	os.Exit(1)
}

func banner(base string) {
	g := func(s string) string { return logx.Paint(logx.CGopher, s) }
	logx.PlainAlways("%s %s %s", g("ʕ◔ϖ◔ʔ"), logx.Bold("flushgopher"), logx.Dim(version))
	logx.PlainNotice("%s %s", logx.Dim("magento"), base)
}

var keyTypes = map[byte][]string{
	'c': {"config"},
	'b': {"block_html"},
	'l': {"layout"},
	't': {"translate"},
	'f': {"full_page"},
	'v': {"block_html", "layout", "full_page", "translate"},
	'm': {"hyva_cms", "hyva_admin_dashboard"},
	'a': {},
}

func showHotkeys() {
	k := logx.Key
	logx.PlainNotice("")
	logx.PlainNotice("%s  %s %s %s %s %s %s %s %s", logx.Dim("caches   "),
		k("c", "onfig"), k("b", "lock_html"), k("l", "ayout"), k("t", "ranslate"),
		k("f", "ull_page"), k("v", "iew"), k("m", "isc"), k("a", "ll"))
	logx.PlainNotice("%s  %s  %s  %s %s", logx.Dim("files    "),
		k("G", "enerated code"), k("I", "ntegration sandboxes"), k("F", "rontend static"), k("A", "dminhtml static"))
	logx.PlainNotice("%s  %s  %s  %s", logx.Dim("gopher   "), k("r", "escan modules"), k("?", " help"), k("q", "uit"))
	logx.PlainNotice("")
}

func runWatch(app *magento.App, cleaner *cache.Cleaner, wopts watch.Options) {
	banner(app.Base)
	if disabled, all := app.DisabledCaches(); all {
		logx.Event(logx.Notice, logx.CStorm, "NOTE", "All caches are currently disabled – consider enabling them while watching.")
	} else if len(disabled) > 0 {
		logx.Event(logx.Notice, logx.CStorm, "NOTE", "Disabled caches: %s", strings.Join(disabled, ", "))
	}

	w := watch.New(app, cleaner, wopts)
	stop := make(chan struct{})
	done := make(chan struct{})
	quit := make(chan struct{}, 1)

	restore, ok := hotkeys.Start(func(key byte) {
		base := app.Base
		switch key {
		case hotkeys.CtrlC, 'q':
			select {
			case quit <- struct{}{}:
			default:
			}
		case 'G':
			magento.CleanGeneratedCode(base)
		case 'I':
			magento.CleanIntegrationSandboxes(base)
		case 'F':
			magento.CleanStaticArea(base, "frontend")
		case 'A':
			magento.CleanStaticArea(base, "adminhtml")
		case 'r':
			w.Rescan()
		case '?', 'h':
			showHotkeys()
		default:
			if types, ok := keyTypes[key]; ok {
				if err := cleaner.CleanTypes(types); err != nil {
					logx.Event(logx.Error, logx.CErr, "ERROR", "%v", err)
				}
			}
		}
	})
	if ok {
		showHotkeys()
	} else {
		logx.PlainNotice("%s", logx.Dim("stdin is not a terminal – hotkeys disabled"))
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		w.Run(stop)
		close(done)
	}()
	logx.PlainNotice("%s", logx.Paint(logx.COK, "Watching – Ctrl-C to quit"))

	select {
	case <-sig:
	case <-quit:
	}
	close(stop)
	<-done
	restore()
	logx.PlainNotice("%s bye!", logx.Paint(logx.CGopher, "ʕ-ϖ-ʔ"))
}
