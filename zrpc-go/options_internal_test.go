package zrpc

import "testing"

func TestNormalizeOptionsDefaults(t *testing.T) {
	workers, streams, err := normalizeOptions(ServerOptions{})
	if err != nil {
		t.Fatalf("normalizeOptions: %v", err)
	}
	if workers != defaultWorkers {
		t.Fatalf("workers = %d, want %d", workers, defaultWorkers)
	}
	if streams != defaultStreamConcurrency {
		t.Fatalf("streams = %d, want %d", streams, defaultStreamConcurrency)
	}
}

func TestNormalizeOptionsExplicit(t *testing.T) {
	workers, streams, err := normalizeOptions(ServerOptions{Workers: 3, StreamConcurrency: 7})
	if err != nil {
		t.Fatalf("normalizeOptions: %v", err)
	}
	if workers != 3 || streams != 7 {
		t.Fatalf("got workers=%d streams=%d, want 3/7", workers, streams)
	}
}

func TestNormalizeOptionsNegative(t *testing.T) {
	if _, _, err := normalizeOptions(ServerOptions{Workers: -1}); err == nil {
		t.Fatal("Workers=-1: want error, got nil")
	}
	if _, _, err := normalizeOptions(ServerOptions{StreamConcurrency: -1}); err == nil {
		t.Fatal("StreamConcurrency=-1: want error, got nil")
	}
}
