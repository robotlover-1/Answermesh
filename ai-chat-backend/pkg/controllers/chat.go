package controllers

import (
	"ai-chat-backend/pkg/config"
	"ai-chat-backend/pkg/log"
	ai_chat_service "ai-chat-backend/services/ai-chat-service"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	ai_chat_service_proto "ai-chat-backend/services/ai-chat-service/proto"

	"ai-chat-backend/pkg/tokenizer"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"
	"k8s.io/klog/v2"
)

type ChatService struct {
	config *config.Config
	log    log.ILogger
}

type ChatCompletionParams struct {
	Model                 string        `json:"model"`
	MaxTokens             int           `json:"max_tokens,omitempty"`
	Temperature           float32       `json:"temperature,omitempty"`
	PresencePenalty       float32       `json:"presence_penalty,omitempty"`
	FrequencyPenalty      float32       `json:"frequency_penalty,omitempty"`
	ChatSessionTTL        time.Duration `json:"chat_session_ttl"`
	ChatMinResponseTokens int           `json:"chat_min_response_tokens"`
}

type ChatMessageRequest struct {
	Prompt  string                    `json:"prompt"`
	Options ChatMessageRequestOptions `json:"options"`
}

type ChatMessageRequestOptions struct {
	Name            string `json:"name"`
	ParentMessageId string `json:"parentMessageId"`
}

type ChatMessage struct {
	ID              string                                              `json:"id"`
	Text            string                                              `json:"text"`
	Role            string                                              `json:"role"`
	Name            string                                              `json:"name"`
	Delta           string                                              `json:"delta"`
	Detail          *ai_chat_service_proto.ChatCompletionStreamResponse `json:"detail"`
	TokenCount      int                                                 `json:"tokenCount"`
	ParentMessageId string                                              `json:"parentMessageId"`
	Source          string                                              `json:"source"`
	TokensUsed      int                                                 `json:"tokensUsed"`
	TokensSaved     int                                                 `json:"tokensSaved"`
}

func NewChatService(config *config.Config, log log.ILogger) (*ChatService, error) {
	return &ChatService{
		config: config,
		log:    log,
	}, nil
}

