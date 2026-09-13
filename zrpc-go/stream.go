package zrpc

/*
#cgo CFLAGS: -I${SRCDIR} -I${SRCDIR}/../third_party/zrpc/include
#cgo LDFLAGS: ${SRCDIR}/../third_party/zrpc/build/libzrpc.a -lpthread -ldl
#include <stdlib.h>
#include "zrpc.h"
#include "bridge.h"
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// Wire event kinds (zrpc_protocol.h message types).
const (
	eventStreamData = 3 // ZRPC_MSG_STREAM_DATA
	eventStreamEnd  = 4 // ZRPC_MSG_STREAM_END
	eventError      = 5 // ZRPC_MSG_ERROR
)

// StreamHandler serves one server-streaming call. Raw is the business request;
// w pushes chunks and terminates with End()/Error().
type StreamHandler func(ctx context.Context, raw json.RawMessage, w *StreamWriter) error

// StreamWriter pushes stream chunks to one client connection. Methods are safe
// to call from any goroutine; a terminal (End/Error) makes later writes a no-op.
type StreamWriter struct {
	srv  *Server
	fd   int
	rid  uint64
	ctx  context.Context
	cancel context.CancelFunc

	mu   sync.Mutex
	done bool
}

func (w *StreamWriter) Context() context.Context { return w.ctx }

func (w *StreamWriter) isDone() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.done
}

func (w *StreamWriter) markDone() {
	w.mu.Lock()
	w.done = true
	w.mu.Unlock()
}

// Send marshals v and pushes one STREAM_DATA chunk.
func (w *StreamWriter) Send(v any) error {
	if w.isDone() {
		return io.EOF
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var p unsafe.Pointer
	var n C.uint32_t
	if len(data) > 0 {
		p = unsafe.Pointer(&data[0])
		n = C.uint32_t(len(data))
	}
	st := C.zrpc_server_send_stream_data(w.srv.c, C.int(w.fd), C.uint64_t(w.rid), p, n)
	runtime.KeepAlive(data)
	if st != C.ZRPC_STATUS_OK {
		w.markDone()
		return &StatusError{Code: Code(int(st)), Message: "send stream data"}
	}
	return nil
}

// End closes the stream with STREAM_END (single terminal).
func (w *StreamWriter) End() error {
	if w.isDone() {
		return io.EOF
	}
	st := C.zrpc_server_send_stream_end(w.srv.c, C.int(w.fd), C.uint64_t(w.rid))
	if st != C.ZRPC_STATUS_OK {
		w.markDone()
		return &StatusError{Code: Code(int(st)), Message: "send stream end"}
	}
	w.markDone()
	return nil
}

// Error terminates the stream with an ERROR frame carrying a stable code.
func (w *StreamWriter) Error(err error) error {
	if w.isDone() {
		return io.EOF
	}
	code := codeOf(err)
	cm := C.CString(err.Error())
	st := C.zrpc_server_send_error(w.srv.c, C.int(w.fd), C.uint64_t(w.rid), C.int(code), cm)
	C.free(unsafe.Pointer(cm))
	if st != C.ZRPC_STATUS_OK {
		w.markDone()
		return &StatusError{Code: Code(int(st)), Message: "send stream error"}
	}
	w.markDone()
	return nil
}

/* ---- per-server in-flight streams, keyed by client fd ---- */

func (s *Server) trackWriter(w *StreamWriter) {
	s.streamsMu.Lock()
	s.streamsByFD[w.fd] = w
	s.streamsMu.Unlock()
}

func (s *Server) untrackWriter(w *StreamWriter) {
	s.streamsMu.Lock()
	if s.streamsByFD[w.fd] == w {
		delete(s.streamsByFD, w.fd)
	}
	s.streamsMu.Unlock()
}

// cancelFD cancels the in-flight stream handler on a closed connection, which in
// turn cancels the handler context so an upstream LLM HTTP request is aborted.
func (s *Server) cancelFD(fd int) {
	s.streamsMu.Lock()
	w := s.streamsByFD[fd]
	delete(s.streamsByFD, fd)
	s.streamsMu.Unlock()
	if w != nil && w.cancel != nil {
		w.cancel()
	}
}

