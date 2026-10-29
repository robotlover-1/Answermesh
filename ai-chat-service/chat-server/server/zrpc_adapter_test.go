package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"ai-chat-service/proto"
	zrpc "echo-zrpc-go"
	"echo-zrpc-go/contract"
)

func sampleRequest() *contract.ChatCompletionRequest {
	return &contract.ChatCompletionRequest{
		Message: "你好 世界", ID: "u-1", PID: "p-1", EnableContext: true,
		ChatParam: &contract.ChatParam{
			Model: "deepseek-v4-flash", MaxTokens: 100, Temperature: 0.5, TopP: 0.9,
			PresencePenalty: 0.8, FrequencyPenalty: 0.5, BotDesc: "b", MinResponseTokens: 50, ContextTTL: 1800, ContextLen: 4,
		},
	}
}

func sampleProtoResponse() *proto.ChatCompletionResponse {
	return &proto.ChatCompletionResponse{
		Id: "r-1", Object: "chat.completion", Created: 1788547200, Model: "deepseek-v4-flash",
		Choices: []*proto.ChatCompletionChoice{{Index: 0, Message: &proto.ChatCompletionMessage{Role: "assistant", Content: "你好"}, FinishReason: "stop"}},
		Usage:   &proto.Usage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15},
	}
}

// Typed equivalence: contract -> proto -> contract loses nothing, and created /
// token values survive as numbers on our JSON wire (no protojson int64 strings).
func TestContractProtoEquivalence(t *testing.T) {
	creq := sampleRequest()
	preq, err := protoReqFromContract(creq)
	if err != nil {
		t.Fatalf("protoReqFromContract: %v", err)
	}
	if preq.Message != creq.Message || preq.Id != creq.ID || preq.Pid != creq.PID ||
		preq.EnableContext != creq.EnableContext || preq.ChatParam == nil ||
		preq.ChatParam.Model != creq.ChatParam.Model || int(preq.ChatParam.MaxTokens) != creq.ChatParam.MaxTokens ||
		preq.ChatParam.ContextTTL != int32(creq.ChatParam.ContextTTL) {
		t.Fatalf("request field mismatch: contract %+v -> proto %+v", creq, preq)
	}

	presp := sampleProtoResponse()
	cresp, err := contractRespFromProto(presp)
	if err != nil {
		t.Fatalf("contractRespFromProto: %v", err)
	}
	if cresp.ID != presp.Id || cresp.Created != presp.Created || cresp.Object != presp.Object {
		t.Fatalf("resp head mismatch: %+v vs %+v", cresp, presp)
	}
	if len(cresp.Choices) != 1 || cresp.Choices[0].Message.Content != "你好" {
		t.Fatalf("resp choices mismatch: %+v", cresp.Choices)
	}
	if cresp.Usage == nil || !reflect.DeepEqual(*cresp.Usage, contract.Usage{12, 3, 15}) {
		t.Fatalf("resp usage mismatch: %+v", cresp.Usage)
	}
}

// ---- fake chat server (no infra) to test the zrpc unary adapter end-to-end ----

type fakeChat struct {
	proto.UnimplementedChatServer
	resp *proto.ChatCompletionResponse
}

func (f *fakeChat) ChatCompletion(_ context.Context, _ *proto.ChatCompletionRequest) (*proto.ChatCompletionResponse, error) {
	return f.resp, nil
}
func (f *fakeChat) ChatCompletionStream(_ *proto.ChatCompletionRequest, _ proto.Chat_ChatCompletionStreamServer) error {
	return status.Error(codes.Unimplemented, "stream not in task 6")
}
func (f *fakeChat) ServeChatStreamZRPC(_ context.Context, _ json.RawMessage, _ *zrpc.StreamWriter) error {
	return status.Error(codes.Unimplemented, "stream not in task 6")
}

func freePortT(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return p
}

func TestChatCompletionZRPCAdapter(t *testing.T) {
	port := freePortT(t)
	zsrv, err := zrpc.NewServer(zrpc.ServerOptions{Address: fmt.Sprintf("127.0.0.1:%d", port), AccessToken: "tok"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	fake := &fakeChat{resp: &proto.ChatCompletionResponse{
		Id: "r-1", Object: "chat.completion", Created: 1788547200, Model: "deepseek-v4-flash",
		Choices: []*proto.ChatCompletionChoice{{Index: 0, Message: &proto.ChatCompletionMessage{Role: "assistant", Content: "hi"}, FinishReason: "stop"}},
		Usage:   &proto.Usage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15},
	}}
	if err := RegisterChatZRPC(zsrv, fake, false, nil); err != nil {
		t.Fatalf("RegisterChatZRPC: %v", err)
	}
	if err := zsrv.Serve(); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	cli, err := zrpc.NewClient(zrpc.ClientOptions{Host: "127.0.0.1", Port: port, Token: "tok"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cli.Close()

	var resp contract.ChatCompletionResponse
	err = cli.Unary(context.Background(), contract.MethodChatCompletion,
		&contract.ChatCompletionRequest{Message: "hi", ID: "u1"}, &resp)
	if err != nil {
		t.Fatalf("chat.completion over zrpc: %v", err)
	}
	if resp.ID != "r-1" || len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "hi" {
		t.Fatalf("unexpected resp: %+v", resp)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 15 {
		t.Fatalf("usage missing: %+v", resp.Usage)
	}
}
