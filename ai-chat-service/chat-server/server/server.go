package server

import (
	chat_context "ai-chat-service/chat-server/chat-context"
	"ai-chat-service/chat-server/data"
	metrics_bus "ai-chat-service/chat-server/metrics-bus"
	semcache "ai-chat-service/chat-server/semcache"
	"ai-chat-service/pkg/config"
	"ai-chat-service/pkg/log"
	"ai-chat-service/proto"
	"ai-chat-service/services/tokenizer"
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/golang/protobuf/jsonpb"
	"github.com/google/uuid"
	"github.com/sashabaranov/go-openai"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const llmProvider = "openai_compatible"

// estimateTokensRough 仅用于缓存命中时的“估算节省 token”；
// 不用于计费、不写入权威用量。ASCII 约 4 字符/token，CJK 约 0.6 token/字符。
func estimateTokensRough(s string) int {
	if s == "" {
		return 0
	}
	ascii, cjk, other := 0, 0, 0
	for _, r := range s {
		switch {
		case r < utf8.RuneSelf:
			ascii++
		case r >= 0x2E80 && r <= 0x9FFF, r >= 0xF900 && r <= 0xFAFF:
			cjk++
		default:
			other++
		}
	}
	return int(math.Ceil(float64(ascii)/4 + float64(cjk)*0.6 + float64(other)*0.5))
}

type chatService struct {
	config     *config.Config
	log        log.ILogger
	data       data.IChatRecordsData
	busMetrics *metrics_bus.BusMetrics
}

func NewChatService(data data.IChatRecordsData, config *config.Config, log log.ILogger, busMetrics *metrics_bus.BusMetrics) ChatSvc {
	return &chatService{
		config:     config,
		log:        log,
		data:       data,
		busMetrics: busMetrics,
	}
}

// enqueueRecord 把记录交给有界入库队列；不阻塞请求路径超过 200ms。
// 失败只记录错误并计数，不把用户内容写入日志。
func (s *chatService) enqueueRecord(records *data.ChatRecord) {
	addCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := s.data.Add(addCtx, records); err != nil {
		// 入队失败已由 data 层按 reason 计入 records_enqueue_failures_total；
		// 这里只记录日志，避免同一失败被重复计数。
		s.log.Error(err)
	}
}

func (s *chatService) ChatCompletion(ctx context.Context, in *proto.ChatCompletionRequest) (*proto.ChatCompletionResponse, error) {
	redisContextCache := chat_context.NewRedisCache()
	defer redisContextCache.Close()

	app := s.newApp(in, redisContextCache)
	//敏感词过滤
	ok, msg, err := app.sensitive(in)
	if err != nil {
		s.log.Error(err)
		return nil, err
	}
	if !ok {
		res := app.buildChatCompletionResponse(msg)
		return res, nil
	}

	// 语义缓存：命中直接返回历史答案（不调大模型）
	if cachedAns, hit := semcache.CacheQuery(ctx, in.Message); hit {
		s.busMetrics.LLMToken("estimated_saved",
			estimateTokensRough(in.Message)+estimateTokensRough(cachedAns))
		return app.buildChatCompletionResponse(cachedAns), nil
	}

	//关键词提取
	keywords := app.keywords(in)

	client := app.getOpenaiClient()
	req, tokens, currTokens, currMessage, err := app.buildChatCompletionRequest(in, false)
	if err != nil {
		s.busMetrics.ErrQuestionsTotalCounter.Inc()
		s.log.Error(err)
		return nil, err
	}
	resp, err := client.CreateChatCompletion(ctx, req)
	if err != nil {
		s.busMetrics.LLMRequest(llmProvider, "error")
		s.log.Error(err)
		return nil, err
	}
	s.busMetrics.LLMRequest(llmProvider, "ok")
	// 只有模型返回 usage 的 unary 路径记录实际 token；缓存节省量单独用 estimated_saved。
	// （usage 为 0 时 LLMToken 会忽略，避免把“未返回 usage”伪装成 0 消耗。）
	s.busMetrics.LLMToken("input", resp.Usage.PromptTokens)
	s.busMetrics.LLMToken("output", resp.Usage.CompletionTokens)
	res := &proto.ChatCompletionResponse{}
	bytes, err := json.Marshal(resp)
	if err != nil {
		s.log.Error(err)
		return nil, err
	}
	err = jsonpb.UnmarshalString(string(bytes), res)
	if err != nil {
		s.log.Error(err)
		return nil, err
	}
	go func() {
		reqContext := &chat_context.ChatMessage{
			ID:      in.Id,
			PID:     in.Pid,
			Message: currMessage,
			Tokens:  currTokens,
		}
		err := app.saveContext(reqContext)
		if err != nil {
			s.log.Error(err)
			return
		}
		resContext := &chat_context.ChatMessage{
			ID:      resp.ID,
			PID:     reqContext.ID,
			Message: resp.Choices[0].Message,
			Tokens:  resp.Usage.CompletionTokens,
		}
		err = app.saveContext(resContext)
		if err != nil {
			s.log.Error(err)
			return
		}
	}()
	records := &data.ChatRecord{
		UserMsg:         in.Message,
		UserMsgTokens:   currTokens,
		UserMsgKeywords: keywords,
		AIMsg:           resp.Choices[0].Message.Content,
		AIMsgTokens:     resp.Usage.CompletionTokens,
		ReqTokens:       tokens,
		CreateAt:        time.Now().Unix(),
	}
	s.enqueueRecord(records)
	// 空回答不写缓存，避免污染；缓存写入不占用 DB 写入路径。
	if content := resp.Choices[0].Message.Content; content != "" {
		go func() {
			cacheCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := semcache.CacheWrite(cacheCtx, in.Message, content); err != nil {
				s.log.Error(err)
			}
		}()
	}
	return res, err
}

func (s *chatService) chatCompletionStream(in *proto.ChatCompletionRequest, stream ChatStream) error {
	redisContextCache := chat_context.NewRedisCache()
	defer redisContextCache.Close()

	app := s.newApp(in, redisContextCache)
	//敏感词过滤
	ok, msg, err := app.sensitive(in)
	if err != nil {
		s.busMetrics.ErrQuestionsTotalCounter.Inc()
		s.log.Error(err)
		return err
	}
	if !ok {
		s.busMetrics.SensitiveQuestionsTotalCounter.Inc()
		resId := uuid.New().String()
		startRes := app.buildChatCompletionStreamResponse(resId, "", "")
		endRes := app.buildChatCompletionStreamResponse(resId, "", "stop")
		err = stream.Send(startRes)
		if err != nil {
			s.log.Error(err)
			return err
		}
		resList := app.buildChatCompletionStreamResponseList(resId, msg)
		for _, res := range resList {
			err = stream.Send(res)
			if err != nil {
				s.log.Error(err)
				return err
			}
		}
		err = stream.Send(endRes)
		if err != nil {
			s.log.Error(err)
			return err
		}
		return nil
	}

	// 语义缓存：命中直接返回历史答案（不调大模型），标注来源 cache
	if cachedAns, hit := semcache.CacheQuery(stream.Context(), in.Message); hit {
		s.busMetrics.LLMToken("estimated_saved",
			estimateTokensRough(in.Message)+estimateTokensRough(cachedAns))
		resId := uuid.New().String()
		startRes := app.withSource(app.buildChatCompletionStreamResponse(resId, "", ""), "cache")
		endRes := app.withSource(app.buildChatCompletionStreamResponse(resId, "", "stop"), "cache")
		_ = stream.Send(startRes)
		resList := app.buildChatCompletionStreamResponseList(resId, cachedAns)
		for _, res := range resList {
			_ = stream.Send(app.withSource(res, "cache"))
		}
		_ = stream.Send(endRes)
		return nil
	}

	//关键词提取
	keywords := app.keywords(in)

	req, tokens, currTokens, currMessage, err := app.buildChatCompletionRequest(in, false)
	if err != nil {
		s.busMetrics.ErrQuestionsTotalCounter.Inc()
		s.log.Error(err)
		return err
	}
	// deepseek-v4-flash 偶发整轮只推理不出 content（content 为空）；重试一次提高成功概率。
	// 只展示正式 content，不把推理过程当答案。
	// TTFT 从本轮 stream 开始计时，仅在首次拿到正式 content 时记录一次。
	ttftStart := time.Now()
	var ttftOnce sync.Once
	onFirstToken := func() {
		ttftOnce.Do(func() { reportFirstToken(stream.Context(), time.Since(ttftStart)) })
	}
	var completionContent, resultID, lastFinish string
	for attempt := 0; attempt < 2 && completionContent == ""; attempt++ {
		if attempt > 0 {
			s.log.InfoF("retry LLM: previous pass produced no content")
		}
		content, rid, fin, serr := app.streamLLMContent(stream.Context(), stream, req, onFirstToken)
		if serr != nil {
			s.busMetrics.LLMRequest(llmProvider, "error")
			s.busMetrics.ErrQuestionsTotalCounter.Inc()
			s.log.Error(serr)
			return serr
		}
		s.busMetrics.LLMRequest(llmProvider, "ok")
		completionContent, resultID, lastFinish = content, rid, fin
	}
	// 收尾：空内容给兜底提示；length 给截断提示；正常发 stop 收尾包
	if completionContent == "" {
		notice := "⚠ 模型未生成正式回答（可能在深度推理中），请重试一次或换个问法"
		res := app.buildChatCompletionStreamResponse(resultID, notice, "stop")
		res.Source = "llm"
		if err := stream.Send(res); err != nil {
			s.log.Error(err)
			return err
		}
		lastFinish = "no_content"
	} else if lastFinish == "length" {
		notice := "\n\n⚠ 回答因长度限制被截断，请继续提问或换一种问法"
		res := app.buildChatCompletionStreamResponse(resultID, notice, "stop")
		res.Source = "llm"
		if err := stream.Send(res); err != nil {
			s.log.Error(err)
			return err
		}
	} else {
		res := app.buildChatCompletionStreamResponse(resultID, "", "stop")
		res.Source = "llm"
		if err := stream.Send(res); err != nil {
			s.log.Error(err)
			return err
		}
	}
	resultMessage := openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleAssistant,
		Content: completionContent,
	}
	model := s.config.Chat.Model
	if in.ChatParam != nil && in.ChatParam.Model != "" {
		model = in.ChatParam.Model
	}
	resultTokens, err := tokenizer.GetTokens(&resultMessage, model)
	if err != nil {
		s.busMetrics.ErrQuestionsTotalCounter.Inc()
		s.log.Error(err)
		return err
	}

	go func() {
		reqContext := &chat_context.ChatMessage{
			ID:      in.Id,
			PID:     in.Pid,
			Message: currMessage,
			Tokens:  currTokens,
		}
		err := app.saveContext(reqContext)
		if err != nil {
			s.log.Error(err)
			return
		}
		// resultID 可能为空（流异常/首包无ID），避免以空 ID 写入空 key（ai_chat_service_）
		if resultID == "" {
			return
		}
		resContext := &chat_context.ChatMessage{
			ID:      resultID,
			PID:     reqContext.ID,
			Message: resultMessage,
			Tokens:  resultTokens,
		}
		err = app.saveContext(resContext)
		if err != nil {
			s.log.Error(err)
			return
		}
	}()
	s.busMetrics.QuestionsTotalCounter.Inc()
	records := &data.ChatRecord{
		UserMsg:         in.Message,
		UserMsgTokens:   currTokens,
		UserMsgKeywords: keywords,
		AIMsg:           completionContent,
		AIMsgTokens:     resultTokens,
		ReqTokens:       tokens,
		CreateAt:        time.Now().Unix(),
	}
	s.enqueueRecord(records)
	// 空回答或被截断(length)的回答不写缓存，避免污染
	if completionContent != "" && lastFinish != "length" {
		go func() {
			cacheCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := semcache.CacheWrite(cacheCtx, in.Message, completionContent); err != nil {
				s.log.Error(err)
			}
		}()
	}
	return nil
}