func (s *Server) cancelAllStreams() {
	s.streamsMu.Lock()
	ws := make([]*StreamWriter, 0, len(s.streamsByFD))
	for _, w := range s.streamsByFD {
		ws = append(ws, w)
	}
	s.streamsByFD = map[int]*StreamWriter{}
	s.streamsMu.Unlock()
	for _, w := range ws {
		if w.cancel != nil {
			w.cancel()
		}
	}
}

/* ---- server-side stream dispatch (spawned per request) ---- */

func (s *Server) runStream(j requestJob, fn StreamHandler) {
	ctx, cancel := context.WithCancel(context.Background())
	if j.deadline > 0 {
		var dc context.CancelFunc
		ctx, dc = context.WithDeadline(ctx, time.UnixMilli(int64(j.deadline)))
		defer dc()
	}
	w := &StreamWriter{srv: s, fd: j.fd, rid: j.rid, ctx: ctx, cancel: cancel}
	s.trackWriter(w)

	var herr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				herr = fmt.Errorf("stream handler panic: %v", r)
			}
		}()
		herr = fn(ctx, j.payload, w)
	}()
	s.untrackWriter(w)
	cancel()
	if herr != nil && !w.isDone() {
		_ = w.Error(herr) // no-op if the connection is already gone
	}
}

/* ---- active-server registry used by goZRPCOnConnClosed ---- */

var (
	serversMu sync.Mutex
	servers   = map[*Server]struct{}{}
)

func serverAdd(s *Server) {
	serversMu.Lock()
	servers[s] = struct{}{}
	serversMu.Unlock()
}

func serverRemove(s *Server) {
	serversMu.Lock()
	delete(servers, s)
	serversMu.Unlock()
}

//export goZRPCOnConnClosed
func goZRPCOnConnClosed(clientFD C.int) {
	serversMu.Lock()
	all := make([]*Server, 0, len(servers))
	for s := range servers {
		all = append(all, s)
	}
	serversMu.Unlock()
	for _, s := range all {
		s.cancelFD(int(clientFD))
	}
}

/* ---- client-side server stream ---- */

