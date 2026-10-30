package main

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

// oneByteReader 模拟 TCP 分片：每次只返回 1 字节，确保解析器不依赖单次 Read 的完整性。
type oneByteReader struct {
	data []byte
	off  int
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	p[0] = r.data[r.off]
	r.off++
	return 1, nil
}

func TestReadRESPFragmentedArray(t *testing.T) {
	payload := "*2\r\n$4\r\nINFO\r\n$7\r\nMEMSTAT\r\n"
	v, err := readRESP(bufio.NewReader(&oneByteReader{data: []byte(payload)}))
	if err != nil {
		t.Fatalf("readRESP: %v", err)
	}
	if v.Kind != respArray || len(v.Arr) != 2 {
		t.Fatalf("got kind=%d len=%d", v.Kind, len(v.Arr))
	}
	if v.Arr[0].Str != "INFO" || v.Arr[1].Str != "MEMSTAT" {
		t.Fatalf("unexpected args: %+v", v.Arr)
	}
}

func TestReadRESPBulkKeepsEmbeddedCRLF(t *testing.T) {
	// bulk 内容本身包含 \r\n，必须按长度读取而不是按行截断。
	payload := "$7\r\nA\r\nB\r\nC\r\n"
	br := bufio.NewReader(strings.NewReader(payload))
	v, err := readRESP(br)
	if err != nil {
		t.Fatalf("readRESP: %v", err)
	}
	if v.Kind != respBulk || v.Str != "A\r\nB\r\nC" {
		t.Fatalf("got kind=%d str=%q", v.Kind, v.Str)
	}
}

func TestReadRESPNullBulk(t *testing.T) {
	v, err := readRESP(bufio.NewReader(strings.NewReader("$-1\r\n")))
	if err != nil {
		t.Fatalf("readRESP: %v", err)
	}
	if v.Kind != respBulk || !v.Null {
		t.Fatalf("want null bulk, got %+v", v)
	}
}

func TestReadRESPRejectsBadTerminator(t *testing.T) {
	payload := "$3\r\nabcXX"
	_, err := readRESP(bufio.NewReader(strings.NewReader(payload)))
	if err == nil {
		t.Fatal("expected error for missing CRLF terminator")
	}
}

func TestReadRESPIntegerAndError(t *testing.T) {
	v, err := readRESP(bufio.NewReader(strings.NewReader(":42\r\n")))
	if err != nil || v.Int != 42 {
		t.Fatalf("integer: %v %+v", err, v)
	}
	v, err = readRESP(bufio.NewReader(strings.NewReader("-ERR boom\r\n")))
	if err != nil || v.Kind != respError || v.Str != "ERR boom" {
		t.Fatalf("error value: %v %+v", err, v)
	}
}

func TestParseKeyValue(t *testing.T) {
	m := parseKeyValue("a:1\nb=2\nbad-line\n\r\nc: 3 \r\n", nil)
	if m["a"] != "1" || m["b"] != "2" || m["c"] != "3" {
		t.Fatalf("unexpected map: %#v", m)
	}
	if _, ok := m["bad-line"]; ok {
		t.Fatal("malformed line should be ignored")
	}
}
