package storage

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Normalized (lower-case, no leading backslash) backend class names.
const (
	backendCmRedis      = `cm_cache_backend_redis`
	backendMagentoRedis = `magento\framework\cache\backend\redis`
	backendRemoteSync   = `magento\framework\cache\backend\remotesynchronizedcache`
)

// New returns the Storage for a cache frontend config:
//
//   - Cm_Cache_Backend_Redis / Magento\Framework\Cache\Backend\Redis → redis
//   - Magento\Framework\Cache\Backend\RemoteSynchronizedCache → remote then local
//   - anything else: redis when backend_options.server is set, else the file backend.
//
// Backend names are compared case-insensitively, ignoring leading backslashes.
// No connection is made until the first operation.
func New(cfg Config) (Storage, error) {
	switch normalizeBackend(str(cfg["backend"])) {
	case backendCmRedis, backendMagentoRedis:
		return newRedis(cfg)
	case backendRemoteSync:
		opts := sub(cfg, "backend_options")
		local, err := New(Config{
			"backend":         opts["local_backend"],
			"id_prefix":       cfg["id_prefix"],
			"backend_options": opts["local_backend_options"],
		})
		if err != nil {
			return nil, fmt.Errorf("remote synchronized cache: local backend: %w", err)
		}
		remote, err := New(Config{
			"backend":         opts["remote_backend"],
			"id_prefix":       cfg["id_prefix"],
			"backend_options": opts["remote_backend_options"],
		})
		if err != nil {
			local.Close()
			return nil, fmt.Errorf("remote synchronized cache: remote backend: %w", err)
		}
		return &remoteSynchronized{local: local, remote: remote}, nil
	}
	if str(sub(cfg, "backend_options")["server"]) != "" {
		return newRedis(cfg)
	}
	return newFile(cfg)
}

func normalizeBackend(name string) string {
	return strings.ToLower(strings.TrimLeft(strings.TrimSpace(name), `\`))
}

// sub returns the nested map under key, or an empty map.
func sub(m map[string]any, key string) map[string]any {
	return asMap(m[key])
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// asList turns a PHP list (decoded as a slice, or as a map with numeric keys)
// into a slice. Map entries are ordered by key.
func asList(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			a, errA := strconv.Atoi(keys[i])
			b, errB := strconv.Atoi(keys[j])
			if errA == nil && errB == nil {
				return a < b
			}
			return keys[i] < keys[j]
		})
		out := make([]any, len(keys))
		for i, k := range keys {
			out[i] = t[k]
		}
		return out
	}
	return nil
}

// str converts a config scalar as decoded by phpconf or encoding/json
// (string, int64, float64, bool, nil) to a string; anything else yields "". Integral floats are formatted without a fraction.
func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return formatFloat(t)
	case bool:
		if t {
			return "1"
		}
		return ""
	}
	return ""
}

func formatFloat(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// intOr converts a loosely typed config scalar to an int, returning def when
// the value is missing, empty or not numeric.
func intOr(v any, def int) int {
	s := strings.TrimSpace(str(v))
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int(f)
	}
	return def
}

var errNoCacheDir = errors.New("no cache_dir present in file cache backend_options")
