package ai_chat_service

import (
	"context"
	"errors"
	"io"

	ai_chat_service_proto "ai-chat-backend/services/ai-chat-service/proto"

	zrpc "echo-zrpc-go"
	"echo-zrpc-go/contract"
)

/*
 * ChatCompletionStream client for the backend —— gRPC 已删，仅走 zrpc v2。
 * Recv() 产出后端 proto chunk，controller 的计费/统计/流式逻辑零改动。
 * 父 ctx 取 Gin 请求 ctx：浏览器断开 → 取消 zrpc 流 → chat-service 取消上游 LLM。
 */

// ChatStream 是 controller 消费的最小面。
type ChatStream interface {
	Recv() (*ai_chat_service_proto.ChatCompletionStreamResponse, error)
	Close() error
}

// OpenChatStream 打开一条 ChatCompletionStream（zrpc）。
// identityToken/sessionID 可选：非空时服务端会记录用量并把最终消息写入会话历史。
func OpenChatStream(ctx context.Context, address, token, identityToken string, sessionID, knowledgeEntryID uint64,
	in *ai_chat_service_proto.ChatCompletionRequest) (ChatStream, error) {

	cli, err := zrpc.NewClient(clientOptionsFromAddress(address, token))
	if err != nil {
		return nil, err
	}
	req := contractReqFromProto(in)
	req.IdentityToken = identityToken
	req.SessionID = sessionID
	req.KnowledgeEntryID = knowledgeEntryID
	st, err := cli.Stream(ctx, contract.MethodChatCompletionStream, req)
	if err != nil {
		cli.Close()
		return nil, err
	}
	return &zrpcChatStream{cli: cli, st: st}, nil
}

func clientOptionsFromAddress(address, token string) zrpc.ClientOptions {
	return zrpc.ClientOptions{Host: hostOf(address), Port: portOf(address), Token: token}
}

type zrpcChatStream struct {
	cli *zrpc.Client
	st  *zrpc.Stream
}

func (z *zrpcChatStream) Recv() (*ai_chat_service_proto.ChatCompletionStreamResponse, error) {
	var c contract.ChatCompletionStreamResponse
	err := z.st.Recv(&c)
	if errors.Is(err, io.EOF) {
		return nil, io.EOF
	}
	if err != nil {
		return nil, err
	}
	return protoFromContractStream(&c), nil
}

func (z *zrpcChatStream) Close() error {
	_ = z.st.Close()
	return z.cli.Close()
}

func contractReqFromProto(in *ai_chat_service_proto.ChatCompletionRequest) *contract.ChatCompletionRequest {
	out := &contract.ChatCompletionRequest{
		Message: in.Message, ID: in.Id, PID: in.Pid, EnableContext: in.EnableContext,
	}
	if in.ChatParam != nil {
		out.ChatParam = &contract.ChatParam{
			Model: in.ChatParam.Model, MaxTokens: int(in.ChatParam.MaxTokens),
			Temperature: float64(in.ChatParam.Temperature), TopP: float64(in.ChatParam.TopP),
			PresencePenalty: float64(in.ChatParam.PresencePenalty), FrequencyPenalty: float64(in.ChatParam.FrequencyPenalty),
			BotDesc: in.ChatParam.BotDesc, MinResponseTokens: int(in.ChatParam.MinResponseTokens),
			ContextTTL: int(in.ChatParam.ContextTTL), ContextLen: int(in.ChatParam.ContextLen),
		}
	}
	return out
}

func protoFromContractStream(c *contract.ChatCompletionStreamResponse) *ai_chat_service_proto.ChatCompletionStreamResponse {
	out := &ai_chat_service_proto.ChatCompletionStreamResponse{
		Id: c.ID, Object: c.Object, Created: c.Created, Model: c.Model, Source: c.Source,
	}
	for _, ch := range c.Choices {
		pc := &ai_chat_service_proto.ChatCompletionStreamChoice{Index: int32(ch.Index), FinishReason: ch.FinishReason}
		if ch.Delta.Content != "" || ch.Delta.Role != "" {
			pc.Delta = &ai_chat_service_proto.ChatCompletionStreamChoiceDelta{Content: ch.Delta.Content, Role: ch.Delta.Role}
		}
		out.Choices = append(out.Choices, pc)
	}
	return out
}

func hostOf(addr string) string { h, _, _ := splitAddr(addr); return h }
func portOf(addr string) int {
	_, p, ok := splitAddr(addr)
	if !ok {
		return 0
	}
	return p
}

func splitAddr(addr string) (string, int, bool) {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			var p int
			for j := i + 1; j < len(addr); j++ {
				if addr[j] < '0' || addr[j] > '9' {
					return addr[:i], 0, false
				}
				p = p*10 + int(addr[j]-'0')
			}
			return addr[:i], p, true
		}
	}
	return addr, 0, false
}
