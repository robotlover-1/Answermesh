package server

import (
	"context"
	"testing"
	"time"
)

func TestReportFirstToken(t *testing.T) {
	var got time.Duration
	ctx := context.WithValue(context.Background(), ttftContextKey{}, ttftReporter(func(d time.Duration) {
		got = d
	}))
	reportFirstToken(ctx, 125*time.Millisecond)
	if got != 125*time.Millisecond {
		t.Fatalf("ttft = %v, want 125ms", got)
	}

	// 无 wrapper（测试/直连）时必须安全地空操作。
	reportFirstToken(context.Background(), time.Second)
}

func TestZRPCStatus(t *testing.T) {
	if zrpcStatus(nil) != "ok" {
		t.Fatal("nil error should be ok")
	}
	if zrpcStatus(context.DeadlineExceeded) != "error" {
		t.Fatal("error should map to error")
	}
}
