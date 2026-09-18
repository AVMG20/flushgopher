package phpconf

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	t.Setenv("MCC_TEST_SET", "hello")
	os.Unsetenv("MCC_TEST_UNSET")
	tests := []struct {
		name string
		src  string
		want any
	}{
		{"empty short array", `<?php return [];`, []any{}},
		{"empty long array", `<?php return array();`, []any{}},
		{"list", `<?php return ['a', "b", 3,];`, []any{"a", "b", int64(3)}},
		{"map", `<?php return ['a' => 1, 'b' => [ 'c' => true ]];`,
			map[string]any{"a": int64(1), "b": map[string]any{"c": true}}},
		{"array() nested", `<?php return array('x' => array(1, 2), 'y' => array());`,
			map[string]any{"x": []any{int64(1), int64(2)}, "y": []any{}}},
		{"int keys stringified", `<?php return [5 => 'a', 'b', '7' => 'c', 'd', '08' => 'e'];`,
			map[string]any{"5": "a", "6": "b", "7": "c", "8": "d", "08": "e"}},
		{"duplicate key overwrites", `<?php return ['a' => 1, 'a' => 2];`, map[string]any{"a": int64(2)}},
		{"bool/float keys", `<?php return [true => 'a', 2.7 => 'b'];`, map[string]any{"1": "a", "2": "b"}},
		{"comments", "<?php\n// line\n# hash\n/* block\n */ return [ /* x */ 'a' => 1, // c\n 'b' => 2 # d\n];",
			map[string]any{"a": int64(1), "b": int64(2)}},
		{"declare use namespace", "<?php\ndeclare(strict_types=1);\nnamespace Foo\\Bar;\nuse Magento\\Framework\\Config\\ConfigOptionsListConstants as C;\nuse PDO;\nreturn ['a' => 1];",
			map[string]any{"a": int64(1)}},
		{"single quote escapes", `<?php return ['it\'s \\ \n $x'];`, []any{`it's \ \n $x`}},
		{"double quote escapes", `<?php return ["a\tb\n\\\"\$x \x41\101\u{1F600}\q $"];`, []any{"a\tb\n\\\"$x AA\U0001F600\\q $"}},
		{"numbers", `<?php return [-1, 0x1F, 0b101, 0o17, 017, 1_000, 1.5, -2.5e3, 1e2];`,
			[]any{int64(-1), int64(31), int64(5), int64(15), int64(15), int64(1000), 1.5, -2500.0, 100.0}},
		{"keywords case-insensitive", `<?php return [TRUE, False, NULL, true];`, []any{true, false, nil, true}},
		{"concat", `<?php return ['a' . 'b' . 1 . true . null . 1.5];`, []any{"ab111.5"}},
		{"class constants", `<?php return [\PDO::MYSQL_ATTR_SSL_CA => 'x', PDO::MYSQL_ATTR_INIT_COMMAND => 'SET NAMES utf8', Foo\Bar::class => 1];`,
			map[string]any{"PDO::MYSQL_ATTR_SSL_CA": "x", "PDO::MYSQL_ATTR_INIT_COMMAND": "SET NAMES utf8", `Foo\Bar`: int64(1)}},
		{"getenv", `<?php return [getenv('MCC_TEST_SET'), \getenv("MCC_TEST_UNSET"), getenv('MCC_TEST_UNSET') ?: 'dflt', getenv('MCC_TEST_SET') ?: 'dflt'];`,
			[]any{"hello", false, "dflt", "hello"}},
		{"coalesce", `<?php return [null ?? 'a', 'b' ?? 'c', false ?? 'd', null ?? null ?? 3];`, []any{"a", "b", false, int64(3)}},
		{"ternary", `<?php return [0 ? 'a' : 'b', '0' ?: 'c', 'x' ?: 'y'];`, []any{"b", "c", "x"}},
		{"casts", `<?php return [(int)'42abc', (int) "0x1A", (string)5, (bool)'0', (bool)'a', ( int )3.9, (float)'1.5', (integer)true];`,
			[]any{int64(42), int64(0), "5", false, true, int64(3), 1.5, int64(1)}},
		{"cast getenv", `<?php return ['port' => (int) (getenv('MCC_TEST_UNSET') ?: 6379)];`, map[string]any{"port": int64(6379)}},
		{"parens", `<?php return (['a']);`, []any{"a"}},
		{"closing tag", "<?php\nreturn ['a'] ?>\n", []any{"a"}},
		{"no trailing semicolon at eof", "<?php return ['a']", []any{"a"}},
		{"heredoc", "<?php return [<<<EOT\n  a\\tb\n    c\n  EOT, <<<'N'\nx\\ty\nN\n];", []any{"a\tb\n  c", `x\ty`}},
		{"nested mixed", `<?php return ['db' => ['connection' => ['default' => ['host' => 'localhost', 'port' => '3306', 'active' => '1', 'driver_options' => [1014 => false]]]], 'x' => [ ['a'], ['b'] ]];`,
			map[string]any{
				"db": map[string]any{"connection": map[string]any{"default": map[string]any{
					"host": "localhost", "port": "3306", "active": "1",
					"driver_options": map[string]any{"1014": false},
				}}},
				"x": []any{[]any{"a"}, []any{"b"}},
			}},
		{"trailing dot float", `<?php return [1., 2.e1, 3 . 4];`, []any{1.0, 20.0, "34"}},
		{"large int overflow to float", `<?php return [9223372036854775808];`, []any{9223372036854775808.0}},
		{"leading whitespace and shebang", "#!/usr/bin/env php\n  <?php return [1];", []any{int64(1)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse([]byte(tt.src))
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %#v\nwant %#v", got, tt.want)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name, src, wantErr string
	}{
		{"no open tag", `return [];`, "open tag"},
		{"variable", `<?php return [$foo];`, "variables are not supported"},
		{"assignment", `<?php $a = 1; return [];`, "unsupported statement"},
		{"interpolation", `<?php return ["a $b"];`, "interpolation"},
		{"interpolation braces", `<?php return ["a {$b}"];`, "interpolation"},
		{"function call", `<?php return [foo('x')];`, "function calls are not supported"},
		{"static call", `<?php return [Foo::bar()];`, "static method calls"},
		{"unknown constant", `<?php return [FOO_BAR];`, "unknown constant"},
		{"unterminated string", `<?php return ['abc];`, "unterminated string"},
		{"unterminated array", `<?php return ['abc'`, "expected"},
		{"unterminated comment", `<?php /* return [];`, "unterminated comment"},
		{"no return", `<?php declare(strict_types=1);`, "no return"},
		{"missing comma", `<?php return ['a' 'b'];`, "expected"},
		{"array cast", `<?php return (array) 'x';`, "unsupported cast"},
		{"concat array", `<?php return 'a' . [];`, "array to string"},
		{"arithmetic", `<?php return [1 * 2];`, "unexpected character"},
		{"hex without digits", `<?php return [0x];`, "invalid number"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.src))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestErrorLine(t *testing.T) {
	_, err := Parse([]byte("<?php\nreturn [\n  'a' => $x,\n];"))
	se, ok := err.(*SyntaxError)
	if !ok || se.Line != 3 {
		t.Fatalf("want SyntaxError on line 3, got %v", err)
	}
}

func TestLoadConfigNative(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "env.php")
	os.WriteFile(p, []byte(`<?php return ['backend' => ['frontName' => 'admin']];`), 0o644)
	m, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if m["backend"].(map[string]any)["frontName"] != "admin" {
		t.Fatalf("unexpected %v", m)
	}
	os.WriteFile(p, []byte(`<?php return [];`), 0o644)
	m, err = LoadConfig(p)
	if err != nil || len(m) != 0 || m == nil {
		t.Fatalf("empty config: %v %v", m, err)
	}
	if _, err := LoadConfig(filepath.Join(dir, "missing.php")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: want ErrNotExist without php fallback, got %v", err)
	}
}

func TestLoadConfigPHPFallback(t *testing.T) {
	if _, err := exec.LookPath(PHPBinary); err != nil {
		t.Skip("php not available")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "it's env.php") // quote in path must be safe
	os.WriteFile(p, []byte(`<?php $x = ['a' => 1.0, 'b' => [], 'c' => [1, 2], 'd' => 2]; return $x;`), 0o644)
	m, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": 1.0, "b": []any{}, "c": []any{int64(1), int64(2)}, "d": int64(2)}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("got %#v want %#v", m, want)
	}

	os.WriteFile(p, []byte(`<?php syntax error here`), 0o644)
	if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), "native parse failed") || !strings.Contains(err.Error(), "php fallback failed") {
		t.Fatalf("want combined error, got %v", err)
	}
}

