// Package magento knows where things live in a Magento 2 installation and how
// its cache frontends are configured.
package magento

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"flushgopher/internal/components"
	"flushgopher/internal/logx"
	"flushgopher/internal/phpconf"
	"flushgopher/internal/storage"
)

const DumpFile = "var/cache-clean-config.json"

// FindBaseDir returns dir if given, otherwise walks up from the working
// directory until a directory containing app/etc/env.php is found.
func FindBaseDir(dir string) (string, error) {
	if dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", err
		}
		if st, err := os.Stat(abs); err != nil || !st.IsDir() {
			return "", fmt.Errorf("the Magento directory %q does not exist", dir)
		}
		return abs, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	d, _ := filepath.EvalSymlinks(cwd)
	for {
		if fileExists(filepath.Join(d, "app/etc/env.php")) {
			return d, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", errors.New("unable to determine the Magento directory (no app/etc/env.php found above the current directory; use -d)")
		}
		d = parent
	}
}

type envCache struct {
	stamp string
	env   map[string]any
}

// App gives access to one Magento installation (plus its integration test sandboxes).
type App struct {
	Base string

	mu     sync.Mutex
	envs   map[string]envCache
	prefix map[string]string
	fpcBug map[string]bool
	warned bool
}

func New(base string) *App {
	return &App{
		Base:   filepath.Clean(base),
		envs:   map[string]envCache{},
		prefix: map[string]string{},
		fpcBug: map[string]bool{},
	}
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func isSandbox(dir string) bool {
	return strings.Contains(filepath.ToSlash(dir), "dev/tests/integration/tmp/sandbox-")
}

// EnvFile is app/etc/env.php, or etc/env.php for integration test sandboxes.
func EnvFile(base string) string {
	if isSandbox(base) {
		return filepath.Join(base, "etc/env.php")
	}
	return filepath.Join(base, "app/etc/env.php")
}

// AllBaseDirs returns the main base dir followed by integration test sandbox dirs.
func (a *App) AllBaseDirs() []string {
	dirs := []string{a.Base}
	tmp := filepath.Join(a.Base, "dev/tests/integration/tmp")
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return dirs
	}
	for _, e := range entries {
		d := filepath.Join(tmp, e.Name())
		if strings.HasPrefix(e.Name(), "sandbox-") && fileExists(filepath.Join(d, "etc/env.php")) {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

func stamp(p string) string {
	st, err := os.Stat(p)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", st.ModTime().UnixNano(), st.Size())
}

// Env returns the env.php configuration of base. It is re-read only when the
// file changes. If var/cache-clean-config.json exists it is used instead.
func (a *App) Env(base string) (map[string]any, error) {
	dump := filepath.Join(base, DumpFile)
	src := EnvFile(base)
	useDump := !isSandbox(base) && fileExists(dump)
	if useDump {
		src = dump
	}
	st := stamp(src)
	a.mu.Lock()
	c, ok := a.envs[base]
	a.mu.Unlock()
	if ok && c.stamp == st && st != "" {
		return c.env, nil
	}
	var env map[string]any
	var err error
	if useDump {
		env, err = readDumpApp(dump)
	} else {
		if st == "" {
			return nil, fmt.Errorf("env.php not found: %s", src)
		}
		start := time.Now()
		env, err = phpconf.LoadConfig(src)
		logx.Debugf("Read %s in %s", src, time.Since(start).Round(time.Microsecond))
	}
	if err != nil {
		if ok { // keep using the last good config, e.g. while env.php is being rewritten
			logx.Debugf("Using previous config, reading %s failed: %v", src, err)
			return c.env, nil
		}
		return nil, err
	}
	a.mu.Lock()
	a.envs[base] = envCache{st, env}
	a.mu.Unlock()
	return env, nil
}

// EnvStamp changes whenever the configuration source of base changes.
func (a *App) EnvStamp(base string) string {
	dump := filepath.Join(base, DumpFile)
	if !isSandbox(base) && fileExists(dump) {
		return "dump:" + stamp(dump)
	}
	return stamp(EnvFile(base))
}

func readDumpApp(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d struct {
		App map[string]any `json:"app"`
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if d.App == nil {
		return map[string]any{}, nil
	}
	return normalizeJSON(d.App).(map[string]any), nil
}

func normalizeJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = normalizeJSON(x)
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = normalizeJSON(x)
		}
		return t
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		f, _ := t.Float64()
		return f
	}
	return v
}

// Get walks nested maps by keys.
func Get(m any, keys ...string) any {
	for _, k := range keys {
		mm, ok := m.(map[string]any)
		if !ok {
			return nil
		}
		m = mm[k]
	}
	return m
}

func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "1"
		}
		return ""
	}
	return fmt.Sprint(v)
}

