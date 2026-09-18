package storage

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewBackendSelection(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string // type name
	}{
		{"cm redis", Config{"backend": "Cm_Cache_Backend_Redis"}, "redis"},
		{"magento redis", Config{"backend": `\Magento\Framework\Cache\Backend\Redis`}, "redis"},
		{"magento redis no slash lower", Config{"backend": `magento\framework\cache\backend\redis`}, "redis"},
		{"server implies redis", Config{"backend_options": map[string]any{"server": "redis", "port": "6379"}}, "redis"},
		{"file", Config{"backend": "Cm_Cache_Backend_File", "backend_options": map[string]any{"cache_dir": "/tmp/x"}}, "file"},
		{"default file", Config{"backend_options": map[string]any{"cache_dir": "/tmp/x"}}, "file"},
		{"remote sync", Config{
			"backend":   `\Magento\Framework\Cache\Backend\RemoteSynchronizedCache`,
			"id_prefix": "69d_",
			"backend_options": map[string]any{
				"remote_backend":         `Magento\Framework\Cache\Backend\Redis`,
				"remote_backend_options": map[string]any{"server": "redis", "database": 0, "port": 6379},
				"local_backend":          "Cm_Cache_Backend_File",
				"local_backend_options":  map[string]any{"cache_dir": "/dev/shm/", "file_name_prefix": "pc"},
			},
		}, "remote"},
	}
	for _, c := range cases {
		s, err := New(c.cfg)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var got string
		switch s.(type) {
		case *redisStorage:
			got = "redis"
		case *fileStorage:
			got = "file"
		case *remoteSynchronized:
			got = "remote"
		}
		if got != c.want {
			t.Errorf("%s: got %s (%s), want %s", c.name, got, s.Describe(), c.want)
		}
	}
	if _, err := New(Config{}); !errors.Is(err, errNoCacheDir) {
		t.Errorf("missing cache_dir: got %v", err)
	}
}

func TestRedisOptions(t *testing.T) {
	cases := []struct {
		opts          map[string]any
		network, addr string
		tls           bool
		db            int
		user, pass    string
	}{
		{map[string]any{"server": "127.0.0.1", "port": "6379", "database": "15"}, "tcp", "127.0.0.1:6379", false, 15, "", ""},
		{map[string]any{"server": "redis", "port": 6380.0, "database": int64(2), "password": "pw"}, "tcp", "redis:6380", false, 2, "", "pw"},
		{map[string]any{"server": "tcp://cache", "port": nil}, "tcp", "cache:6379", false, 0, "", ""},
		{map[string]any{"server": "tls://secure.example", "port": 6390, "user": "u", "password": "p"}, "tcp", "secure.example:6390", true, 0, "u", "p"},
		{map[string]any{"server": "unix:///var/run/redis.sock"}, "unix", "/var/run/redis.sock", false, 0, "", ""},
		{map[string]any{"server": "/tmp/r.sock", "database": 3}, "unix", "/tmp/r.sock", false, 3, "", ""},
		{map[string]any{"server": "tcp://host:7000"}, "tcp", "host:7000", false, 0, "", ""},
		{map[string]any{}, "tcp", "127.0.0.1:6379", false, 0, "", ""},
	}
	for _, c := range cases {
		o, err := parseRedisOptions(c.opts)
		if err != nil {
			t.Fatalf("%v: %v", c.opts, err)
		}
		if o.network != c.network || o.addr != c.addr || o.useTLS != c.tls || o.database != c.db || o.username != c.user || o.password != c.pass {
			t.Errorf("%v: got %+v", c.opts, o)
		}
	}
}

// ---- file backend ----

func md5Suffix(id string, n int) string {
	s := md5.Sum([]byte(id))
	h := hex.EncodeToString(s[:])
	return h[len(h)-n:]
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}

