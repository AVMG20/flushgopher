// Package cache cleans Magento cache types and ids across the configured
// backends, keeping backend connections open between calls.
package cache

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"

	"flushgopher/internal/logx"
	"flushgopher/internal/magento"
	"flushgopher/internal/storage"
)

var typeTags = map[string]string{
	"collections":                     "COLLECTION_DATA",
	"config_webservices":              "WEBSERVICE",
	"config_webservice":               "WEBSERVICE",
	"layout":                          "LAYOUT_GENERAL_CACHE_TAG",
	"full_page":                       "FPC",
	"config_integration_consolidated": "INTEGRATION_CONSOLIDATED",
	"config_integration_api":          "INTEGRATION_API_CONFIG",
	"config_integration":              "INTEGRATION",
	"hyva_svg":                        "HYVA_ICONS",
}

// Tag returns the cache tag of a cache type.
func Tag(cacheType string) string {
	if t, ok := typeTags[cacheType]; ok {
		return t
	}
	return strings.ToUpper(cacheType)
}

type backend struct {
	stamp   string
	def     storage.Storage
	defPfx  string
	page    storage.Storage
	pagePfx string
	shared  bool // page_cache uses the very same backend as default
	varnish storage.Storage
}

// Cleaner is safe for concurrent use; calls are serialized.
type Cleaner struct {
	App    *magento.App
	DryRun bool

	mu       sync.Mutex
	backends map[string]*backend
}

func New(app *magento.App) *Cleaner {
	return &Cleaner{App: app, backends: map[string]*backend{}}
}

func (c *Cleaner) closeBackend(b *backend) {
	for _, s := range []storage.Storage{b.def, b.page, b.varnish} {
		if s != nil {
			s.Close()
		}
	}
}

// Close closes all backend connections.
func (c *Cleaner) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, b := range c.backends {
		c.closeBackend(b)
		delete(c.backends, k)
	}
}

func (c *Cleaner) backend(base string) (*backend, error) {
	st := c.App.EnvStamp(base)
	if b, ok := c.backends[base]; ok && b.stamp == st {
		return b, nil
	}
	if b, ok := c.backends[base]; ok {
		logx.Debugf("Configuration changed, reconnecting cache backends")
		c.closeBackend(b)
		delete(c.backends, base)
	}
	defCfg, err := c.App.CacheConfig(base, "default")
	if err != nil {
		return nil, err
	}
	pageCfg, err := c.App.CacheConfig(base, "page_cache")
	if err != nil {
		return nil, err
	}
	b := &backend{stamp: st, defPfx: prefix(defCfg), pagePfx: prefix(pageCfg)}
	if b.def, err = storage.New(defCfg); err != nil {
		return nil, err
	}
	if b.page, err = storage.New(pageCfg); err != nil {
		b.def.Close()
		return nil, err
	}
	b.shared = b.def.Describe() == b.page.Describe()
	b.varnish = storage.NewVarnish(c.App.VarnishHosts(base))
	logx.Debugf("Cache backends for %s: default=%s page_cache=%s", base, b.def.Describe(), b.page.Describe())
	c.backends[base] = b
	return b, nil
}

func prefix(cfg storage.Config) string {
	if s, ok := cfg["id_prefix"].(string); ok {
		return s
	}
	return ""
}

// CleanTypes cleans the given cache types in all base dirs. No types means
// flush everything.
func (c *Cleaner) CleanTypes(types []string) error {
	if len(types) == 0 {
		logx.Event(logx.Notice, logx.CFlushAll, "FLUSH", "%s", logx.Paint(logx.CFlushAll, "all caches"))
	} else {
		logx.Event(logx.Notice, logx.CType, "CLEAN", "%s", logx.Chips(logx.CType, types))
	}
	if c.DryRun {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var errs []error
	for _, base := range c.App.AllBaseDirs() {
		if err := c.cleanTypes(base, types); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c *Cleaner) cleanTypes(base string, types []string) error {
	b, err := c.backend(base)
	if err != nil {
		return err
	}
	if len(types) == 0 {
		return errors.Join(b.def.CleanAll(), c.cleanPage(b))
	}
	var tags []string
	fpc := false
	for _, t := range types {
		if t == "full_page" {
			fpc = true
			continue
		}
		tags = append(tags, b.defPfx+Tag(t))
	}
	var errs []error
	if len(tags) > 0 {
		logx.Debugf("Cleaning tags %s", strings.Join(tags, " "))
		errs = append(errs, b.def.CleanTags(tags))
	}
	if fpc {
		errs = append(errs, c.cleanPage(b))
	}
	return errors.Join(errs...)
}

// cleanPage flushes the page cache and Varnish. If page_cache shares its
// backend with the default cache only the FPC tag is cleaned, so the rest of
// the cache survives.
func (c *Cleaner) cleanPage(b *backend) error {
	var err error
	if b.shared {
		err = b.page.CleanTags([]string{b.pagePfx + "FPC"})
	} else {
		err = b.page.CleanAll()
	}
	return errors.Join(err, b.varnish.CleanAll())
}

// CleanIDs removes individual cache records in all base dirs.
func (c *Cleaner) CleanIDs(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	ids = slices.Clone(ids)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	shown := ids
	more := ""
	if len(ids) > 4 && !logx.Enabled(logx.Info) {
		shown = ids[:3]
		more = logx.Dim(" +" + strconv.Itoa(len(ids)-3) + " more (-v)")
	}
	logx.Event(logx.Notice, logx.CID, "IDS", "%s%s", logx.Chips(logx.CID, shown), more)
	if c.DryRun {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var errs []error
	for _, base := range c.App.AllBaseDirs() {
		b, err := c.backend(base)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		keys := make([]string, len(ids))
		for i, id := range ids {
			keys[i] = b.defPfx + strings.ToUpper(id)
		}
		errs = append(errs, b.def.CleanIDs(keys))
	}
	return errors.Join(errs...)
}
