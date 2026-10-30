package server

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	metrics_bus "ai-chat-service/chat-server/metrics-bus"
	"ai-chat-service/proto"
	zrpc "echo-zrpc-go"
	"echo-zrpc-go/contract"
)

/*
 * zrpc v2 side of the chat service (double-stack with gRPC during the
 * observation period). Unary and stream both reuse the transport-agnostic
 * business path (chatService.ChatCompletion / chatCompletionStream via the
 * ChatStream adapter), so caching / context / keywords / DB / LLM logic is
 * shared unchanged. Auth is enforced by the zrpc server envelope (Bearer).
 */

// ChatSvc 是 chat 业务的 zrpc 单传输出口（gRPC 已删；proto 结构体仅作内部 DTO）。
type ChatSvc interface {
	ChatCompletion(ctx context.Context, in *proto.ChatCompletionRequest) (*proto.ChatCompletionResponse, error)
	ServeChatStreamZRPC(ctx context.Context, raw json.RawMessage, w *zrpc.StreamWriter) error
}

func zrpcStatus(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

// ttftReporter 由 zRPC stream wrapper 注入 context；业务层在首个正式 token 到达时调用。
type ttftReporter func(time.Duration)

type ttftContextKey struct{}

// reportFirstToken 在首个 token 到达时上报 TTFT；没有 wrapper（如测试）时为空操作。
func reportFirstToken(ctx context.Context, d time.Duration) {
	if f, ok := ctx.Value(ttftContextKey{}).(ttftReporter); ok && f != nil {
		f(d)
	}
}

// RegisterChatZRPC wires chat methods on a zrpc.Server and records zRPC-level
// latency/count (current primary transport). Labels only contain method/status.
// wrapUnaryMetrics 为任意 unary 方法统一记录 requests_total 与耗时。
func wrapUnaryMetrics(method string, h zrpc.UnaryHandler,
	bus *metrics_bus.BusMetrics) zrpc.UnaryHandler {
	if bus == nil {
		return h
	}
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		start := time.Now()
		resp, err := h(ctx, raw)
		status := zrpcStatus(err)
		bus.ZrpcRequests.WithLabelValues(method, "unary", status).Inc()
		bus.ZrpcUnaryDuration.WithLabelValues(method, status).
			Observe(time.Since(start).Seconds())
		return resp, err
	}
}

func RegisterChatZRPC(zsrv *zrpc.Server, chat ChatSvc, streamOK bool,
	bus *metrics_bus.BusMetrics) error {
	unary := wrapUnaryMetrics(contract.MethodChatCompletion, unaryChatAdapter(chat), bus)
	if err := zsrv.RegisterUnary(contract.MethodChatCompletion, unary); err != nil {
		return err
	}
	if streamOK {
		stream := chat.ServeChatStreamZRPC
		if bus != nil {
			inner := stream
			stream = func(ctx context.Context, raw json.RawMessage, w *zrpc.StreamWriter) error {
				start := time.Now()
				method := contract.MethodChatCompletionStream
				bus.ZrpcStreamActive.WithLabelValues(method).Inc()
				defer bus.ZrpcStreamActive.WithLabelValues(method).Dec()
				ctx = context.WithValue(ctx, ttftContextKey{}, ttftReporter(func(d time.Duration) {
					bus.ZrpcFirstTokenDuration.WithLabelValues(method).Observe(d.Seconds())
				}))
				err := inner(ctx, raw, w)
				status := zrpcStatus(err)
				bus.ZrpcRequests.WithLabelValues(method, "stream", status).Inc()
				bus.ZrpcStreamDuration.WithLabelValues(method, status).
					Observe(time.Since(start).Seconds())
				return err
			}
		}
		if err := zsrv.RegisterStream(contract.MethodChatCompletionStream, stream); err != nil {
			return err
		}
	}
	return nil
}