func TestFileBackend(t *testing.T) {
	for _, level := range []int{0, 1, 2} {
		dir := t.TempDir()
		s, err := New(Config{"backend": "Cm_Cache_Backend_File", "backend_options": map[string]any{
			"cache_dir": dir, "hashed_directory_level": level,
		}})
		if err != nil {
			t.Fatal(err)
		}
		idFile := func(id string) string {
			if level == 0 {
				return filepath.Join(dir, "mage---"+id)
			}
			return filepath.Join(dir, "mage--"+md5Suffix(id, level), "mage---"+id)
		}
		tagFile := func(tag string) string { return filepath.Join(dir, "mage-tags", "mage---"+tag) }

		ids := []string{"69D_CONFIG_SCOPES", "69D_LAYOUT_A", "69D_LAYOUT_B", "69D_OTHER", "69D_SHARED"}
		for _, id := range ids {
			writeFile(t, idFile(id), "data")
		}
		writeFile(t, tagFile("69D_CONFIG"), "69D_CONFIG_SCOPES\n69D_SHARED\n\n69D_MISSING\n")
		writeFile(t, tagFile("69D_LAYOUT_GENERAL_CACHE_TAG"), "69D_LAYOUT_A\r\n69D_LAYOUT_B\n69D_SHARED\n")
		writeFile(t, tagFile("69D_UNRELATED"), "69D_OTHER\n")

		if err := s.CleanTags([]string{"69D_CONFIG", "69D_LAYOUT_GENERAL_CACHE_TAG", "69D_NOPE"}); err != nil {
			t.Fatalf("level %d CleanTags: %v", level, err)
		}
		for _, id := range []string{"69D_CONFIG_SCOPES", "69D_LAYOUT_A", "69D_LAYOUT_B", "69D_SHARED"} {
			if exists(idFile(id)) {
				t.Errorf("level %d: %s still exists", level, id)
			}
		}
		for _, tag := range []string{"69D_CONFIG", "69D_LAYOUT_GENERAL_CACHE_TAG"} {
			if fi, err := os.Stat(tagFile(tag)); err != nil || fi.Size() != 0 {
				t.Errorf("level %d: tag file %s not truncated", level, tag)
			}
		}
		if !exists(idFile("69D_OTHER")) || !exists(tagFile("69D_UNRELATED")) {
			t.Errorf("level %d: unrelated entries removed", level)
		}

		if err := s.CleanIDs([]string{"69D_OTHER", "69D_NOT_THERE", "../evil"}); err != nil {
			t.Fatal(err)
		}
		if exists(idFile("69D_OTHER")) {
			t.Errorf("level %d: CleanIDs did not remove file", level)
		}

		writeFile(t, idFile("X"), "x")
		writeFile(t, filepath.Join(dir, "pc--a", "pc---OTHER_PREFIX"), "x")
		if err := s.CleanAll(); err != nil {
			t.Fatal(err)
		}
		entries, _ := os.ReadDir(dir)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		if strings.Join(names, ",") != "mage-tags,pc--a" {
			t.Errorf("level %d: CleanAll left %v, want only mage-tags and pc--a", level, names)
		}
		if tagEntries, _ := os.ReadDir(filepath.Join(dir, "mage-tags")); len(tagEntries) != 0 {
			t.Errorf("level %d: CleanAll left %d tag files", level, len(tagEntries))
		}
	}

	// Custom prefix, default level 1, missing dir is fine.
	dir := filepath.Join(t.TempDir(), "missing")
	s, _ := New(Config{"backend_options": map[string]any{"cache_dir": dir, "file_name_prefix": "pc"}})
	if err := s.CleanAll(); err != nil {
		t.Fatal(err)
	}
	if err := s.CleanTags([]string{"T"}); err != nil {
		t.Fatal(err)
	}
	if got := s.(*fileStorage).idPath("ABC"); got != filepath.Join(dir, "pc--"+md5Suffix("ABC", 1), "pc---ABC") {
		t.Errorf("idPath = %s", got)
	}
}

// ---- varnish ----

type purgeRecorder struct {
	mu       sync.Mutex
	methods  []string
	patterns []string
}

func (p *purgeRecorder) handler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.methods = append(p.methods, r.Method)
		p.patterns = append(p.patterns, r.Header.Get("X-Magento-Tags-Pattern"))
		p.mu.Unlock()
		w.WriteHeader(status)
		w.Write([]byte(body))
	}
}

func hostOf(t *testing.T, srv *httptest.Server) VarnishHost {
	h, p, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	return VarnishHost{Host: h, Port: p}
}

