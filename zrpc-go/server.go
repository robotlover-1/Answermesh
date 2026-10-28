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
	"errors"
	"fmt"
	"log"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// UnaryHandler serves one method call. raw is the business request JSON.
// The returned value is JSON-marshalled as the business response; a returned
// error is mapped to a stable zrpc status code (StatusError when available).
type UnaryHandler func(ctx context.Context, raw json.RawMessage) (any, error)

// StreamHandler is reserved for Task 5 (chat completion stream); the Go-side
// Stream/StreamWriter types land with the stream bridge.

// ServerOptions configures a zrpc server backed by the C/NtyCo server.
type ServerOptions struct {
	Address     string // e.g. "0.0.0.0:50055"
	AccessToken string // empty disables auth
	Workers     int    // dispatch workers; 0 => 8
}

/* ---- global handle registry (C never stores Go pointers, only uint64) ---- */

type handlerEntry struct {
	fn  any // UnaryHandler or StreamHandler
	srv *Server
}

var (
	handlesMu  sync.RWMutex
	handles    = map[uint64]*handlerEntry{}
	nextHandle atomic.Uint64
)

func handleAdd(fn any, s *Server) uint64 {
	id := nextHandle.Add(1)
	handlesMu.Lock()
	handles[id] = &handlerEntry{fn: fn, srv: s}
	handlesMu.Unlock()
	return id
}

func handleGet(id uint64) *handlerEntry {
	handlesMu.RLock()
	defer handlesMu.RUnlock()
	return handles[id]
}

func handleClearFor(s *Server) {
	handlesMu.Lock()
	for id, e := range handles {
		if e.srv == s {
			delete(handles, id)
		}
	}
	handlesMu.Unlock()
}

func handleCount() int {
	handlesMu.RLock()
	defer handlesMu.RUnlock()
	return len(handles)
}

/* ---- Server ---- */

type Server struct {
	mu     sync.Mutex
	closed bool
	c      *C.zrpc_server_t
	jobs   chan requestJob
	done   chan struct{}
	wg     sync.WaitGroup

	streamsMu   sync.Mutex
	streamsByFD map[int]*StreamWriter
}

type requestJob struct {
	rid      uint64
	fd       int
	payload  []byte
	deadline uint64
	entry    *handlerEntry
}

// NewServer creates the C server. Call Serve() to start listening.
func NewServer(opts ServerOptions) (*Server, error) {
	addr := C.CString(opts.Address)
	var tok *C.char
	if opts.AccessToken != "" {
		tok = C.CString(opts.AccessToken)
	}
	var copts C.zrpc_server_options_t
	copts.address = addr
	copts.access_token = tok
	copts.io_timeout_ms = 0
	copts.max_connections = 0
	copts.backlog = 0
	csrv := C.zrpc_server_new(&copts)
	C.free(unsafe.Pointer(addr))
	if tok != nil {
		C.free(unsafe.Pointer(tok))
	}
	if csrv == nil {
		return nil, fmt.Errorf("zrpc: server_new failed")
	}

	s := &Server{
		c:           csrv,
		jobs:        make(chan requestJob, 1024),
		done:        make(chan struct{}),
		streamsByFD: map[int]*StreamWriter{},
	}
	return s, nil
}

// RegisterStream registers a server-streaming handler under method.
func (s *Server) RegisterStream(method string, h StreamHandler) error {
	return s.register(method, h, 1)
}

// RegisterUnary registers a unary handler under method.
func (s *Server) RegisterUnary(method string, h UnaryHandler) error {
	return s.register(method, h, 0)
}

func (s *Server) register(method string, fn any, isStream int) error {
	id := handleAdd(fn, s)
	cm := C.CString(method)
	st := C.zrpc_bridge_register(s.c, cm, C.int(isStream), C.uint64_t(id))
	C.free(unsafe.Pointer(cm))
	if st != C.ZRPC_STATUS_OK {
		handlesMu.Lock()
		delete(handles, id)
		handlesMu.Unlock()
		return &StatusError{Code: Code(int(st)), Message: "register " + method + ": " + C.GoString(C.zrpc_status_str(C.int(st)))}
	}
	return nil
}

