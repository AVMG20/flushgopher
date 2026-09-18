// Package components discovers Magento components (modules, themes, language
// packs, libraries, setup components) without running PHP. It replaces
// \Magento\Framework\Component\ComponentRegistrar::getPaths(), which needs a
// working composer autoloader and breaks while registration.php files are
// missing (e.g. during a branch switch or setup:upgrade).
package components

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"

	"flushgopher/internal/phpconf"
)

// Type is a Magento component type, as in ComponentRegistrar's constants.
type Type string

const (
	Module   Type = "module"
	Theme    Type = "theme"
	Language Type = "language"
	Library  Type = "library"
	Setup    Type = "setup"
)

// Component is a registered Magento component.
type Component struct {
	Type Type
	Name string // e.g. "Magento_Catalog", "frontend/Magento/luma"
	Dir  string // absolute, cleaned, symlinks resolved when possible
}

// DefaultGlobPatterns is Magento's stock app/etc/registration_globlist.php.
var DefaultGlobPatterns = []string{
	"app/code/*/*/cli_commands.php",
	"app/code/*/*/registration.php",
	"app/design/*/*/*/registration.php",
	"app/i18n/*/*/registration.php",
	"lib/internal/*/*/registration.php",
	"lib/internal/*/*/*/registration.php",
	"setup/src/*/*/registration.php",
}

// Discover returns all components registered for the Magento install at
// baseDir, sorted by type then name.
func Discover(baseDir string) ([]Component, error) {
	c, _, err := DiscoverWithWarnings(baseDir)
	return c, err
}

// DiscoverWithWarnings is Discover, additionally returning non-fatal problems
// such as missing or unparsable registration.php files.
func DiscoverWithWarnings(baseDir string) ([]Component, []string, error) {
	base, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, nil, err
	}
	vendorDir := findVendorDir(base)
	files, nonComposer, err := readAutoloadFiles(base, vendorDir)
	if err != nil {
		return nil, nil, err
	}
	var warnings []string
	if nonComposer {
		patterns, warn := globPatterns(base)
		if warn != "" {
			warnings = append(warnings, warn)
		}
		for _, pat := range patterns {
			if filepath.Base(pat) != "registration.php" {
				continue // e.g. cli_commands.php
			}
			matches, err := filepath.Glob(filepath.Join(base, filepath.FromSlash(pat)))
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("bad glob pattern %q: %v", pat, err))
				continue
			}
			sort.Strings(matches) // PHP uses GLOB_NOSORT, but order only matters for duplicates
			files = append(files, matches...)
		}
	}

	results := parseAll(files)
	seen := make(map[[2]string]bool)
	var comps []Component
	for _, r := range results {
		if r.warn != "" {
			warnings = append(warnings, r.warn)
		}
		for _, c := range r.comps {
			key := [2]string{string(c.Type), c.Name}
			if seen[key] {
				continue
			}
			seen[key] = true
			comps = append(comps, c)
		}
	}
	sort.Slice(comps, func(i, j int) bool {
		if comps[i].Type != comps[j].Type {
			return comps[i].Type < comps[j].Type
		}
		return comps[i].Name < comps[j].Name
	})
	return comps, warnings, nil
}

// findVendorDir honours composer.json's config.vendor-dir.
func findVendorDir(base string) string {
	vendor := filepath.Join(base, "vendor")
	data, err := os.ReadFile(filepath.Join(base, "composer.json"))
	if err != nil {
		return vendor
	}
	var cj struct {
		Config struct {
			VendorDir string `json:"vendor-dir"`
		} `json:"config"`
	}
	if json.Unmarshal(data, &cj) != nil || cj.Config.VendorDir == "" {
		return vendor
	}
	v := filepath.FromSlash(cj.Config.VendorDir)
	if !filepath.IsAbs(v) {
		v = filepath.Join(base, v)
	}
	return filepath.Clean(v)
}

var autoloadEntryRe = regexp.MustCompile(`=>\s*(?:\$(vendorDir|baseDir)\s*\.\s*)?'((?:[^'\\]|\\.)*)'`)

