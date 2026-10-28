package zrpc_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	zrpc "echo-zrpc-go"
)

const capAddr = "127.0.0.1:19097"
const capTok = "stream-cap-token"

// TestStreamConcurrencyLimit verifies that when StreamConcurrency is exhausted
// the next stream is rejected immediately with CodeResourceExhausted instead of
// queueing or silently dropping the request.
func TestStreamConcurrencyLimit(t *testing.T) {
	started := make(chan struct{}, 1)
	srv, err := zrpc.NewServer(zrpc.ServerOptions{
		Address:           capAddr,
		AccessToken:       capTok,
		StreamConcurrency: 1,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.RegisterStream("s.hang", func(ctx context.Context, _ json.RawMessage, _ *zrpc.StreamWriter) error {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	}); err != nil {
		t.Fatalf("RegisterStream: %v", err)
	}
	if err := srv.Serve(); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	defer srv.Close()

	var cli *zrpc.Client
	for i := 0; i < 200; i++ {
		cli, err = zrpc.NewClient(zrpc.ClientOptions{Host: "127.0.0.1", Port: 19097, Token: capTok})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		if cli.Ping(context.Background()) == nil {
			break
		}
		cli.Close()
		cli = nil
		time.Sleep(10 * time.Millisecond)
	}
	if cli == nil {
		t.Fatal("server not ready")
	}
	defer cli.Close()

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	st1, err := cli.Stream(ctx1, "s.hang", nil)
	if err != nil {
		t.Fatalf("Stream(1): %v", err)
	}
	defer st1.Close()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first stream handler did not start")
	}

	st2, err := cli.Stream(context.Background(), "s.hang", nil)
	if err != nil {
		t.Fatalf("Stream(2): %v", err)
	}
	defer st2.Close()

	var out chunk
	err = st2.Recv(&out)
	var se *zrpc.StatusError
	if !errors.As(err, &se) || se.Code != zrpc.CodeResourceExhausted {
		t.Fatalf("Recv(2) = %v, want CodeResourceExhausted", err)
	}
}
