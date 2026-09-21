package magento

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func opt(t *testing.T, a *App, frontend, key string) string {
	t.Helper()
	cfg, err := a.CacheConfig(a.Base, frontend)
	if err != nil {
		t.Fatal(err)
	}
	return str(cfg["backend_options"].(map[string]any)[key])
}

func TestCacheConfigDefaults(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	write(t, filepath.Join(base, "app/etc/env.php"), "<?php return ['cache_types' => ['config' => 1, 'layout' => 0]];")
	a := New(base)
	if got, want := opt(t, a, "default", "cache_dir"), filepath.Join(base, "var/cache"); got != want {
		t.Errorf("default cache_dir = %q, want %q", got, want)
	}
	if got, want := opt(t, a, "page_cache", "cache_dir"), filepath.Join(base, "var/page_cache"); got != want {
		t.Errorf("page_cache cache_dir = %q, want %q", got, want)
	}
	cfg, _ := a.CacheConfig(base, "default")
	if p := str(cfg["id_prefix"]); len(p) != 4 || p[3] != '_' {
		t.Errorf("id_prefix = %q, want 3 hex chars and an underscore", p)
	}
	if cfg["backend"] != "Cm_Cache_Backend_File" {
		t.Errorf("backend = %v", cfg["backend"])
	}
	disabled, all := a.DisabledCaches()
	if !slices.Equal(disabled, []string{"layout"}) || all {
		t.Errorf("DisabledCaches = %v, %v", disabled, all)
	}
}

func TestCacheConfigRedisAndRelativeDir(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	write(t, filepath.Join(base, "app/etc/env.php"), `<?php return ['cache' => ['frontend' => [
		'default' => ['id_prefix' => 'abc_', 'backend_options' => ['server' => '127.0.0.1', 'port' => '6379']],
		'page_cache' => ['backend_options' => ['cache_dir' => 'var/fpc']],
	]]];`)
	a := New(base)
	cfg, err := a.CacheConfig(base, "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg["backend"]; ok || cfg["id_prefix"] != "abc_" || opt(t, a, "default", "cache_dir") != "" {
		t.Errorf("redis config got file defaults: %v", cfg)
	}
	if got, want := opt(t, a, "page_cache", "cache_dir"), filepath.Join(base, "var/fpc"); got != want {
		t.Errorf("cache_dir = %q, want %q", got, want)
	}
}

func TestFpcDirBugWorkaround(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	write(t, filepath.Join(base, "app/etc/env.php"), "<?php return ['cache' => ['frontend' => ['page_cache' => ['id_prefix' => 'x_']]]];")
	write(t, filepath.Join(base, "vendor/magento/framework/App/Cache/Frontend/Pool.php"), "<?php // array_merge only")
	if got, want := opt(t, New(base), "page_cache", "cache_dir"), filepath.Join(base, "var/cache"); got != want {
		t.Errorf("cache_dir = %q, want %q", got, want)
	}
}

func TestSandboxBaseDirs(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	write(t, filepath.Join(base, "app/etc/env.php"), "<?php return [];")
	sandbox := filepath.Join(base, "dev/tests/integration/tmp/sandbox-0_abc")
	write(t, filepath.Join(sandbox, "etc/env.php"), "<?php return [];")
	write(t, filepath.Join(base, "dev/tests/integration/tmp/other/etc/env.php"), "<?php return [];")
	a := New(base)
	if got := a.AllBaseDirs(); !slices.Equal(got, []string{base, sandbox}) {
		t.Fatalf("AllBaseDirs = %v", got)
	}
	if got, want := EnvFile(sandbox), filepath.Join(sandbox, "etc/env.php"); got != want {
		t.Errorf("EnvFile = %q, want %q", got, want)
	}
	CleanIntegrationSandboxes(base)
	if got := a.AllBaseDirs(); !slices.Equal(got, []string{base}) {
		t.Fatalf("after clean: %v", got)
	}
}

func TestGeneratedCodeRemoval(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	gen := filepath.Join(base, "generated/code")
	keep := filepath.Join(gen, "Acme/Shop/Model/ThingFactory.php")
	gone := []string{
		filepath.Join(gen, "Acme/Shop/Model/Thing/Interceptor.php"),
		filepath.Join(gen, "Acme/Shop/Model/Thing/Proxy.php"),
	}
	for _, f := range append(gone, keep) {
		write(t, f, "<?php")
	}
	src := filepath.Join(base, "app/code/Acme/Shop/Model/Thing.php")
	write(t, src, "<?php\ndeclare(strict_types=1);\n\nnamespace Acme\\Shop\\Model;\n\nabstract class Thing\n{\n}\n")
	if n := RemoveGeneratedForPHP(base, []string{src}); n != len(gone) {
		t.Fatalf("removed %d files, want %d", n, len(gone))
	}
	if !fileExists(keep) {
		t.Error("factory must be kept")
	}

	ext := filepath.Join(gen, "Acme/Shop/Api/Data/ThingExtension.php")
	write(t, ext, "<?php")
	if n := RemoveGeneratedExtensionAttributes(base); n != 1 || fileExists(ext) {
		t.Errorf("extension attributes: removed %d", n)
	}
}

func TestInterceptorsForDiXML(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	plugged := filepath.Join(base, "generated/code/Acme/Shop/Model/Thing/Interceptor.php")
	other := filepath.Join(base, "generated/code/Acme/Shop/Model/Other/Interceptor.php")
	write(t, plugged, "<?php")
	write(t, other, "<?php")
	compiled := filepath.Join(base, "generated/metadata/staticcache/global_primary_frontend_compiled_plugins.php")
	write(t, compiled, "<?php")
	di := filepath.Join(base, "app/code/Acme/Shop/etc/frontend/di.xml")
	write(t, di, `<config>
	<type name="Acme\Shop\Model\Thing"><plugin name="p" type="Acme\Shop\Plugin\P"/></type>
	<type name="Acme\Shop\Model\Other"><arguments/></type>
</config>`)
	CleanCreatuityCache(base, "frontend")
	if fileExists(compiled) {
		t.Error("compiled plugin list not removed")
	}
	if n := RemoveInterceptorsForDiXML(base, di); n != 1 || fileExists(plugged) || !fileExists(other) {
		t.Errorf("removed %d interceptors", n)
	}
}
