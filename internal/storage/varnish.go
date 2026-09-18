package storage

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	varnishTimeout  = 10 * time.Second
	varnishDownTime = 60 * time.Second
	// Magento\CacheInvalidate\Model\PurgeCache's default $maxHeaderSize; the
	// pattern is split over several requests to stay below Varnish's
	// http_req_hdr_len (8k by default).
	varnishMaxHeaderSize = 7680
)

// VarnishHost is one entry of env.php http_cache_hosts.
type VarnishHost struct {
	Host string
	Port string
}

func (h VarnishHost) addr() string {
	host, port := h.Host, h.Port
	if host == "" {
		host = "localhost"
	}
	if port == "" {
		port = "80"
	}
	return net.JoinHostPort(host, port)
}

// VarnishHostsFromConfig parses env.php http_cache_hosts: a list of maps with
// "host" (which may contain "host:port") and an optional "port". Missing
// values default to localhost and 80. A single map is accepted as one host.
func VarnishHostsFromConfig(httpCacheHosts any) []VarnishHost {
	entries := asList(httpCacheHosts)
	if m := asMap(httpCacheHosts); m["host"] != nil || m["port"] != nil {
		entries = []any{m}
	}
	var hosts []VarnishHost
	for _, e := range entries {
		m := asMap(e)
		if len(m) == 0 {
			continue
		}
		host := strings.TrimSpace(str(m["host"]))
		port := strings.TrimSpace(str(m["port"]))
		if h, p, err := net.SplitHostPort(host); err == nil {
			host = h
			if port == "" {
				port = p
			}
		}
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
		if host == "" {
			host = "localhost"
		}
		if port == "" {
			port = "80"
		}
		hosts = append(hosts, VarnishHost{Host: host, Port: port})
	}
	return hosts
}

type varnishStorage struct {
	hosts  []VarnishHost
	client *http.Client

	mu        sync.Mutex
	downUntil map[string]time.Time
	now       func() time.Time
}

// NewVarnish returns a Storage that sends PURGE requests to every host.
// CleanIDs is a no-op. Hosts that cannot be reached are skipped for 60s.
func NewVarnish(hosts []VarnishHost) Storage {
	return &varnishStorage{
		hosts: hosts,
		client: &http.Client{
			Timeout: varnishTimeout,
			Transport: &http.Transport{
				Proxy:             nil,
				DisableKeepAlives: true,
				DialContext:       (&net.Dialer{Timeout: varnishTimeout}).DialContext,
			},
		},
		downUntil: make(map[string]time.Time),
		now:       time.Now,
	}
}

// tagsPatterns builds the X-Magento-Tags-Pattern header values like
// Magento\CacheInvalidate\Observer\InvalidateVarnishObserver, split into
// chunks of at most varnishMaxHeaderSize bytes like PurgeCache::chunkTags.
func tagsPatterns(tags []string) []string {
	var chunks, cur []string
	size := 0
	for _, t := range dedupe(tags) {
		p := "((^|,)" + t + "(,|$))"
		if len(cur) > 0 && size+len(p)+len(cur) > varnishMaxHeaderSize {
			chunks = append(chunks, strings.Join(cur, "|"))
			cur, size = nil, 0
		}
		cur = append(cur, p)
		size += len(p)
	}
	if len(cur) > 0 {
		chunks = append(chunks, strings.Join(cur, "|"))
	}
	return chunks
}

func (v *varnishStorage) CleanTags(tags []string) error {
	var errs []error
	for _, p := range tagsPatterns(tags) {
		errs = append(errs, v.purgeAll(p))
	}
	return errors.Join(errs...)
}

func (v *varnishStorage) CleanIDs([]string) error { return nil }

func (v *varnishStorage) CleanAll() error { return v.purgeAll(".*") }

func (v *varnishStorage) Close() error {
	v.client.CloseIdleConnections()
	return nil
}

func (v *varnishStorage) Describe() string {
	if len(v.hosts) == 0 {
		return "varnish (no hosts)"
	}
	addrs := make([]string, len(v.hosts))
	for i, h := range v.hosts {
		addrs[i] = h.addr()
	}
	return "varnish " + strings.Join(addrs, ", ")
}

func (v *varnishStorage) purgeAll(pattern string) error {
	switch len(v.hosts) {
	case 0:
		return nil
	case 1:
		return v.purge(v.hosts[0], pattern)
	}
	errs := make([]error, len(v.hosts))
	var wg sync.WaitGroup
	for i, h := range v.hosts {
		wg.Go(func() { errs[i] = v.purge(h, pattern) })
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (v *varnishStorage) available(addr string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	until, ok := v.downUntil[addr]
	if !ok {
		return true
	}
	if v.now().Before(until) {
		return false
	}
	delete(v.downUntil, addr)
	return true
}

func (v *varnishStorage) markDown(addr string) {
	v.mu.Lock()
	v.downUntil[addr] = v.now().Add(varnishDownTime)
	v.mu.Unlock()
}

var titleStatus = regexp.MustCompile(`<title>(\d+)[^<]*</title>`)

// purge sends one PURGE request, like Magento\CacheInvalidate\Model\PurgeCache.
func (v *varnishStorage) purge(h VarnishHost, pattern string) error {
	addr := h.addr()
	if !v.available(addr) {
		return nil
	}
	req, err := http.NewRequest("PURGE", "http://"+addr+"/", nil)
	if err != nil {
		return fmt.Errorf("varnish %s: %w", addr, err)
	}
	req.Header.Set("X-Magento-Tags-Pattern", pattern)
	resp, err := v.client.Do(req)
	if err != nil {
		var opErr *net.OpError
		var dnsErr *net.DNSError
		if (errors.As(err, &opErr) && opErr.Op == "dial") || errors.As(err, &dnsErr) {
			// Varnish not running: not an error, but don't retry for a while.
			v.markDown(addr)
			return nil
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			v.markDown(addr)
		}
		return fmt.Errorf("varnish %s: %w", addr, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	// A synthetic response from the VCL may carry the real status in its title.
	if m := titleStatus.FindSubmatch(body); m != nil && string(m[1]) == "200" {
		return nil
	}
	return fmt.Errorf("varnish %s: PURGE returned status %d: %s", addr, resp.StatusCode, trimBody(body))
}

var htmlTag = regexp.MustCompile(`<[^>]*>`)

func trimBody(body []byte) string {
	s := htmlTag.ReplaceAllString(string(body), " ")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