// streamLLMContent 发起一次原始 SSE 流式请求，把正式 content 逐块发给客户端。
// 不展示 reasoning_content（思考过程）；只返回累积 content、resultID、最后 finish_reason。
// 收尾（stop/截断提示/兜底）由调用方处理，以便 content 为空时可整体重试。
func (a *app) streamLLMContent(ctx context.Context, stream ChatStream, req openai.ChatCompletionRequest, onFirstToken func()) (string, string, string, error) {
	req.Stream = true
	httpResp, err := a.streamRawRequest(ctx, req)
	if err != nil {
		return "", "", "", err
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		return "", "", "", fmt.Errorf("llm stream status=%d body=%s", httpResp.StatusCode, string(body))
	}
	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	content := ""
	resultID := ""
	lastFinish := ""
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			ID      string `json:"id"`
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		if resultID == "" {
			resultID = chunk.ID
		}
		delta := chunk.Choices[0].Delta
		text := delta.Content // 只把正式 content 当答案；reasoning_content 是思考过程，不作为答案显示
		fin := chunk.Choices[0].FinishReason
		if fin != "" {
			lastFinish = fin
		}
		if text == "" {
			continue // 纯推理 chunk：跳过
		}
		if content == "" && onFirstToken != nil {
			onFirstToken()
		}
		content += text
		res := a.withSource(a.buildChatCompletionStreamResponse(resultID, text, ""), "llm")
		if err := stream.Send(res); err != nil {
			return "", "", "", err
		}
	}
	if err := scanner.Err(); err != nil {
		return "", "", "", err
	}
	return content, resultID, lastFinish, nil
}