func TestVarnish(t *testing.T) {
	rec := &purgeRecorder{}
	ok := httptest.NewServer(rec.handler(200, "ok"))
	defer ok.Close()
	rec2 := &purgeRecorder{}
	ok2 := httptest.NewServer(rec2.handler(503, `<html><head><title>200 Purged</title></head><body>done</body></html>`))
	defer ok2.Close()

	v := NewVarnish([]VarnishHost{hostOf(t, ok), hostOf(t, ok2)})
	if err := v.CleanTags([]string{"cat_p_1", "cat_c_2", "cat_p_1"}); err != nil {
		t.Fatal(err)
	}
	want := "((^|,)cat_p_1(,|$))|((^|,)cat_c_2(,|$))"
	if len(rec.patterns) != 1 || rec.patterns[0] != want || rec.methods[0] != "PURGE" {
		t.Errorf("got %v %v", rec.methods, rec.patterns)
	}
	if len(rec2.patterns) != 1 {
		t.Errorf("second host not purged")
	}
	if err := v.CleanAll(); err != nil {
		t.Fatal(err)
	}
	if rec.patterns[1] != ".*" {
		t.Errorf("CleanAll pattern %q", rec.patterns[1])
	}
	if err := v.CleanIDs([]string{"X"}); err != nil || len(rec.patterns) != 2 {
		t.Errorf("CleanIDs should be no-op")
	}

	bad := httptest.NewServer((&purgeRecorder{}).handler(405, "<html><title>405 Method Not Allowed</title><body>Method not allowed</body></html>"))
	defer bad.Close()
	err := NewVarnish([]VarnishHost{hostOf(t, bad)}).CleanAll()
	if err == nil || !strings.Contains(err.Error(), "405") || !strings.Contains(err.Error(), "Method not allowed") {
		t.Errorf("expected 405 error, got %v", err)
	}

	// Unreachable host: no error, and skipped until the down period passes.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().(*net.TCPAddr)
	l.Close()
	vs := NewVarnish([]VarnishHost{{Host: "127.0.0.1", Port: itoa(addr.Port)}}).(*varnishStorage)
	now := time.Now()
	vs.now = func() time.Time { return now }
	if err := vs.CleanAll(); err != nil {
		t.Fatalf("unreachable: %v", err)
	}
	if vs.available("127.0.0.1:" + itoa(addr.Port)) {
		t.Error("host should be marked down")
	}
	now = now.Add(61 * time.Second)
	if !vs.available("127.0.0.1:" + itoa(addr.Port)) {
		t.Error("host should be available again")
	}

	if err := NewVarnish(nil).CleanAll(); err != nil {
		t.Error(err)
	}
}

func itoa(n int) string { return str(n) }

func TestVarnishHostsFromConfig(t *testing.T) {
	got := VarnishHostsFromConfig([]any{
		map[string]any{"host": "varnish", "port": "6081"},
		map[string]any{"host": "10.0.0.1:8080"},
		map[string]any{"port": 81},
		map[string]any{"host": "v2"},
	})
	want := []VarnishHost{{"varnish", "6081"}, {"10.0.0.1", "8080"}, {"localhost", "81"}, {"v2", "80"}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: got %v want %v", i, got[i], want[i])
		}
	}
	if got := VarnishHostsFromConfig(nil); len(got) != 0 {
		t.Errorf("nil: %v", got)
	}
	if got := VarnishHostsFromConfig(map[string]any{"1": map[string]any{"host": "b"}, "0": map[string]any{"host": "a"}}); len(got) != 2 || got[0].Host != "a" {
		t.Errorf("map list: %v", got)
	}
}

// ---- redis ----

func startRedis(t *testing.T, args ...string) {
	t.Helper()
	bin, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server not found")
	}
	dir := t.TempDir()
	cmd := exec.Command(bin, append([]string{"--save", "", "--appendonly", "no", "--dir", dir}, args...)...)
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
}

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return itoa(l.Addr().(*net.TCPAddr).Port)
}

