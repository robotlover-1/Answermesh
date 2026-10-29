package ai_chat_service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	ai_chat_service_proto "ai-chat-backend/services/ai-chat-service/proto"

	zrpc "echo-zrpc-go"
	"echo-zrpc-go/contract"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return p
}

// Backend zrpc chat client driven by an in-process fake chat.stream server.
func TestOpenChatStreamZRPC(t *testing.T) {
	port := freePort(t)
	zsrv, err := zrpc.NewServer(zrpc.ServerOptions{Address: fmt.Sprintf("127.0.0.1:%d", port), AccessToken: "tok"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := zsrv.RegisterStream(contract.MethodChatCompletionStream,
		func(ctx context.Context, raw json.RawMessage, w *zrpc.StreamWriter) error {
			for i := 0; i < 3; i++ {
				if err := w.Send(contract.ChatCompletionStreamResponse{
					ID: "r1", Object: "chat.completion.chunk", Created: 1788547200,
					Model: "deepseek-v4-flash", Source: "llm",
					Choices: []contract.ChatCompletionStreamChoice{{
						Delta: contract.ChatCompletionStreamChoiceDelta{Content: fmt.Sprintf("块%d", i)},
					}},
				}); err != nil {
					return err
				}
			}
			return w.End()
		}); err != nil {
		t.Fatalf("RegisterStream: %v", err)
	}
	if err := zsrv.Serve(); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	in := &ai_chat_service_proto.ChatCompletionRequest{
		Id: "u1", Message: "hi", EnableContext: true,
		ChatParam: &ai_chat_service_proto.ChatParam{Model: "deepseek-v4-flash", MaxTokens: 100},
	}
	ctx := context.Background()
	var st ChatStream
	retry := func() error {
		var err error
		st, err = OpenChatStream(ctx, addr, "tok", "", 0, in)
		return err
	}
	// retry until the server accepts
	var lasterr error
	for i := 0; i < 100; i++ {
		if lasterr = retry(); lasterr == nil {
			break
		}
	}
	if lasterr != nil {
		t.Fatalf("OpenChatStream: %v", lasterr)
	}
	defer st.Close()

	got := ""
	for {
		rsp, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if rsp.Source != "llm" || rsp.Created != 1788547200 {
			t.Fatalf("bad chunk head: %+v", rsp)
		}
		if len(rsp.Choices) > 0 {
			got += rsp.Choices[0].Delta.Content
		}
	}
	if got != "块0块1块2" {
		t.Fatalf("concatenated content = %q", got)
	}

	// wrong token must surface as an auth error on Recv
	bad, err := OpenChatStream(ctx, addr, "nope", "", 0, in)
	if err != nil {
		t.Fatalf("bad OpenChatStream err: %v", err)
	}
	_, rerr := bad.Recv()
	if rerr == nil || errors.Is(rerr, io.EOF) {
		t.Fatalf("expected auth error, got %v", rerr)
	}
}