func truthyNum(v any) bool {
	s := str(v)
	return s != "" && s != "0"
}

// defaultIDPrefix mirrors \Magento\Framework\App\Cache\Frontend\Factory: the
// first 3 chars of md5 of the absolute app/etc/ path, plus "_".
func (a *App) defaultIDPrefix(base string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if p, ok := a.prefix[base]; ok {
		return p
	}
	dir := filepath.Join(base, "app/etc")
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	h := md5.Sum([]byte(dir + "/"))
	p := hex.EncodeToString(h[:])[:3] + "_"
	logx.Debugf("Calculated default cache ID prefix %s from %s/", p, dir)
	a.prefix[base] = p
	return p
}

// fpcDirBugPresent: https://github.com/magento/magento2/pull/22228
func (a *App) fpcDirBugPresent(base string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if v, ok := a.fpcBug[base]; ok {
		return v
	}
	v := false
	for _, c := range []string{"lib/internal/Magento/Framework/App/Cache/Frontend/Pool.php", "vendor/magento/framework/App/Cache/Frontend/Pool.php"} {
		if b, err := os.ReadFile(filepath.Join(base, c)); err == nil {
			v = !strings.Contains(string(b), "array_replace_recursive($")
			break
		}
	}
	a.fpcBug[base] = v
	return v
}

func isFileBackend(cfg map[string]any) bool {
	b := strings.TrimLeft(str(cfg["backend"]), `\`)
	return b == "" || b == "Cm_Cache_Backend_File"
}

// CacheConfig returns the cache frontend config ("default" or "page_cache")
// with Magento's defaults filled in.
func (a *App) CacheConfig(base, frontend string) (storage.Config, error) {
	env, err := a.Env(base)
	if err != nil {
		return nil, err
	}
	cfg := storage.Config{}
	if m, ok := Get(env, "cache", "frontend", frontend).(map[string]any); ok {
		for k, v := range m {
			cfg[k] = v
		}
	}
	opts := map[string]any{}
	if m, ok := cfg["backend_options"].(map[string]any); ok {
		for k, v := range m {
			opts[k] = v
		}
	}
	cfg["backend_options"] = opts
	// Like storage.New: without a backend name but with a server, it is redis.
	fileBackend := isFileBackend(cfg) && str(opts["server"]) == ""
	if fileBackend {
		cfg["backend"] = "Cm_Cache_Backend_File"
	}
	if fileBackend && str(opts["cache_dir"]) == "" {
		dir := "cache"
		if frontend == "page_cache" {
			dir = "page_cache"
			if str(cfg["id_prefix"]) != "" && a.fpcDirBugPresent(base) {
				dir = "cache"
				if !a.warned {
					a.warned = true
					logx.PlainNotice("NOTICE: Workaround for FPC cache dir bug enabled!\nPlease read https://github.com/mage-os/magento-cache-clean/blob/master/doc/fpc-dir-bug.md")
				}
			}
		}
		opts["cache_dir"] = filepath.Join(base, "var", dir)
	} else if d := str(opts["cache_dir"]); d != "" && !filepath.IsAbs(d) {
		opts["cache_dir"] = filepath.Join(base, d)
	}
	if strings.TrimSpace(str(cfg["id_prefix"])) == "" {
		cfg["id_prefix"] = a.defaultIDPrefix(base)
	}
	return cfg, nil
}

// VarnishHosts returns the configured http_cache_hosts.
func (a *App) VarnishHosts(base string) []storage.VarnishHost {
	env, err := a.Env(base)
	if err != nil {
		return nil
	}
	return storage.VarnishHostsFromConfig(env["http_cache_hosts"])
}

// DisabledCaches returns cache types switched off in env.php, and whether all are off.
func (a *App) DisabledCaches() (disabled []string, all bool) {
	env, err := a.Env(a.Base)
	if err != nil {
		return nil, false
	}
	types, _ := Get(env, "cache_types").(map[string]any)
	for t, v := range types {
		if !truthyNum(v) {
			disabled = append(disabled, t)
		}
	}
	sort.Strings(disabled)
	return disabled, len(types) > 0 && len(disabled) == len(types)
}

// Components lists modules, themes and language packs.
func (a *App) Components() ([]components.Component, error) {
	if comps, ok, err := components.FromDump(a.Base); ok {
		return comps, err
	}
	return components.Discover(a.Base)
}