// rawClient returns a raw RESP connection, waiting for the server to come up.
func rawClient(t *testing.T, network, addr string) *respConn {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.Dial(network, addr)
		if err == nil {
			rc := newRespConn(c)
			if _, err := rc.do([][]string{{"PING"}}); err == nil {
				t.Cleanup(func() { c.Close() })
				return rc
			}
			c.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("redis at %s did not start: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func mustDo(t *testing.T, c *respConn, cmds ...[]string) []any {
	t.Helper()
	r, err := c.do(cmds)
	if err != nil {
		t.Fatal(err)
	}
	if err := firstRedisError(r); err != nil {
		t.Fatal(err)
	}
	return r
}

// seed stores ids with tags the way Cm_Cache_Backend_Redis does.
func seed(t *testing.T, c *respConn, id string, tags ...string) {
	t.Helper()
	cmds := [][]string{{"HSET", redisPrefixKey + id, "d", "data", "t", strings.Join(tags, ",")}, {"SADD", redisSetIDs, id}}
	for _, tag := range tags {
		cmds = append(cmds, []string{"SADD", redisPrefixTagID + tag, id}, []string{"SADD", redisSetTags, tag})
	}
	mustDo(t, c, cmds...)
}

func keyExists(t *testing.T, c *respConn, key string) bool {
	return mustDo(t, c, []string{"EXISTS", key})[0].(int64) == 1
}

func isMember(t *testing.T, c *respConn, set, m string) bool {
	return mustDo(t, c, []string{"SISMEMBER", set, m})[0].(int64) == 1
}

func TestRedis(t *testing.T) {
	port := freePort(t)
	startRedis(t, "--port", port, "--bind", "127.0.0.1")
	raw := rawClient(t, "tcp", "127.0.0.1:"+port)
	mustDo(t, raw, []string{"SELECT", "5"})

	s, err := New(Config{"backend": "Cm_Cache_Backend_Redis", "backend_options": map[string]any{
		"server": "127.0.0.1", "port": port, "database": "5",
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Many ids to exercise chunking.
	var many []string
	for i := 0; i < 1234; i++ {
		id := "69D_BIG_" + itoa(i)
		many = append(many, id)
	}
	cmds := [][]string{{"SADD", redisPrefixTagID + "69D_BIG"}, {"SADD", redisSetIDs}, {"MSET"}}
	for _, id := range many {
		cmds[0] = append(cmds[0], id)
		cmds[1] = append(cmds[1], id)
		cmds[2] = append(cmds[2], redisPrefixKey+id, "x")
	}
	cmds = append(cmds, []string{"SADD", redisSetTags, "69D_BIG"})
	mustDo(t, raw, cmds...)

	seed(t, raw, "69D_A", "69D_CONFIG")
	seed(t, raw, "69D_B", "69D_CONFIG", "69D_LAYOUT")
	seed(t, raw, "69D_C", "69D_OTHER")
	seed(t, raw, "69D_D", "69D_OTHER")

	if err := s.CleanTags([]string{"69D_CONFIG", "69D_BIG", "69D_NONE"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range append([]string{"69D_A", "69D_B"}, many...) {
		if keyExists(t, raw, redisPrefixKey+id) || isMember(t, raw, redisSetIDs, id) {
			t.Fatalf("%s not cleaned", id)
		}
	}
	if keyExists(t, raw, redisPrefixTagID+"69D_CONFIG") || isMember(t, raw, redisSetTags, "69D_CONFIG") || isMember(t, raw, redisSetTags, "69D_BIG") {
		t.Error("tag not cleaned")
	}
	if !keyExists(t, raw, redisPrefixKey+"69D_C") || !isMember(t, raw, redisSetTags, "69D_OTHER") {
		t.Error("unrelated data removed")
	}

	if err := s.CleanIDs([]string{"69D_C"}); err != nil {
		t.Fatal(err)
	}
	if keyExists(t, raw, redisPrefixKey+"69D_C") || isMember(t, raw, redisSetIDs, "69D_C") || !keyExists(t, raw, redisPrefixKey+"69D_D") {
		t.Error("CleanIDs wrong")
	}

	// Kill our storage's connection from another client; next call must reconnect.
	mustDo(t, raw, []string{"CLIENT", "SETNAME", "raw"})
	kill := mustDo(t, raw, []string{"CLIENT", "KILL", "TYPE", "normal", "SKIPME", "yes"})
	if kill[0].(int64) < 1 {
		t.Fatalf("expected to kill the storage connection, killed %v", kill[0])
	}
	if err := s.CleanIDs([]string{"69D_D"}); err != nil {
		t.Fatalf("after kill: %v", err)
	}
	if keyExists(t, raw, redisPrefixKey+"69D_D") {
		t.Error("CleanIDs after reconnect did not work")
	}

	// db 0 must be untouched by CleanAll on db 5.
	mustDo(t, raw, []string{"SELECT", "0"}, []string{"SET", "keep", "1"}, []string{"SELECT", "5"})
	if err := s.CleanAll(); err != nil {
		t.Fatal(err)
	}
	if n := mustDo(t, raw, []string{"DBSIZE"})[0].(int64); n != 0 {
		t.Errorf("DBSIZE after flush = %d", n)
	}
	if mustDo(t, raw, []string{"SELECT", "0"}, []string{"EXISTS", "keep"})[1].(int64) != 1 {
		t.Error("other db flushed")
	}
	if err := s.Close(); err != nil {
		t.Error(err)
	}
	// Reusable after Close (reconnects lazily).
	if err := s.CleanAll(); err != nil {
		t.Error(err)
	}
}

func TestRedisAuthAndUnixSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "r.sock")
	startRedis(t, "--port", "0", "--unixsocket", sock, "--requirepass", "secret")
	raw := rawClient(t, "unix", sock)
	mustDo(t, raw, []string{"AUTH", "secret"})
	seed(t, raw, "X1", "T1")

	for _, server := range []string{"unix://" + sock, sock} {
		s, err := New(Config{"backend_options": map[string]any{"server": server, "password": "secret", "port": 6379}})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(s.Describe(), "unix") {
			t.Errorf("describe: %s", s.Describe())
		}
		if err := s.CleanTags([]string{"T1"}); err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
	if keyExists(t, raw, redisPrefixKey+"X1") {
		t.Error("not cleaned over unix socket")
	}

	s, _ := New(Config{"backend_options": map[string]any{"server": sock, "password": "wrong"}})
	if err := s.CleanAll(); err == nil {
		t.Error("expected auth error")
	}

	// ACL user.
	mustDo(t, raw, []string{"ACL", "SETUSER", "cleaner", "on", ">pw2", "~*", "+@all"})
	s, _ = New(Config{"backend_options": map[string]any{"server": sock, "username": "cleaner", "password": "pw2"}})
	if err := s.CleanAll(); err != nil {
		t.Errorf("acl user: %v", err)
	}
	s.Close()
}

func TestRedisUnreachable(t *testing.T) {
	s, _ := New(Config{"backend_options": map[string]any{"server": "127.0.0.1", "port": freePort(t)}})
	start := time.Now()
	if err := s.CleanAll(); err == nil {
		t.Error("expected error")
	}
	if time.Since(start) > 3*time.Second {
		t.Error("dial took too long")
	}
	if err := s.Close(); err != nil {
		t.Error(err)
	}
}

func TestRemoteSynchronized(t *testing.T) {
	dir := t.TempDir()
	port := freePort(t)
	startRedis(t, "--port", port, "--bind", "127.0.0.1")
	raw := rawClient(t, "tcp", "127.0.0.1:"+port)
	seed(t, raw, "69D_A", "69D_T")
	fst := &fileStorage{cacheDir: dir, prefix: "pc", level: 1}
	writeFile(t, fst.idPath("69D_A"), "x")
	writeFile(t, fst.tagPath("69D_T"), "69D_A\n")

	s, err := New(Config{
		"backend":   `Magento\Framework\Cache\Backend\RemoteSynchronizedCache`,
		"id_prefix": "69d_",
		"backend_options": map[string]any{
			"remote_backend":         `\Magento\Framework\Cache\Backend\Redis`,
			"remote_backend_options": map[string]any{"server": "127.0.0.1", "port": port},
			"local_backend":          "Cm_Cache_Backend_File",
			"local_backend_options":  map[string]any{"cache_dir": dir, "file_name_prefix": "pc"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CleanTags([]string{"69D_T"}); err != nil {
		t.Fatal(err)
	}
	if tagIDs, _ := os.ReadFile(fst.tagPath("69D_T")); keyExists(t, raw, redisPrefixKey+"69D_A") || exists(fst.idPath("69D_A")) || len(tagIDs) != 0 {
		t.Error("not cleaned in both backends")
	}
	if !strings.Contains(s.Describe(), "remote") {
		t.Error(s.Describe())
	}
}
