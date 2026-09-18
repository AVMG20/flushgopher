package storage

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
)

// redisError is an error reply ("-ERR ...") from the server. The connection
// is still usable after one.
type redisError string

func (e redisError) Error() string { return "redis: " + string(e) }

// respConn is a minimal RESP2 client connection supporting pipelining.
type respConn struct {
	c net.Conn
	r *bufio.Reader
	w *bufio.Writer
}

func newRespConn(c net.Conn) *respConn {
	return &respConn{c: c, r: bufio.NewReaderSize(c, 32*1024), w: bufio.NewWriterSize(c, 32*1024)}
}

func (c *respConn) writeCmd(args []string) error {
	c.w.WriteByte('*')
	c.w.WriteString(strconv.Itoa(len(args)))
	c.w.WriteString("\r\n")
	for _, a := range args {
		c.w.WriteByte('$')
		c.w.WriteString(strconv.Itoa(len(a)))
		c.w.WriteString("\r\n")
		c.w.WriteString(a)
		if _, err := c.w.WriteString("\r\n"); err != nil {
			return err
		}
	}
	return nil
}

// do sends all commands in one batch and reads one reply per command.
// Replies are string, int64, []any, nil or redisError. A non-nil error means
// the connection is broken (protocol or I/O failure).
func (c *respConn) do(cmds [][]string) ([]any, error) {
	for _, cmd := range cmds {
		if err := c.writeCmd(cmd); err != nil {
			return nil, err
		}
	}
	if err := c.w.Flush(); err != nil {
		return nil, err
	}
	replies := make([]any, len(cmds))
	for i := range cmds {
		v, err := c.readReply()
		if err != nil {
			return nil, err
		}
		replies[i] = v
	}
	return replies, nil
}

func (c *respConn) readLine() (string, error) {
	line, err := c.r.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return "", errors.New("redis: reply line too long")
		}
		return "", err
	}
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return "", errors.New("redis: malformed reply line")
	}
	return string(line[:len(line)-2]), nil
}

func (c *respConn) readReply() (any, error) {
	line, err := c.readLine()
	if err != nil {
		return nil, err
	}
	if line == "" {
		return nil, errors.New("redis: empty reply")
	}
	switch line[0] {
	case '+':
		return line[1:], nil
	case '-':
		return redisError(line[1:]), nil
	case ':':
		n, err := strconv.ParseInt(line[1:], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("redis: bad integer reply %q", line)
		}
		return n, nil
	case '$':
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return nil, fmt.Errorf("redis: bad bulk length %q", line)
		}
		if n < 0 {
			return nil, nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(c.r, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	case '*':
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return nil, fmt.Errorf("redis: bad array length %q", line)
		}
		if n < 0 {
			return nil, nil
		}
		arr := make([]any, n)
		for i := range arr {
			if arr[i], err = c.readReply(); err != nil {
				return nil, err
			}
		}
		return arr, nil
	}
	return nil, fmt.Errorf("redis: unexpected reply %q", line)
}

func (c *respConn) Close() error { return c.c.Close() }