// Stream is a client-side server stream: Recv() drives the
// `for { rsp, err := stream.Recv(&out) }` loop the backend already uses.
type Stream struct {
	evCh   chan streamEvent
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

type streamEvent struct {
	kind int
	code Code
	data []byte
}

// Close cancels the stream (wakes the C reader, cancels upstream). Idempotent.
func (s *Stream) Close() error {
	s.once.Do(func() { s.cancel() })
	return nil
}

// Recv returns nil on STREAM_DATA (unmarshalled into out), io.EOF at STREAM_END,
// *StatusError for an ERROR frame, and ctx.Err() when the context is cancelled.
func (s *Stream) Recv(out any) error {
	select {
	case ev, ok := <-s.evCh:
		if !ok {
			return io.EOF
		}
		switch ev.kind {
		case eventStreamData:
			if out == nil {
				return nil
			}
			if err := json.Unmarshal(ev.data, out); err != nil {
				return fmt.Errorf("zrpc: unmarshal stream chunk: %w", err)
			}
			return nil
		case eventStreamEnd:
			return io.EOF
		case eventError:
			return &StatusError{Code: ev.code, Message: errorMsgFromBytes(ev.data)}
		default:
			return &StatusError{Code: CodeProtocolError, Message: "unknown stream event"}
		}
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func errorMsgFromBytes(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	var e struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &e) == nil {
		return e.Message
	}
	return ""
}

/* ---- client stream handle registry (uint64 keys, C holds no pointers) ---- */

var (
	cstreamMu  sync.Mutex
	cstreams   = map[uint64]*Stream{}
	cstreamSeq atomic.Uint64
)

func cstreamAdd(s *Stream) uint64 {
	id := cstreamSeq.Add(1)
	cstreamMu.Lock()
	cstreams[id] = s
	cstreamMu.Unlock()
	return id
}

func cstreamGet(id uint64) *Stream {
	cstreamMu.Lock()
	defer cstreamMu.Unlock()
	return cstreams[id]
}

func cstreamDel(id uint64) {
	cstreamMu.Lock()
	delete(cstreams, id)
	cstreamMu.Unlock()
}

//export goZRPCOnStreamEvent
func goZRPCOnStreamEvent(handle C.uint64_t, _ C.uint64_t, event C.int, status C.int,
	data unsafe.Pointer, dataLen C.uint32_t) {
	s := cstreamGet(uint64(handle))
	if s == nil {
		return
	}
	ev := streamEvent{kind: int(event), code: Code(int(status))}
	if dataLen > 0 {
		ev.data = C.GoBytes(data, C.int(dataLen))
	}
	// 队列满时**阻塞等待**，绝不丢弃 —— 这是整条流的水位控制。
	//
	// 该回调由 zrpc_client_call_stream 在「本流专属的 C 连接读循环」上同步调用
	// （zrpc_client.c 里未另起线程），所以这里阻塞只会停住这一条流的读取：
	// 消费者慢 → 读循环停 → 对端 TCP 窗口关闭 → 服务端自然减速。只影响本流。
	//
	// 曾经用 select/default 直接丢弃，代价是：消费者（HTTP 写回浏览器）一慢，
	// 队列就溢出，客户端看到的回答缺段；若丢掉 STREAM_END(kind=4)，Recv 永远
	// 等不到 io.EOF，前端就一直转圈。日志表现为 "stream event queue full"。
	//
	// 留 ctx.Done() 分支：消费者提前放弃（取消/超时）时放行，避免 C 线程被永久
	// 阻塞在回调里出不来。
	select {
	case s.evCh <- ev:
	case <-s.ctx.Done():
	}
}

// Stream opens a server stream on a DEDICATED C connection. Cancel ctx or call
// Close to abort (this wakes the blocked C reader and cancels the upstream).
func (c *Client) Stream(ctx context.Context, method string, req any) (*Stream, error) {
	body, err := marshalReq(req)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &Stream{
		evCh:   make(chan streamEvent, 128),
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go c.runStream(ctx, method, body, s)
	return s, nil
}

func (c *Client) runStream(ctx context.Context, method string, req []byte, s *Stream) {
	handle := cstreamAdd(s)
	defer func() {
		cstreamDel(handle)
		close(s.evCh)
	}()

	// dedicated client: stream cancellation never disturbs other calls
	ch := C.CString(c.host)
	var tk *C.char
	if c.token != "" {
		tk = C.CString(c.token)
	}
	scc := C.zrpc_client_new(ch, C.uint16_t(c.port), tk,
		C.int(c.connectTimeoutMs), C.int(c.ioTimeoutMs))
	C.free(unsafe.Pointer(ch))
	if tk != nil {
		C.free(unsafe.Pointer(tk))
	}
	if scc == nil {
		s.push(streamEvent{kind: eventError, code: CodeUnavailable})
		return
	}
	defer C.zrpc_client_free(scc)

	cm := C.CString(method)
	defer C.free(unsafe.Pointer(cm))

	var reqPtr unsafe.Pointer
	var reqLen C.uint32_t
	if len(req) > 0 {
		reqPtr = unsafe.Pointer(&req[0])
		reqLen = C.uint32_t(len(req))
	}

	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			C.zrpc_bridge_cancel(scc) // wake the blocked C read
		case <-stop:
		}
	}()

	C.zrpc_bridge_call_stream(scc, cm, reqPtr, reqLen,
		C.uint64_t(deadlineUnixMs(ctx)), C.uint64_t(handle))
	runtime.KeepAlive(req)
	close(stop)
}

// push 与 goZRPCOnStreamEvent 同样是"不丢事件"的阻塞投递；见那里的注释。
// 现有调用点（连接建立失败时投递 ERROR）队列必空，不会真的阻塞。
func (s *Stream) push(ev streamEvent) {
	select {
	case s.evCh <- ev:
	case <-s.ctx.Done():
	}
}
