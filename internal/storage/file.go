package storage

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// fileStorage implements the Cm_Cache_Backend_File layout:
//
//	<cache_dir>/<prefix>-tags/<prefix>---<tag>        newline separated ids
//	<cache_dir>/<prefix>--<hash>/<prefix>---<id>      hash = last N hex chars of md5(id)
//
// With hashed_directory_level 0 the id files live directly in cache_dir.
type fileStorage struct {
	cacheDir string
	prefix   string
	level    int
}

func newFile(cfg Config) (*fileStorage, error) {
	opts := sub(cfg, "backend_options")
	dir := str(opts["cache_dir"])
	if dir == "" {
		return nil, errNoCacheDir
	}
	prefix := str(opts["file_name_prefix"])
	if prefix == "" {
		prefix = "mage"
	}
	level := intOr(opts["hashed_directory_level"], 1)
	if level < 0 {
		level = 0
	}
	if level > 32 {
		level = 32
	}
	return &fileStorage{cacheDir: dir, prefix: prefix, level: level}, nil
}

// safeName reports whether a tag or id can be used as a file name component
// without escaping the cache directory.
func safeName(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\\x00")
}

func (f *fileStorage) idPath(id string) string {
	name := f.prefix + "---" + id
	if f.level == 0 {
		return filepath.Join(f.cacheDir, name)
	}
	sum := md5.Sum([]byte(id))
	h := hex.EncodeToString(sum[:])
	return filepath.Join(f.cacheDir, f.prefix+"--"+h[len(h)-f.level:], name)
}

func (f *fileStorage) tagPath(tag string) string {
	return filepath.Join(f.cacheDir, f.prefix+"-tags", f.prefix+"---"+tag)
}

func remove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (f *fileStorage) CleanTags(tags []string) error {
	var errs []error
	seen := make(map[string]struct{})
	for _, tag := range tags {
		if !safeName(tag) {
			continue
		}
		data, err := takeTagFile(f.tagPath(tag))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for line := range bytes.SplitSeq(data, []byte{'\n'}) {
			id := string(bytes.TrimSpace(line))
			if !safeName(id) {
				continue
			}
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

// takeTagFile returns the ids in a tag file and truncates it, holding an
// exclusive flock like Cm_Cache_Backend_File does. Truncating instead of
// unlinking keeps ids that a concurrent PHP request appends right after we
// release the lock; with unlink they could land in the orphaned inode and the
// entry would never be cleaned by tag again. A missing file yields no ids.
func takeTagFile(path string) ([]byte, error) {
	fh, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer fh.Close()
	if err := syscall.Flock(int(fh.Fd()), syscall.LOCK_EX); err != nil {
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	data, err := io.ReadAll(fh)
	if err != nil {
		return nil, err
	}
	if err := fh.Truncate(0); err != nil {
		return nil, err
	}
	return data, nil
}

func (f *fileStorage) CleanIDs(ids []string) error {
	var errs []error
	for _, id := range ids {
		if !safeName(id) {
			continue
		}
		if err := remove(f.idPath(id)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// CleanAll removes what Cm_Cache_Backend_File's clean('all') removes: the
// <prefix>--* entries in cache_dir and the tag files. Other files in cache_dir
// (e.g. another frontend's entries under a different prefix) are left alone,
// and the tags directory itself is kept because a running PHP process only
// creates it once.
func (f *fileStorage) CleanAll() error {
	entries, err := os.ReadDir(f.cacheDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	var errs []error
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), f.prefix+"--") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(f.cacheDir, e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	tagDir := filepath.Join(f.cacheDir, f.prefix+"-tags")
	tagFiles, err := os.ReadDir(tagDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	for _, e := range tagFiles {
		if strings.HasPrefix(e.Name(), f.prefix+"---") {
			if err := remove(filepath.Join(tagDir, e.Name())); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (f *fileStorage) Close() error { return nil }

func (f *fileStorage) Describe() string {
	return fmt.Sprintf("file %s (prefix %s)", f.cacheDir, f.prefix)
}