// ServeChatStreamZRPC streams one chat.completion_stream request over zrpc.
func (s *chatService) ServeChatStreamZRPC(ctx context.Context, raw json.RawMessage, w *zrpc.StreamWriter) error {
	var creq contract.ChatCompletionRequest
	if err := json.Unmarshal(raw, &creq); err != nil {
		return zrpc.InvalidArgument(err)
	}
	preq, err := protoReqFromContract(&creq)
	if err != nil {
		return zrpc.InvalidArgument(err)
	}
	ctx = withChatMeta(ctx, &creq)
	// 使用 handler 收到的 ctx（而不是 w.Context()）：wrapper 在里面注入了 TTFT 上报器，
	// 两者共享同一取消链；w.Context() 是 runStream 的原始 ctx，拿不到 wrapper 的 value。
	zw := &zrpcChatStream{ctx: ctx, w: w}
	if err := s.chatCompletionStream(preq, zw); err != nil {
		return err // zrpc-go turns it into an ERROR frame unless already ended
	}
	return zw.End()
}

func unaryChatAdapter(chat ChatSvc) zrpc.UnaryHandler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var creq contract.ChatCompletionRequest
		if err := json.Unmarshal(raw, &creq); err != nil {
			return nil, zrpc.InvalidArgument(err)
		}
		preq, err := protoReqFromContract(&creq)
		if err != nil {
			return nil, zrpc.InvalidArgument(err)
		}
		presp, err := chat.ChatCompletion(withChatMeta(ctx, &creq), preq)
		if err != nil {
			// 保留业务层已给出的 StatusError（如身份校验失败→Unauthenticated）。
			var se *zrpc.StatusError
			if errors.As(err, &se) {
				return nil, se
			}
			return nil, zrpc.Internal(err)
		}
		return contractRespFromProto(presp)
	}
}

// protoReqFromContract converts the shared contract to the gRPC proto request
// (explicit mapping; JSON numbers stay numbers on the wire).
func protoReqFromContract(c *contract.ChatCompletionRequest) (*proto.ChatCompletionRequest, error) {
	out := &proto.ChatCompletionRequest{
		Message:       c.Message,
		Id:            c.ID,
		Pid:           c.PID,
		EnableContext: c.EnableContext,
	}
	if c.ChatParam != nil {
		out.ChatParam = &proto.ChatParam{
			Model:             c.ChatParam.Model,
			MaxTokens:         int32(c.ChatParam.MaxTokens),
			Temperature:       float32(c.ChatParam.Temperature),
			TopP:              float32(c.ChatParam.TopP),
			PresencePenalty:   float32(c.ChatParam.PresencePenalty),
			FrequencyPenalty:  float32(c.ChatParam.FrequencyPenalty),
			BotDesc:           c.ChatParam.BotDesc,
			MinResponseTokens: int32(c.ChatParam.MinResponseTokens),
			ContextTTL:        int32(c.ChatParam.ContextTTL),
			ContextLen:        int32(c.ChatParam.ContextLen),
		}
	}
	return out, nil
}

func contractRespFromProto(p *proto.ChatCompletionResponse) (*contract.ChatCompletionResponse, error) {
	out := &contract.ChatCompletionResponse{
		ID:      p.Id,
		Object:  p.Object,
		Created: p.Created,
		Model:   p.Model,
	}
	for _, ch := range p.Choices {
		if ch == nil {
			continue
		}
		cch := contract.ChatCompletionChoice{
			Index:        int(ch.Index),
			FinishReason: ch.FinishReason,
		}
		if ch.Message != nil {
			cch.Message = contract.ChatCompletionMessage{
				Role:    ch.Message.Role,
				Content: ch.Message.Content,
				Name:    ch.Message.Name,
			}
		}
		out.Choices = append(out.Choices, cch)
	}
	if p.Usage != nil {
		out.Usage = &contract.Usage{
			PromptTokens:     int(p.Usage.PromptTokens),
			CompletionTokens: int(p.Usage.CompletionTokens),
			TotalTokens:      int(p.Usage.TotalTokens),
		}
	}
	return out, nil
}