// readAutoloadFiles returns the registration.php files from composer's
// autoload_files.php and whether NonComposerComponentRegistration.php is
// included.
func readAutoloadFiles(base, vendorDir string) ([]string, bool, error) {
	composerDir := filepath.Join(vendorDir, "composer")
	path := filepath.Join(composerDir, "autoload_files.php")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("reading composer autoload files: %w", err)
	}
	// $vendorDir = dirname(__DIR__), $baseDir = dirname($vendorDir) in
	// composer's generated file; derive from the directory we actually read.
	vendorDir = filepath.Dir(composerDir)
	var files []string
	nonComposer := false
	for _, m := range autoloadEntryRe.FindAllSubmatch(data, -1) {
		rel := strings.NewReplacer(`\'`, `'`, `\\`, `\`).Replace(string(m[2]))
		var p string
		switch string(m[1]) {
		case "vendorDir":
			p = vendorDir + filepath.FromSlash(rel)
		case "baseDir":
			p = base + filepath.FromSlash(rel)
		default:
			p = filepath.FromSlash(rel)
			if !filepath.IsAbs(p) {
				p = filepath.Join(base, p)
			}
		}
		switch filepath.Base(p) {
		case "registration.php":
			files = append(files, filepath.Clean(p))
		case "NonComposerComponentRegistration.php":
			nonComposer = true
		}
	}
	return files, nonComposer, nil
}

func globPatterns(base string) ([]string, string) {
	path := filepath.Join(base, "app", "etc", "registration_globlist.php")
	if _, err := os.Stat(path); err != nil {
		return DefaultGlobPatterns, ""
	}
	v, err := phpconf.ParseFile(path)
	if err != nil {
		return DefaultGlobPatterns, fmt.Sprintf("using default glob patterns: %v", err)
	}
	var out []string
	add := func(e any) bool {
		s, ok := e.(string)
		if ok {
			out = append(out, s)
		}
		return ok
	}
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			if !add(e) {
				return DefaultGlobPatterns, "using default glob patterns: registration_globlist.php contains non-strings"
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !add(x[k]) {
				return DefaultGlobPatterns, "using default glob patterns: registration_globlist.php contains non-strings"
			}
		}
	default:
		return DefaultGlobPatterns, "using default glob patterns: registration_globlist.php does not return an array"
	}
	return out, ""
}

type parseResult struct {
	comps []Component
	warn  string
}

// parseAll parses the registration files with a bounded worker pool,
// keeping results in input order.
func parseAll(files []string) []parseResult {
	results := make([]parseResult, len(files))
	workers := min(max(runtime.GOMAXPROCS(0)*2, 4), 32, len(files))
	var wg sync.WaitGroup
	next := make(chan int)
	for range workers {
		wg.Go(func() {
			for i := range next {
				comps, err := ParseRegistration(files[i])
				if err != nil {
					if !errors.Is(err, os.ErrNotExist) {
						results[i].warn = err.Error()
					} else {
						results[i].warn = "missing " + files[i]
					}
					continue
				}
				results[i].comps = comps
			}
		})
	}
	for i := range files {
		next <- i
	}
	close(next)
	wg.Wait()
	return results
}

var (
	// Matches: [\ns\]Registrar::register( [\ns\]Registrar::TYPE | 'type', 'Name', __DIR__ [. '/rel'] )
	registerRe = regexp.MustCompile(`(?is)\b[\w\\]*?\w+\s*::\s*register\s*\(\s*` +
		`(?:[\w\\]*\w+\s*::\s*(MODULE|THEME|LANGUAGE|LIBRARY|SETUP)|(['"])(module|theme|language|library|setup)['"])\s*,\s*` +
		`(?:'((?:[^'\\]|\\.)*)'|"((?:[^"\\$]|\\.)*)")\s*,\s*` +
		`__DIR__\s*(?:\.\s*(?:'((?:[^'\\]|\\.)*)'|"((?:[^"\\$]|\\.)*)")\s*)?,?\s*\)`)
	// String literals naming another registration.php (or a glob of them),
	// optionally prefixed by "__DIR__ .".
	includedRe = regexp.MustCompile(`(__DIR__\s*\.\s*)?(?:'([^'\\\n]*registration\.php)'|"([^"\\$\n]*registration\.php)")`)
)

// ParseRegistration extracts the ComponentRegistrar::register() calls from a
// registration.php file. Files that only include other registration.php
// files (a list of literal paths or glob patterns relative to __DIR__, as
// some "superpackages" do) are followed.
func ParseRegistration(path string) ([]Component, error) {
	return parseRegistration(path, 0)
}

const maxIncludeDepth = 3

func parseRegistration(path string, depth int) ([]Component, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	src := stripComments(data)
	dir := filepath.Dir(path)
	var comps []Component
	for _, m := range registerRe.FindAllSubmatch(src, -1) {
		typ := strings.ToLower(string(m[1]))
		if typ == "" {
			typ = strings.ToLower(string(m[3]))
		}
		name := unquote(m[4], m[5])
		rel := unquote(m[6], m[7])
		d := dir
		if rel != "" {
			d = filepath.Join(dir, filepath.FromSlash(rel))
		}
		comps = append(comps, Component{Type: Type(typ), Name: name, Dir: resolve(d)})
	}
	if depth < maxIncludeDepth {
		for _, m := range includedRe.FindAllSubmatch(src, -1) {
			rel := filepath.FromSlash(unquote(m[2], m[3]))
			if m[1] != nil || !filepath.IsAbs(rel) {
				rel = filepath.Join(dir, rel)
			}
			candidates := []string{rel}
			if strings.ContainsAny(rel, "*?[") {
				candidates, _ = filepath.Glob(rel)
			}
			for _, c := range candidates {
				if c == path {
					continue
				}
				sub, err := parseRegistration(c, depth+1)
				if err == nil {
					comps = append(comps, sub...)
				}
			}
		}
	}
	if len(comps) == 0 {
		return nil, fmt.Errorf("%s: no ComponentRegistrar::register() call found", path)
	}
	return comps, nil
}

// stripComments removes PHP comments, leaving string literals intact so that
// e.g. a glob like '/*/registration.php' is not mistaken for a comment start.
func stripComments(src []byte) []byte {
	out := make([]byte, 0, len(src))
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '\'' || c == '"':
			j := i + 1
			for j < len(src) && src[j] != c {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			j = min(j, len(src)-1)
			out = append(out, src[i:j+1]...)
			i = j
		case c == '#' || (c == '/' && i+1 < len(src) && src[i+1] == '/'):
			for i+1 < len(src) && src[i+1] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := bytes.Index(src[i+2:], []byte("*/"))
			if end < 0 {
				return out
			}
			i += 2 + end + 1
		default:
			out = append(out, c)
		}
	}
	return out
}

func unquote(single, double []byte) string {
	if single != nil {
		return strings.NewReplacer(`\'`, `'`, `\\`, `\`).Replace(string(single))
	}
	if double != nil {
		return strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\$`, `$`).Replace(string(double))
	}
	return ""
}

func resolve(dir string) string {
	dir = filepath.Clean(dir)
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		return r
	}
	return dir
}

// DumpFile is where the cache-clean config dump lives, relative to the base dir.
const DumpFile = "var/cache-clean-config.json"

// FromDump reads the component list from <base>/var/cache-clean-config.json
// ({"app": {...}, "modules": [paths], "themes": [paths]}). ok is false when
// the file does not exist. Names are taken from the component's
// registration.php when present, else guessed from the path.
func FromDump(baseDir string) ([]Component, bool, error) {
	base, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, false, err
	}
	path := filepath.Join(base, filepath.FromSlash(DumpFile))
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var dump struct {
		Modules []string `json:"modules"`
		Themes  []string `json:"themes"`
	}
	if err := json.Unmarshal(data, &dump); err != nil {
		return nil, true, fmt.Errorf("%s: %w", path, err)
	}
	var comps []Component
	seen := map[[2]string]bool{}
	add := func(t Type, paths []string) {
		for _, p := range paths {
			p = filepath.FromSlash(p)
			if !filepath.IsAbs(p) {
				p = filepath.Join(base, p)
			}
			dir := resolve(p)
			name := nameFor(t, dir)
			key := [2]string{string(t), name}
			if seen[key] {
				continue
			}
			seen[key] = true
			comps = append(comps, Component{Type: t, Name: name, Dir: dir})
		}
	}
	add(Module, dump.Modules)
	add(Theme, dump.Themes)
	sort.Slice(comps, func(i, j int) bool {
		if comps[i].Type != comps[j].Type {
			return comps[i].Type < comps[j].Type
		}
		return comps[i].Name < comps[j].Name
	})
	return comps, true, nil
}

func nameFor(t Type, dir string) string {
	if cs, err := ParseRegistration(filepath.Join(dir, "registration.php")); err == nil {
		for _, c := range cs {
			if c.Type == t {
				return c.Name
			}
		}
	}
	// Guess from the path: app/code/Vendor/Module → Vendor_Module,
	// app/design/frontend/Vendor/theme → frontend/Vendor/theme.
	parent := filepath.Dir(dir)
	if t == Module {
		return filepath.Base(parent) + "_" + filepath.Base(dir)
	}
	return filepath.Base(filepath.Dir(parent)) + "/" + filepath.Base(parent) + "/" + filepath.Base(dir)
}