func (chat *ChatService) ChatProcess(ctx *gin.Context) {
	payload := ChatMessageRequest{}
	if err := ctx.BindJSON(&payload); err != nil {
		klog.Error(err)
		ctx.JSON(200, gin.H{
			"status":  "Fail",
			"message": fmt.Sprintf("%v", err),
			"data":    nil,
		})
		return
	}

	// 不做额度校验：登录（AuthMiddleware）仍生效以取得 device_id，但对话不限额度。

	messageID := uuid.New().String()

	result := ChatMessage{
		ID:              uuid.New().String(),
		Role:            openai.ChatMessageRoleAssistant,
		Text:            "",
		ParentMessageId: messageID,
	}

	in := &ai_chat_service_proto.ChatCompletionRequest{
		Id:            messageID,
		Message:       payload.Prompt,
		Pid:           payload.Options.ParentMessageId,
		EnableContext: false,
		ChatParam: &ai_chat_service_proto.ChatParam{
			Model:             chat.config.Chat.Model,
			MaxTokens:         int32(chat.config.Chat.MaxTokens),
			Temperature:       chat.config.Chat.Temperature,
			TopP:              chat.config.Chat.TopP,
			PresencePenalty:   chat.config.Chat.PresencePenalty,
			FrequencyPenalty:  chat.config.Chat.FrequencyPenalty,
			BotDesc:           chat.config.Chat.BotDesc,
			ContextTTL:        int32(chat.config.Chat.ContextTTL),
			ContextLen:        int32(chat.config.Chat.ContextLen),
			MinResponseTokens: int32(chat.config.Chat.MinResponseTokens),
		},
	}
	if in.Pid != "" {
		in.EnableContext = true
	}

	dep := chat.config.DependOn.AiChatService
	// 单 zrpc 下游（gRPC 已删）；父 ctx 取 Gin 请求 ctx（浏览器断开 → 取消 → 上游 LLM 释放）
	stream, err := ai_chat_service.OpenChatStream(ctx.Request.Context(), dep.Address, dep.AccessToken, in)
	if err != nil {
		chat.log.Error(err)
		ctx.JSON(200, gin.H{
			"status":  "Fail",
			"message": fmt.Sprintf("%v", err),
			"data":    nil,
		})
		return
	}
	defer stream.Close()

	firstChunk := true
	ctx.Header("Content-type", "application/octet-stream")
	for {
		rsp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			// 流结束：统计本轮 tokens，按来源分派（缓存命中不计费、记节省；LLM 计费、记消耗）
			promptMsg := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: payload.Prompt}
			respMsg := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: result.Text}
			pt, err1 := tokenizer.GetTokenCount(promptMsg, chat.config.Chat.Model)
			rt, err2 := tokenizer.GetTokenCount(respMsg, chat.config.Chat.Model)
			// 只统计用量用于前端展示，不再扣减额度（额度限制已移除）。
			if err1 == nil && err2 == nil {
				if result.Source == "cache" {
					result.TokensSaved = pt + rt
				} else {
					result.TokensUsed = pt + rt
				}
			} else {
				chat.log.ErrorF("token 统计失败: %v / %v", err1, err2)
			}
			// 末包：把 source 与 tokens 统计带给前端
			bts, err := json.Marshal(result)
			if err != nil {
				klog.Error(err)
				return
			}
			ctx.Writer.Write([]byte("\n"))
			if _, err := ctx.Writer.Write(bts); err != nil {
				klog.Error(err)
				return
			}
			// 末尾补一个换行做**记录终止符**：前端 createNdjsonReader 只解析已用 '\n'
			// 结束的行，没有它这一帧会永远留在读取器的 carry 里不被解析 —— 而 tokensUsed /
			// tokensSaved 只在这一帧下发，于是用量显示一直不更新（此前就存在的老问题）。
			ctx.Writer.Write([]byte("\n"))
			ctx.Writer.Flush()
			return
		}

		if err != nil {
			klog.Error(err)
			ctx.JSON(200, gin.H{
				"status":  "Fail",
				"message": fmt.Sprintf("OpenAI Event Error %v", err),
				"data":    nil,
			})
			return
		}

		if rsp.Id != "" {
			result.ID = rsp.Id
		}

		if rsp.Source != "" {
			result.Source = rsp.Source
		}

		if len(rsp.Choices) > 0 {
			content := rsp.Choices[0].Delta.Content
			result.Delta = content
			if len(content) > 0 {
				result.Text += content
			}
			result.Detail = rsp
		}

		// 这里原本每 15 个 chunk 用**累积全文**打两次同步 HTTP 去 tokenizer 统计 tokens。
		// 那是又一处 O(n²)：调用次数随流长线性增长、每次载荷又是全文，而且卡在流的热路径上。
		// 现在只在流结束时统计一次（见上面 io.EOF 分支），数值一样是这一轮的最终用量。

		// 每帧只发增量（Delta），**不带累积全文**：result.Text 每帧都发的话，单帧载荷随回答
		// 长度线性增长，整条流总流量是 O(n²)（3000 字回答约 1.4MB，只发增量约 240KB）。
		// 公网隧道上这会越传越慢；配合 zrpc 背压还会反过来拖慢上游 LLM。
		// 全文只在末帧（io.EOF 分支）给一次，供前端对齐全文与重试基准。
		frame := result
		frame.Text = ""
		bts, err := json.Marshal(frame)
		if err != nil {
			klog.Error(err)
			ctx.JSON(200, gin.H{
				"status":  "Fail",
				"message": fmt.Sprintf("OpenAI Event Marshal Error %v", err),
				"data":    nil,
			})
			return
		}

		if !firstChunk {
			ctx.Writer.Write([]byte("\n"))
		} else {
			firstChunk = false
		}

		if _, err := ctx.Writer.Write(bts); err != nil {
			klog.Error(err)
			return
		}

		ctx.Writer.Flush()
	}
}
