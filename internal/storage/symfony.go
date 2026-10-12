package storage

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/zeebo/xxh3"
)

// Magento 2.4.9 replaced Zend_Cache with Symfony Cache
// (Magento\Framework\Cache\Frontend\Adapter\Symfony). The data model differs
// from the Cm_Cache_Backend_* one:
//
//	redis       <ns>:<id>                     record (Symfony RedisAdapter)
//	            cache:tags:<ns><tag>          SET of ids (RedisTagAdapter)
//	            cache:id_tags:<ns><id>        SET of tags (reverse index)
//	            cache:all_ids                 SET of ids
//	filesystem  <cache_dir>/<ns>/X/Y/<hash>   record (Symfony FilesystemAdapter)
//	            var/cache/symfony/tags/<tag>  newline separated ids (FilesystemTagAdapter)
//
// <ns> is the id_prefix. Ids and tags are stored without it, except in the
// redis index keys. The tag directory is shared by all cache frontends.

// Keys that magento.App.CacheConfig adds to a Config for a Symfony Cache based Magento.
const (
	// SymfonyKey marks the config as belonging to a Magento with Symfony Cache.
	SymfonyKey = "flushgopher:symfony"
	// SymfonyTagDirKey is the FilesystemTagAdapter directory (var/cache/symfony/tags).
	SymfonyTagDirKey = "flushgopher:symfony_tag_dir"
)

const (
	symfonyTagPrefix    = "cache:tags:"
	symfonyIDTagsPrefix = "cache:id_tags:"
	symfonyAllIDs       = "cache:all_ids"
	// static::class in FilesystemCommonTrait::getFile(), part of the hashed file name.
	symfonyFilesystemAdapter = `Symfony\Component\Cache\Adapter\FilesystemAdapter`
)

// newSymfony mirrors SymfonyAdapterProvider::createAdapter(): only the short
// names "redis" and "valkey" select redis, anything else (including backend
// class names) ends up on the filesystem. When the redis connection fails
// Magento silently falls back to the filesystem as well (2.4.9 turns a unix
// socket into an invalid DSN), so for redis both locations are cleaned.
func newSymfony(cfg Config) (Storage, error) {
	file, err := newSymfonyFile(cfg)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(str(cfg["backend"])) {
	case "redis", "valkey":
		r, err := newRedis(cfg)
		if err != nil {
			return nil, err
		}
		return &symfonyRedisWithFallback{redis: &symfonyRedis{redisStorage: r, ns: str(cfg["id_prefix"])}, file: file}, nil
	}
	return file, nil
}

// symfonyRedis cleans the Symfony RedisAdapter + RedisTagAdapter data model.
// It reuses the connection handling of redisStorage.
type symfonyRedis struct {
	*redisStorage
	ns string
}

// recordCmds removes the records of the given (prefixed) ids together with
// their reverse index and all_ids membership.
func (r *symfonyRedis) recordCmds(ids []string) [][]string {
	var cmds [][]string
	for start := 0; start < len(ids); start += redisChunkSize {
		chunk := ids[start:min(start+redisChunkSize, len(ids))]
		del := []string{"DEL"}
		srem := []string{"SREM", symfonyAllIDs}
		for _, id := range chunk {
			bare := strings.TrimPrefix(id, r.ns)
			del = append(del, r.ns+":"+bare, symfonyIDTagsPrefix+r.ns+bare)
			srem = append(srem, bare)
		}
		cmds = append(cmds, del, srem)
	}
	return cmds
}

func (r *symfonyRedis) CleanTags(tags []string) error {
	tags = dedupe(tags)
	if len(tags) == 0 {
		return nil
	}
	lookups := make([][]string, len(tags))
	for i, tag := range tags {
		lookups[i] = []string{"SMEMBERS", symfonyTagPrefix + tag}
	}
	replies, err := r.pipeline(lookups)
	if err != nil {
		return err
	}
	var ids []string
	for _, rep := range replies {
		members, _ := rep.([]any)
		for _, m := range members {
			if s, ok := m.(string); ok {
				ids = append(ids, r.ns+s)
			}
		}
	}
	cmds := r.recordCmds(dedupe(ids))
	delTags := []string{"DEL"}
	for _, tag := range tags {
		delTags = append(delTags, symfonyTagPrefix+tag)
	}
	_, err = r.pipeline(append(cmds, delTags))
	return err
}

