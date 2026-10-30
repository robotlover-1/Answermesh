package main

// 最小 RESP2 客户端：只用于只读的 INFO / MEMSTAT。
// 必须按 Bulk 长度读取完整 payload，不能按行读，否则会截断 DATA 中的 \r\n。

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

type respKind int

const (
	respSimple respKind = iota
	respError
	respInteger
	respBulk
	respArray
)

const (
	maxLineBytes = 1 << 20
	maxBulkBytes = 8 << 20
)

type respValue struct {
	Kind respKind
	Str  string
	Int  int64
	Arr  []respValue
	Null bool // nil bulk / nil array
}

func readRESPLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	if len(line) > maxLineBytes {
		return "", fmt.Errorf("resp: line too long (%d bytes)", len(line))
	}
	if !strings.HasSuffix(line, "\r\n") {
		return "", fmt.Errorf("resp: line not terminated by CRLF")
	}
	return line[:len(line)-2], nil
}

// readRESP 读取一个完整 RESP 值（支持 fragmented 输入，由 bufio.Reader 接管）。
func readRESP(r *bufio.Reader) (respValue, error) {
	prefix, err := r.ReadByte()
	if err != nil {
		return respValue{}, err
	}
	switch prefix {
	case '+', '-':
		line, err := readRESPLine(r)
		if err != nil {
			return respValue{}, err
		}
		if prefix == '+' {
			return respValue{Kind: respSimple, Str: line}, nil
		}
		return respValue{Kind: respError, Str: line}, nil
	case ':':
		line, err := readRESPLine(r)
		if err != nil {
			return respValue{}, err
		}
		n, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			return respValue{}, fmt.Errorf("resp: bad integer %q: %w", line, err)
		}
		return respValue{Kind: respInteger, Int: n}, nil
	case '$':
		line, err := readRESPLine(r)
		if err != nil {
			return respValue{}, err
		}
		n, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			return respValue{}, fmt.Errorf("resp: bad bulk length %q: %w", line, err)
		}
		if n == -1 {
			return respValue{Kind: respBulk, Null: true}, nil
		}
		if n < 0 || n > maxBulkBytes {
			return respValue{}, fmt.Errorf("resp: bulk length out of range: %d", n)
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return respValue{}, err
		}
		if buf[n] != '\r' || buf[n+1] != '\n' {
			return respValue{}, fmt.Errorf("resp: bulk payload not terminated by CRLF")
		}
		return respValue{Kind: respBulk, Str: string(buf[:n])}, nil
	case '*':
		line, err := readRESPLine(r)
		if err != nil {
			return respValue{}, err
		}
		n, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			return respValue{}, fmt.Errorf("resp: bad array length %q: %w", line, err)
		}
		if n == -1 {
			return respValue{Kind: respArray, Null: true}, nil
		}
		if n < 0 || n > 1024 {
			return respValue{}, fmt.Errorf("resp: array length out of range: %d", n)
		}
		arr := make([]respValue, 0, n)
		for i := int64(0); i < n; i++ {
			v, err := readRESP(r)
			if err != nil {
				return respValue{}, err
			}
			arr = append(arr, v)
		}
		return respValue{Kind: respArray, Arr: arr}, nil
	default:
		return respValue{}, fmt.Errorf("resp: unknown prefix %q", prefix)
	}
}

func encodeCommand(args ...string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	return b.Bytes()
}

// respClient 复用一个 TCP 连接；出错后关闭并在下一次调用时重连。
type respClient struct {
	addr    string
	timeout time.Duration

	mu   sync.Mutex
	conn net.Conn
	br   *bufio.Reader
}

func newRESPClient(addr string, timeout time.Duration) *respClient {
	return &respClient{addr: addr, timeout: timeout}
}

func (c *respClient) ensureConnectedLocked() error {
	if c.conn != nil {
		return nil
	}
	d := net.Dialer{Timeout: c.timeout}
	conn, err := d.Dial("tcp", c.addr)
	if err != nil {
		return err
	}
	c.conn = conn
	c.br = bufio.NewReaderSize(conn, 16<<10)
	return nil
}

func (c *respClient) closeLocked() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.conn = nil
	c.br = nil
}

func (c *respClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeLocked()
}

func writeAll(conn net.Conn, buf []byte) error {
	for len(buf) > 0 {
		n, err := conn.Write(buf)
		if err != nil {
			return err
		}
		buf = buf[n:]
	}
	return nil
}

// command 发送一条命令并读取完整响应；任何 IO/RESP 错误都会丢弃连接。
func (c *respClient) command(args ...string) (respValue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureConnectedLocked(); err != nil {
		return respValue{}, err
	}
	_ = c.conn.SetDeadline(time.Now().Add(c.timeout))
	if err := writeAll(c.conn, encodeCommand(args...)); err != nil {
		c.closeLocked()
		return respValue{}, err
	}
	v, err := readRESP(c.br)
	if err != nil {
		c.closeLocked()
		return respValue{}, err
	}
	if v.Kind == respError {
		return respValue{}, fmt.Errorf("pocketkv error: %s", v.Str)
	}
	return v, nil
}

func (c *respClient) textCommand(args ...string) (string, error) {
	v, err := c.command(args...)
	if err != nil {
		return "", err
	}
	switch v.Kind {
	case respBulk:
		if v.Null {
			return "", fmt.Errorf("pocketkv returned nil bulk")
		}
		return v.Str, nil
	case respSimple:
		return v.Str, nil
	default:
		return "", fmt.Errorf("unexpected RESP kind %d for text command", v.Kind)
	}
}