// TestRealMagentoConfigs compares the native parser with php for every
// Magento install under ~/sites. Only key paths are reported, never values.
func TestRealMagentoConfigs(t *testing.T) {
	if _, err := exec.LookPath(PHPBinary); err != nil {
		t.Skip("php not available")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	var files []string
	for _, name := range []string{"env.php", "config.php"} {
		m, _ := filepath.Glob(filepath.Join(home, "sites", "*", "app", "etc", name))
		files = append(files, m...)
	}
	if len(files) == 0 {
		t.Skip("no Magento installs found under ~/sites")
	}
	sort.Strings(files)
	for _, f := range files {
		rel := strings.TrimPrefix(f, home+string(filepath.Separator))
		t.Run(rel, func(t *testing.T) {
			native, err := ParseFile(f)
			if err != nil {
				// Error messages carry no values beyond position info.
				t.Fatalf("native parse failed: %v", err)
			}
			viaPHP, err := PHPEval(f)
			if err != nil {
				t.Skipf("php evaluation failed: %v", err)
			}
			var diffs []string
			compareValues("", normalize(native), normalize(viaPHP), &diffs)
			for _, d := range diffs {
				t.Errorf("mismatch at %s", d)
			}
			t.Logf("native and php agree (%d top-level keys)", topLen(native))
		})
	}
}

func topLen(v any) int {
	switch x := v.(type) {
	case map[string]any:
		return len(x)
	case []any:
		return len(x)
	}
	return 0
}

// normalize converts maps with keys "0".."n-1" to lists and empty maps to
// empty lists, mirroring json_encode's list detection.
func normalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		if len(x) == 0 {
			return []any{}
		}
		list := make([]any, len(x))
		isList := true
		for i := range list {
			e, ok := x[strconv.Itoa(i)]
			if !ok {
				isList = false
				break
			}
			list[i] = normalize(e)
		}
		if isList {
			return list
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = normalize(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = normalize(e)
		}
		return out
	}
	return v
}

func compareValues(path string, a, b any, diffs *[]string) {
	if path == "" {
		path = "<root>"
	}
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			*diffs = append(*diffs, path+" (type differs)")
			return
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok {
				*diffs = append(*diffs, path+"."+k+" (missing in php output)")
				continue
			}
			compareValues(path+"."+k, v, w, diffs)
		}
		for k := range y {
			if _, ok := x[k]; !ok {
				*diffs = append(*diffs, path+"."+k+" (missing in native output)")
			}
		}
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			*diffs = append(*diffs, path+" (list type or length differs)")
			return
		}
		for i := range x {
			compareValues(path+"["+strconv.Itoa(i)+"]", x[i], y[i], diffs)
		}
	default:
		if !reflect.DeepEqual(a, b) {
			*diffs = append(*diffs, path+" (value differs)")
		}
	}
}
