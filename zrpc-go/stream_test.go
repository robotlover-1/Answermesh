package zrpc_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	zrpc "echo-zrpc-go"
)

const streamAddr = "127.0.0.1:19095"
const streamTok = "task5-token"

type chunk struct {
	K int `json:"k"`
}

type hangState struct {
	running atomic.Bool
	exited  atomic.Bool
}

func multiHandler(_ context.Context, _ json.RawMessage, w *zrpc.StreamWriter) error {
	for i := 0; i < 10; i++ {
		if err := w.Send(chunk{K: i}); err != nil {
			return err
		}
	}
	return w.End()
}

func zeroHandler(_ context.Context, _ json.RawMessage, w *zrpc.StreamWriter) error {
	return w.End()
}

// manyChunks 远大于客户端事件队列容量（128），用来复现"生产快、消费慢"。
const manyChunks = 2000

func manyHandler(_ context.Context, _ json.RawMessage, w *zrpc.StreamWriter) error {
	for i := 0; i < manyChunks; i++ {
		if err := w.Send(chunk{K: i}); err != nil {
			return err
		}
	}
	return w.End()
}

func errHandler(_ context.Context, _ json.RawMessage, w *zrpc.StreamWriter) error {
	_ = w.Send(chunk{K: 0})
	_ = w.Send(chunk{K: 1})
	return zrpc.InvalidArgument(errors.New("boom after data"))
}

func startStreamServer(t *testing.T, hs *hangState) *zrpc.Server {
	t.Helper()
	srv, err := zrpc.NewServer(zrpc.ServerOptions{Address: streamAddr, AccessToken: streamTok})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	reg := func(m string, h zrpc.StreamHandler) {
		if err := srv.RegisterStream(m, h); err != nil {
			t.Fatalf("RegisterStream %s: %v", m, err)
		}
	}
	reg("s.multi", multiHandler)
	reg("s.zero", zeroHandler)
	reg("s.err", errHandler)
	reg("s.many", manyHandler)
	if hs != nil {
		reg("s.hang", func(ctx context.Context, _ json.RawMessage, w *zrpc.StreamWriter) error {
			hs.running.Store(true)
			defer hs.running.Store(false)
			<-ctx.Done() // stands in for an upstream LLM HTTP call
			hs.exited.Store(true)
			return ctx.Err()
		})
	}
	if err := srv.Serve(); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	return srv
}

func streamClient(t *testing.T) *zrpc.Client {
	t.Helper()
	cli, err := zrpc.NewClient(zrpc.ClientOptions{Host: "127.0.0.1", Port: 19095, Token: streamTok})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return cli
}

func waitStreamReady(t *testing.T) *zrpc.Client {
	t.Helper()
	for i := 0; i < 200; i++ {
		cli := streamClient(t)
		if cli.Ping(context.Background()) == nil {
			return cli
		}
		cli.Close()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server not ready")
	return nil
}

func TestStreamMultiChunksOrdered(t *testing.T) {
	srv := startStreamServer(t, nil)
	defer srv.Close()
	cli := waitStreamReady(t)
	defer cli.Close()

	st, err := cli.Stream(context.Background(), "s.multi", map[string]any{"q": 1})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer st.Close()

	got := make([]int, 0, 10)
	for {
		var c chunk
		err := st.Recv(&c)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		got = append(got, c.K)
	}
	if len(got) != 10 {
		t.Fatalf("got %d chunks, want 10", len(got))
	}
	for i := 0; i < 10; i++ {
		if got[i] != i {
			t.Fatalf("chunk %d = %d, want %d (order)", i, got[i], i)
		}
	}
}

// 回归测试：消费者慢（每收几个 chunk 就歇一下），生产端事件数远超 evCh 容量 128。
//
// 修好前，回调在队列满时用 select/default **丢弃**事件，本用例会出现缺号或提前 EOF
// （丢了流终止事件时连 EOF 都等不到）——日志里的 "stream event queue full, dropping
// kind=N" 就是它；这正是线上"回答缺段/输出不完整"的成因。
//
// 修好后靠背压（回调阻塞等待消费者）一个不丢。
func TestStreamSlowConsumerLosesNothing(t *testing.T) {
	srv := startStreamServer(t, nil)
	defer srv.Close()
	cli := waitStreamReady(t)
	defer cli.Close()

	st, err := cli.Stream(context.Background(), "s.many", nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer st.Close()

	got := 0
	for {
		var c chunk
		err := st.Recv(&c)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if c.K != got {
			t.Fatalf("乱序或缺号：期望 %d，收到 %d（说明事件被丢弃）", got, c.K)
		}
		got++
		// 故意慢下来，让生产端把队列灌满
		if got%10 == 0 {
			time.Sleep(3 * time.Millisecond)
		}
	}
	if got != manyChunks {
		t.Fatalf("丢事件：只收到 %d/%d 个 chunk", got, manyChunks)
	}
}

func TestStreamZeroChunks(t *testing.T) {
	srv := startStreamServer(t, nil)
	defer srv.Close()
	cli := waitStreamReady(t)
	defer cli.Close()

	st, _ := cli.Stream(context.Background(), "s.zero", nil)
	defer st.Close()
	var c chunk
	if err := st.Recv(&c); !errors.Is(err, io.EOF) {
		t.Fatalf("Recv = %v, want io.EOF", err)
	}
}

func TestStreamErrorAfterData(t *testing.T) {
	srv := startStreamServer(t, nil)
	defer srv.Close()
	cli := waitStreamReady(t)
	defer cli.Close()

	st, _ := cli.Stream(context.Background(), "s.err", nil)
	defer st.Close()

	n := 0
	var last error
	for {
		var c chunk
		err := st.Recv(&c)
		if err != nil {
			last = err
			break
		}
		n++
	}
	var se *zrpc.StatusError
	if !errors.As(last, &se) || se.Code != zrpc.CodeInvalidArgument {
		t.Fatalf("last err = %v, want CodeInvalidArgument", last)
	}
	if n != 2 {
		t.Fatalf("chunks before error = %d, want 2", n)
	}
}

// Client cancels mid-stream; the server handler's context must be cancelled so
// the "upstream LLM request" is released within a bounded time.
func TestStreamClientCancelReleasesUpstream(t *testing.T) {
	var hs hangState
	srv := startStreamServer(t, &hs)
	defer srv.Close()
	cli := waitStreamReady(t)
	defer cli.Close()

	ctx, cancel := context.WithCancel(context.Background())
	st, err := cli.Stream(ctx, "s.hang", nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for !hs.running.Load() {
		if time.Now().After(deadline) {
			t.Fatalf("server handler never started")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel() // ctx -> C cancel -> server sees disconnect -> cancels handler ctx
	var c chunk
	recvErr := st.Recv(&c)
	if !errors.Is(recvErr, context.Canceled) {
		t.Fatalf("Recv = %v, want context.Canceled", recvErr)
	}

	deadline = time.Now().Add(3 * time.Second)
	for !hs.exited.Load() {
		if time.Now().After(deadline) {
			t.Fatalf("server handler context was not cancelled in time (upstream LLM not released)")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
