package magento

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"flushgopher/internal/logx"
	"flushgopher/internal/rules"
)

// GeneratedCodeDir returns generated/code (or var/generation on old versions), or "".
func GeneratedCodeDir(base string) string {
	for _, c := range []string{"generated/code", "var/generation"} {
		if d := filepath.Join(base, c); dirExists(d) {
			return d
		}
	}
	return ""
}

func classFile(dir, class string) string {
	return filepath.Join(dir, filepath.FromSlash(strings.ReplaceAll(strings.TrimPrefix(class, `\`), `\`, "/"))+".php")
}

// generatedClassesFor lists classes Magento may generate for class.
// Factories are left alone since they don't depend on the source class body.
func generatedClassesFor(class string) []string {
	out := []string{
		class + "Converter",
		class + "InterfaceFactory",
		class + `\Interceptor`,
		class + `\Logger`,
		class + "Mapper",
		class + "Persistor",
		class + `\Proxy`,
	}
	if base, ok := strings.CutSuffix(class, "Interface"); ok {
		out = append(out, base+"Extension", base+"ExtensionInterface", base+`\Repository`)
	}
	return out
}

func removeFiles(base, what string, files []string) int {
	var removed []string
	for _, f := range files {
		if err := os.Remove(f); err == nil {
			rel, _ := filepath.Rel(base, f)
			removed = append(removed, rel)
		}
	}
	if len(removed) > 0 {
		if len(removed) > 3 && !logx.Enabled(logx.Info) {
			logx.Event(logx.Notice, logx.CGen, "GEN", "removed %s %s", logx.Bold(strconv.Itoa(len(removed))), what)
		} else {
			logx.Event(logx.Notice, logx.CGen, "GEN", "removed %s: %s", what, logx.Paint(logx.CGen, strings.Join(removed, ", ")))
		}
	}
	return len(removed)
}

// RemoveGeneratedForPHP removes generated classes (interceptors, proxies, ...)
// of the classes declared in the given PHP files.
func RemoveGeneratedForPHP(base string, phpFiles []string) int {
	dir := GeneratedCodeDir(base)
	if dir == "" || len(phpFiles) == 0 {
		return 0
	}
	var candidates []string
	for _, f := range phpFiles {
		class := rules.PHPClass(rules.Head(f, 4096))
		if class == "" {
			continue
		}
		for _, g := range generatedClassesFor(class) {
			candidates = append(candidates, classFile(dir, g))
		}
	}
	return removeFiles(base, "generated code", candidates)
}

// RemoveGeneratedExtensionAttributes removes all generated *Extension and
// *ExtensionInterface classes, which depend on extension_attributes.xml.
func RemoveGeneratedExtensionAttributes(base string) int {
	dir := GeneratedCodeDir(base)
	if dir == "" {
		return 0
	}
	var files []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && (strings.HasSuffix(p, "Extension.php") || strings.HasSuffix(p, "ExtensionInterface.php")) {
			files = append(files, p)
		}
		return nil
	})
	return removeFiles(base, "generated extension attribute classes", files)
}

var pluginTypeRe = regexp.MustCompile(`<type\b[^>]*\bname\s*=\s*['"]([^'"]+)['"][^>]*>[\s\S]*?</type>`)

// RemoveInterceptorsForDiXML removes generated interceptors of classes with
// plugins declared in di.xml. Only needed with the Creatuity interception
// cache (generated/metadata/staticcache), otherwise Magento handles it.
func RemoveInterceptorsForDiXML(base, diXML string) int {
	if !dirExists(filepath.Join(base, "generated/metadata/staticcache")) {
		return 0
	}
	dir := GeneratedCodeDir(base)
	b, err := os.ReadFile(diXML)
	if dir == "" || err != nil {
		return 0
	}
	var files []string
	for _, m := range pluginTypeRe.FindAllSubmatch(b, -1) {
		if strings.Contains(string(m[0]), "<plugin") {
			files = append(files, classFile(dir, string(m[1])+`\Interceptor`))
		}
	}
	return removeFiles(base, "generated interceptors", files)
}

// CleanCreatuityCache removes the compiled plugin list of the Creatuity
// interception cache, for one area or all of them.
func CleanCreatuityCache(base, area string) {
	for _, d := range []string{"generated/staticcache", "generated/metadata/staticcache"} {
		dir := filepath.Join(base, d)
		if !dirExists(dir) {
			continue
		}
		if area != "" {
			f := filepath.Join(dir, "global_primary_"+area+"_compiled_plugins.php")
			if os.Remove(f) == nil {
				logx.Event(logx.Notice, logx.CGen, "GEN", "removed creatuity interceptor cache %s", f)
			}
			continue
		}
		logx.Event(logx.Notice, logx.CGen, "GEN", "removing creatuity interceptors cache %s", dir)
		removeContents(dir)
	}
}

func removeContents(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// CleanGeneratedCode removes the generated code directory.
func CleanGeneratedCode(base string) {
	dir := GeneratedCodeDir(base)
	if dir == "" {
		logx.Event(logx.Notice, logx.CGen, "GEN", "no generated code directory found")
		return
	}
	logx.Event(logx.Notice, logx.CGen, "GEN", "removing %s", dir)
	os.RemoveAll(dir)
}

// themeLocaleDirs returns <root>/<area>/<Vendor>/<theme>/<locale> dirs.
func themeLocaleDirs(root, area string) []string {
	m, _ := filepath.Glob(filepath.Join(root, area, "*", "*", "*"))
	var out []string
	for _, d := range m {
		if dirExists(d) {
			out = append(out, d)
		}
	}
	return out
}

// StaticThemeLocaleDirs returns pub/static/<area>/<Vendor>/<theme>/<locale> dirs.
func StaticThemeLocaleDirs(base, area string) []string {
	return themeLocaleDirs(filepath.Join(base, "pub/static"), area)
}

// RemoveJSTranslations removes compiled js-translation.json files of an area.
func RemoveJSTranslations(base, area string) int {
	dirs := append(StaticThemeLocaleDirs(base, area), themeLocaleDirs(filepath.Join(base, "var/view_preprocessed/pub/static"), area)...)
	var files []string
	for _, d := range dirs {
		files = append(files, filepath.Join(d, "js-translation.json"))
	}
	return removeFiles(base, "compiled js-translation.json files", files)
}

// CleanStaticArea removes the static files of an area, keeping the directories.
func CleanStaticArea(base, area string) {
	logx.Event(logx.Notice, logx.CGen, "STATIC", "removing static content area %s", area)
	for _, d := range []string{
		filepath.Join(base, "pub/static", area),
		filepath.Join(base, "var/view_preprocessed/pub/static", area),
		filepath.Join(base, "var/view_preprocessed/pub/static/app"),
		filepath.Join(base, "var/view_preprocessed/pub/static/vendor"),
	} {
		filepath.WalkDir(d, func(p string, e os.DirEntry, err error) error {
			if err == nil && !e.IsDir() {
				os.Remove(p)
			}
			return nil
		})
	}
}

// CleanIntegrationSandboxes removes dev/tests/integration/tmp/sandbox-* dirs.
func CleanIntegrationSandboxes(base string) {
	tmp := filepath.Join(base, "dev/tests/integration/tmp")
	if !dirExists(tmp) {
		logx.Event(logx.Notice, logx.CGen, "TESTS", "integration test tmp directory %s not found", tmp)
		return
	}
	logx.Event(logx.Notice, logx.CGen, "TESTS", "removing integration test sandboxes in %s", tmp)
	m, _ := filepath.Glob(filepath.Join(tmp, "sandbox-*"))
	for _, d := range m {
		os.RemoveAll(d)
	}
}
