package storage

import (
	"path/filepath"
	"testing"
)

func TestSymfonyBackendSelection(t *testing.T) {
	cases := []struct {
		backend, want string
	}{
		{"redis", "redis"},
		{"valkey", "redis"},
		{"Valkey", "redis"},
		{"file", "file"},
		{"", "file"},
		// Not in SymfonyAdapterProvider's map: Magento falls back to the filesystem.
		{`\Magento\Framework\Cache\Backend\Redis`, "file"},
		{"Cm_Cache_Backend_Redis", "file"},
	}
	for _, c := range cases {
		s, err := New(Config{SymfonyKey: true, "backend": c.backend, "id_prefix": "3ec_", "backend_options": map[string]any{
			"cache_dir": "/tmp/x", "server": "127.0.0.1",
		}})
		if err != nil {
			t.Fatalf("%s: %v", c.backend, err)
		}
		var got string
		switch s.(type) {
		case *symfonyRedisWithFallback:
			got = "redis"
		case *symfonyFile:
			got = "file"
		}
		if got != c.want {
			t.Errorf("%q: got %s (%s), want %s", c.backend, got, s.Describe(), c.want)
		}
	}
}

func TestSymfonyFilePath(t *testing.T) {
	// base64(hash('xxh128', FilesystemAdapter::class.'LAYOUT_FRONTEND_STORE1_12_HYVA', true))
	// in PHP is Y/oMamIBlC6+i9PTqOrZ8w==
	f := &symfonyFile{dir: "/c/3ec_"}
	if got, want := f.idPath("LAYOUT_FRONTEND_STORE1_12_HYVA"), "/c/3ec_/Y/-/oMamIBlC6+i9PTqOrZ8w"; got != want {
		t.Errorf("idPath = %q, want %q", got, want)
	}
}

func TestSymfonyFile(t *testing.T) {
	dir := t.TempDir()
	tagDir := filepath.Join(dir, "symfony/tags")
	s, err := New(Config{SymfonyKey: true, SymfonyTagDirKey: tagDir, "backend": "file", "id_prefix": "3ec_",
		"backend_options": map[string]any{"cache_dir": dir}})
	if err != nil {
		t.Fatal(err)
	}
	f := s.(*symfonyFile)
	for _, id := range []string{"A", "B", "C", "D"} {
		writeFile(t, f.idPath(id), "data")
	}
	writeFile(t, filepath.Join(tagDir, "CONFIG"), "A\nB\n")
	writeFile(t, filepath.Join(tagDir, "LAYOUT"), "B\n")
	writeFile(t, filepath.Join(tagDir, "OTHER"), "C\nD\n")

	if err := s.CleanTags([]string{"3ec_CONFIG", "3ec_LAYOUT", "3ec_MISSING", "3ec_../x"}); err != nil {
		t.Fatal(err)
	}
	if exists(f.idPath("A")) || exists(f.idPath("B")) {
		t.Error("tagged records not removed")
	}
	if !exists(f.idPath("C")) || !exists(f.idPath("D")) {
		t.Error("untagged records removed")
	}
	if !exists(filepath.Join(tagDir, "CONFIG")) {
		t.Error("shared tag file removed")
	}

	if err := s.CleanIDs([]string{"3ec_C", "3ec_MISSING"}); err != nil {
		t.Fatal(err)
	}
	if exists(f.idPath("C")) || !exists(f.idPath("D")) {
		t.Error("CleanIDs removed the wrong records")
	}

	writeFile(t, filepath.Join(dir, "other.txt"), "keep")
	if err := s.CleanAll(); err != nil {
		t.Fatal(err)
	}
	if exists(f.idPath("D")) || exists(filepath.Join(dir, "3ec_")) {
		t.Error("CleanAll left records behind")
	}
	if !exists(filepath.Join(dir, "other.txt")) || !exists(filepath.Join(tagDir, "OTHER")) {
		t.Error("CleanAll removed files outside the namespace")
	}
}