// Serve starts the C/NtyCo listener and dispatch workers.
func (s *Server) Serve() error {
	if st := C.zrpc_server_serve(s.c); st != C.ZRPC_STATUS_OK {
		return &StatusError{Code: Code(int(st)), Message: "serve failed"}
	}
	C.zrpc_bridge_set_conn_close(s.c) // stream handlers cancelled on disconnect
	serverAdd(s)
	for i := 0; i < 8; i++ {
		s.wg.Add(1)
		go s.worker()
	}
	return nil
}

func (s *Server) worker() {
	defer s.wg.Done()
	for {
		select {
		case <-s.done:
			return
		case j := <-s.jobs:
			s.runJob(j)
		}
	}
}

func (s *Server) runJob(j requestJob) {
	entry := j.entry
	switch fn := entry.fn.(type) {
	case UnaryHandler:
		s.runUnary(j, fn)
	case StreamHandler:
		go s.runStream(j, fn) // long-running: hand off to its own goroutine
	default:
		s.sendErr(j.fd, j.rid, CodeInternal, "unregistered handler kind")
	}
}

func (s *Server) runUnary(j requestJob, unary UnaryHandler) {
	ctx := context.Background()
	if j.deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, time.UnixMilli(int64(j.deadline)))
		defer cancel()
	}

	var out any
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("handler panic: %v", r)
			}
		}()
		out, err = unary(ctx, j.payload)
	}()

	if err != nil {
		s.sendErr(j.fd, j.rid, codeOf(err), err.Error())
		return
	}
	data, merr := json.Marshal(out)
	if merr != nil {
		s.sendErr(j.fd, j.rid, CodeInternal, "marshal response: "+merr.Error())
		return
	}
	s.sendResp(j.fd, j.rid, data)
}

func (s *Server) sendResp(fd int, rid uint64, data []byte) {
	var p unsafe.Pointer
	var n C.uint32_t
	if len(data) > 0 {
		p = unsafe.Pointer(&data[0])
		n = C.uint32_t(len(data))
	}
	C.zrpc_server_send_response(s.c, C.int(fd), C.uint64_t(rid), p, n)
	runtime.KeepAlive(data)
}

func (s *Server) sendErr(fd int, rid uint64, code Code, msg string) {
	cm := C.CString(msg)
	C.zrpc_server_send_error(s.c, C.int(fd), C.uint64_t(rid), C.int(code), cm)
	C.free(unsafe.Pointer(cm))
}

func codeOf(err error) Code {
	var se *StatusError
	if errors.As(err, &se) && se != nil {
		return se.Code
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return CodeDeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return CodeCancelled
	}
	return CodeInternal
}

/* ---- C -> Go bridge: called from the NtyCo server coroutine. ---- */

//export goZRPCDispatchRequest
func goZRPCDispatchRequest(handle C.uint64_t, rid C.uint64_t, fd C.int,
	data unsafe.Pointer, dataLen C.uint32_t, deadline C.uint64_t) {
	payload := C.GoBytes(data, C.int(dataLen))
	entry := handleGet(uint64(handle))
	if entry == nil || entry.srv.closed {
		return
	}
	select {
	case entry.srv.jobs <- requestJob{
		rid:      uint64(rid),
		fd:       int(fd),
		payload:  payload,
		deadline: uint64(deadline),
		entry:    entry,
	}:
	default:
		// Never block the NtyCo coroutine; but don't silently drop either:
		// reply immediately so the client fails fast instead of waiting for timeout.
		// sendErr is safe from this goroutine (conn_lock_for_write serializes writers).
		log.Printf("zrpc: dispatch queue full, rejecting request rid=%d", uint64(rid))
		entry.srv.sendErr(int(fd), uint64(rid), CodeResourceExhausted, "server overloaded")
	}
}

// Close stops accepting (best effort; the NtyCo scheduler thread is torn down
// in Task 8), cancels in-flight streams and releases Go-side handles/workers so
// the registry empties.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	C.zrpc_server_shutdown(s.c)   // stop accepting + wake/shutdown conn readers
	s.cancelAllStreams()
	serverRemove(s)
	close(s.done)
	s.wg.Wait()
	handleClearFor(s)
	C.zrpc_server_join(s.c)       // wait for the NtyCo scheduler thread to exit
	return nil
}

// RegisteredCount returns how many handler handles this server still owns;
// tests use it to assert the registry empties after Close.
func (s *Server) RegisteredCount() int {
	handlesMu.RLock()
	defer handlesMu.RUnlock()
	n := 0
	for _, e := range handles {
		if e.srv == s {
			n++
		}
	}
	return n
}