func (r *symfonyRedis) CleanIDs(ids []string) error {
	ids = dedupe(ids)
	if len(ids) == 0 {
		return nil
	}
	_, err := r.pipeline(r.recordCmds(ids))
	return err
}

func (r *symfonyRedis) Describe() string {
	return "symfony " + r.redisStorage.Describe()
}

// symfonyRedisWithFallback cleans redis and the filesystem location Magento
// falls back to when it cannot connect to redis.
type symfonyRedisWithFallback struct {
	redis *symfonyRedis
	file  *symfonyFile
}

func (s *symfonyRedisWithFallback) CleanTags(tags []string) error {
	return errors.Join(s.redis.CleanTags(tags), s.file.CleanTags(tags))
}

func (s *symfonyRedisWithFallback) CleanIDs(ids []string) error {
	return errors.Join(s.redis.CleanIDs(ids), s.file.CleanIDs(ids))
}

func (s *symfonyRedisWithFallback) CleanAll() error {
	return errors.Join(s.redis.CleanAll(), s.file.CleanAll())
}

func (s *symfonyRedisWithFallback) Close() error {
	return errors.Join(s.redis.Close(), s.file.Close())
}

// Describe leaves out the fallback directory: the cleaner compares
// descriptions to detect a page_cache that shares its backend with the default
// cache, and the directories of the two frontends always differ.
func (s *symfonyRedisWithFallback) Describe() string {
	return s.redis.Describe() + " (+ file fallback)"
}

// symfonyFile cleans the Symfony FilesystemAdapter + FilesystemTagAdapter data
// model. Tag files are only read: like Magento, entries of deleted ids stay
// behind. The tag directory is shared with the other cache frontend, so
// truncating a tag file would orphan its records.
type symfonyFile struct {
	dir    string // <cache_dir>/<ns>
	tagDir string
	ns     string
}

func newSymfonyFile(cfg Config) (*symfonyFile, error) {
	cacheDir := str(sub(cfg, "backend_options")["cache_dir"])
	if cacheDir == "" {
		return nil, errNoCacheDir
	}
	ns := str(cfg["id_prefix"])
	return &symfonyFile{dir: filepath.Join(cacheDir, ns), tagDir: str(cfg[SymfonyTagDirKey]), ns: ns}, nil
}

// idPath mirrors FilesystemCommonTrait::getFile().
func (f *symfonyFile) idPath(id string) string {
	sum := xxh3.Hash128([]byte(symfonyFilesystemAdapter + id)).Bytes()
	h := strings.ReplaceAll(base64.StdEncoding.EncodeToString(sum[:]), "/", "-")
	return filepath.Join(f.dir, strings.ToUpper(h[0:1]), strings.ToUpper(h[1:2]), h[2:22])
}

func (f *symfonyFile) CleanTags(tags []string) error {
	if f.tagDir == "" {
		return nil
	}
	var errs []error
	seen := make(map[string]struct{})
	for _, tag := range tags {
		tag = strings.TrimPrefix(tag, f.ns)
		if !safeName(tag) {
			continue
		}
		ids, err := readLines(filepath.Join(f.tagDir, tag))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, id := range ids {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			if err := remove(f.idPath(id)); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func readLines(path string) ([]string, error) {
	fh, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer fh.Close()
	var out []string
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if s := strings.TrimSpace(sc.Text()); s != "" {
			out = append(out, s)
		}
	}
	return out, sc.Err()
}

func (f *symfonyFile) CleanIDs(ids []string) error {
	var errs []error
	for _, id := range ids {
		if err := remove(f.idPath(strings.TrimPrefix(id, f.ns))); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// CleanAll removes the namespace directory of this frontend. The shared tag
// directory is left alone.
func (f *symfonyFile) CleanAll() error {
	if f.ns == "" {
		// Without a namespace the records live directly in cache_dir, next to
		// other data; don't wipe that.
		return nil
	}
	return os.RemoveAll(f.dir)
}

func (f *symfonyFile) Close() error { return nil }

func (f *symfonyFile) Describe() string {
	return fmt.Sprintf("symfony file %s", f.dir)
}