// seedSymfony stores an id with tags the way RedisAdapter + RedisTagAdapter do.
func seedSymfony(t *testing.T, c *respConn, ns, id string, tags ...string) {
	t.Helper()
	cmds := [][]string{{"SET", ns + ":" + id, "data"}, {"SADD", symfonyAllIDs, id}}
	for _, tag := range tags {
		cmds = append(cmds, []string{"SADD", symfonyTagPrefix + ns + tag, id}, []string{"SADD", symfonyIDTagsPrefix + ns + id, tag})
	}
	mustDo(t, c, cmds...)
}

func TestSymfonyRedis(t *testing.T) {
	port := freePort(t)
	startRedis(t, "--port", port, "--bind", "127.0.0.1")
	raw := rawClient(t, "tcp", "127.0.0.1:"+port)
	mustDo(t, raw, []string{"SELECT", "3"})

	dir := t.TempDir()
	tagDir := filepath.Join(dir, "symfony/tags")
	s, err := New(Config{SymfonyKey: true, SymfonyTagDirKey: tagDir, "backend": "valkey", "id_prefix": "3ec_",
		"backend_options": map[string]any{"server": "127.0.0.1", "port": port, "database": 3, "cache_dir": dir}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Many ids to exercise chunking.
	var many []string
	for i := 0; i < 1234; i++ {
		many = append(many, "BIG_"+itoa(i))
	}
	for _, id := range many {
		seedSymfony(t, raw, "3ec_", id, "BIG")
	}
	seedSymfony(t, raw, "3ec_", "A", "CONFIG")
	seedSymfony(t, raw, "3ec_", "B", "CONFIG", "LAYOUT_GENERAL_CACHE_TAG")
	seedSymfony(t, raw, "3ec_", "C", "OTHER")
	seedSymfony(t, raw, "3ec_", "D", "OTHER")

	// Records Magento wrote to the file fallback must be cleaned too.
	fallback := s.(*symfonyRedisWithFallback).file
	writeFile(t, fallback.idPath("A"), "data")
	writeFile(t, filepath.Join(tagDir, "CONFIG"), "A\n")

	if err := s.CleanTags([]string{"3ec_LAYOUT_GENERAL_CACHE_TAG", "3ec_CONFIG", "3ec_BIG"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range append([]string{"A", "B"}, many...) {
		if keyExists(t, raw, "3ec_:"+id) || keyExists(t, raw, symfonyIDTagsPrefix+"3ec_"+id) || isMember(t, raw, symfonyAllIDs, id) {
			t.Fatalf("%s not fully removed", id)
		}
	}
	for _, tag := range []string{"CONFIG", "LAYOUT_GENERAL_CACHE_TAG", "BIG"} {
		if keyExists(t, raw, symfonyTagPrefix+"3ec_"+tag) {
			t.Errorf("tag set %s not removed", tag)
		}
	}
	if !keyExists(t, raw, "3ec_:C") || !keyExists(t, raw, symfonyTagPrefix+"3ec_OTHER") {
		t.Error("untagged record removed")
	}
	if exists(fallback.idPath("A")) {
		t.Error("file fallback record not removed")
	}

	if err := s.CleanIDs([]string{"3ec_C"}); err != nil {
		t.Fatal(err)
	}
	if keyExists(t, raw, "3ec_:C") || keyExists(t, raw, symfonyIDTagsPrefix+"3ec_C") || isMember(t, raw, symfonyAllIDs, "C") {
		t.Error("CleanIDs did not remove C")
	}
	if !keyExists(t, raw, "3ec_:D") {
		t.Error("CleanIDs removed D")
	}

	if err := s.CleanAll(); err != nil {
		t.Fatal(err)
	}
	if n := mustDo(t, raw, []string{"DBSIZE"})[0].(int64); n != 0 {
		t.Errorf("CleanAll left %d keys", n)
	}
}
