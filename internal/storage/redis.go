package storage

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Cm_Cache_Backend_Redis data model.
const (
	redisSetIDs      = "zc:ids"
	redisSetTags     = "zc:tags"
	redisPrefixKey   = "zc:k:"
	redisPrefixTagID = "zc:ti:"

	redisChunkSize      = 500
	redisDialTimeout    = 2 * time.Second
	redisCommandTimeout = 10 * time.Second
)

type redisOptions struct {
	network  string // "tcp" or "unix"
	addr     string // host:port or socket path
	host     string // for TLS server name
	useTLS   bool
	username string
	password string
	database int
}

// redisStorage is a Cm_Cache_Backend_Redis compatible cleaner. It connects
// lazily, keeps the connection open between calls and reconnects once when a
// command fails because the connection broke.
type redisStorage struct {
	opts redisOptions

	mu   sync.Mutex
	conn *respConn
}

func newRedis(cfg Config) (*redisStorage, error) {
	opts, err := parseRedisOptions(sub(cfg, "backend_options"))
	if err != nil {
		return nil, err
	}
	return &redisStorage{opts: opts}, nil
}

func parseRedisOptions(o map[string]any) (redisOptions, error) {
	opts := redisOptions{
		password: str(o["password"]),
		username: str(o["username"]),
		database: intOr(o["database"], 0),
	}
	if opts.username == "" {
		opts.username = str(o["user"])
	}
	server := strings.TrimSpace(str(o["server"]))
	lower := strings.ToLower(server)
	switch {
	case strings.HasPrefix(lower, "unix://"):
		opts.network, opts.addr = "unix", server[len("unix://"):]
		if opts.addr == "" {
			return opts, fmt.Errorf("redis: empty unix socket path in server %q", server)
		}
		return opts, nil
	case strings.HasPrefix(server, "/"):
		opts.network, opts.addr = "unix", server
		return opts, nil
	case strings.HasPrefix(lower, "tls://"):
		opts.useTLS = true
		server = server[len("tls://"):]
	case strings.HasPrefix(lower, "tcp://"):
		server = server[len("tcp://"):]
	}
	server = strings.TrimRight(server, "/")
	host, embeddedPort := server, ""
	if h, p, err := net.SplitHostPort(server); err == nil {
		host, embeddedPort = h, p
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if host == "" {
		host = "127.0.0.1"
	}
	port := str(o["port"])
	if port == "" || port == "0" {
		port = embeddedPort
	}
	if port == "" {
		port = "6379"
	}
	if _, err := strconv.Atoi(port); err != nil {
		return opts, fmt.Errorf("redis: invalid port %q", port)
	}
	opts.network, opts.host, opts.addr = "tcp", host, net.JoinHostPort(host, port)
	return opts, nil
}

func (r *redisStorage) Describe() string {
	scheme := r.opts.network
	if r.opts.useTLS {
		scheme = "tls"
	}
	return fmt.Sprintf("redis %s:%s db %d", scheme, r.opts.addr, r.opts.database)
}

func (r *redisStorage) connect() (*respConn, error) {
	d := net.Dialer{Timeout: redisDialTimeout, KeepAlive: 30 * time.Second}
	var nc net.Conn
	var err error
	if r.opts.useTLS {
		nc, err = (&tls.Dialer{NetDialer: &d, Config: &tls.Config{ServerName: r.opts.host}}).Dial(r.opts.network, r.opts.addr)
	} else {
		nc, err = d.Dial(r.opts.network, r.opts.addr)
	}
	if err != nil {
		return nil, fmt.Errorf("redis: connect %s: %w", r.opts.addr, err)
	}
	c := newRespConn(nc)
	var setup [][]string
	if r.opts.password != "" {
		if r.opts.username != "" {
			setup = append(setup, []string{"AUTH", r.opts.username, r.opts.password})
		} else {
			setup = append(setup, []string{"AUTH", r.opts.password})
		}
	}
	if r.opts.database != 0 {
		setup = append(setup, []string{"SELECT", strconv.Itoa(r.opts.database)})
	}
	if len(setup) > 0 {
		nc.SetDeadline(time.Now().Add(redisCommandTimeout))
		replies, err := c.do(setup)
		if err == nil {
			err = firstRedisError(replies)
		}
		if err != nil {
			c.Close()
			return nil, fmt.Errorf("redis: %s: %w", r.opts.addr, err)
		}
	}
	return c, nil
}

func firstRedisError(replies []any) error {
	for _, v := range replies {
		if e, ok := v.(redisError); ok {
			return e
		}
	}
	return nil
}

// pipeline runs the commands in one round trip. When the (reused) connection
// turns out to be broken it reconnects and retries once; all commands used
// here are idempotent so a partial first attempt is harmless.
func (r *redisStorage) pipeline(cmds [][]string) ([]any, error) {
	if len(cmds) == 0 {
		return nil, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		fresh := false
		if r.conn == nil {
			c, err := r.connect()
			if err != nil {
				return nil, err
			}
			r.conn, fresh = c, true
		}
		r.conn.c.SetDeadline(time.Now().Add(redisCommandTimeout))
		replies, err := r.conn.do(cmds)
		if err == nil {
			return replies, firstRedisError(replies)
		}
		r.conn.Close()
		r.conn = nil
		lastErr = fmt.Errorf("redis %s: %w", r.opts.addr, err)
		if fresh {
			break
		}
	}
	return nil, lastErr
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

// deleteIDCmds returns DEL zc:k:<id>... and SREM zc:ids <id>... commands in chunks.
func deleteIDCmds(ids []string) [][]string {
	var cmds [][]string
	for start := 0; start < len(ids); start += redisChunkSize {
		chunk := ids[start:min(start+redisChunkSize, len(ids))]
		del := make([]string, 0, len(chunk)+1)
		srem := make([]string, 0, len(chunk)+2)
		del = append(del, "DEL")
		srem = append(srem, "SREM", redisSetIDs)
		for _, id := range chunk {
			del = append(del, redisPrefixKey+id)
			srem = append(srem, id)
		}
		cmds = append(cmds, del, srem)
	}
	return cmds
}

func (r *redisStorage) CleanTags(tags []string) error {
	tags = dedupe(tags)
	if len(tags) == 0 {
		return nil
	}
	lookups := make([][]string, len(tags))
	for i, tag := range tags {
		lookups[i] = []string{"SMEMBERS", redisPrefixTagID + tag}
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
				ids = append(ids, s)
			}
		}
	}
	cmds := deleteIDCmds(dedupe(ids))
	delTags := []string{"DEL"}
	sremTags := []string{"SREM", redisSetTags}
	for _, tag := range tags {
		delTags = append(delTags, redisPrefixTagID+tag)
		sremTags = append(sremTags, tag)
	}
	cmds = append(cmds, delTags, sremTags)
	_, err = r.pipeline(cmds)
	return err
}

func (r *redisStorage) CleanIDs(ids []string) error {
	ids = dedupe(ids)
	if len(ids) == 0 {
		return nil
	}
	_, err := r.pipeline(deleteIDCmds(ids))
	return err
}

func (r *redisStorage) CleanAll() error {
	_, err := r.pipeline([][]string{{"FLUSHDB"}})
	return err
}

func (r *redisStorage) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn == nil {
		return nil
	}
	c := r.conn
	r.conn = nil
	c.c.SetDeadline(time.Now().Add(time.Second))
	c.do([][]string{{"QUIT"}}) // best effort
	err := c.Close()
	if errors.Is(err, net.ErrClosed) {
		err = nil
	}
	return err
}
