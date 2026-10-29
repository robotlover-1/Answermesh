package contract

/*
 * Chat contract shared by ai-chat-service (server) and ai-chat-backend (client).
 * JSON field names replicate proto json_name (ai-chat-service/proto/chat.proto)
 * so contract JSON is semantically identical to the gRPC baseline. Stream types
 * (chat.completion_stream) are added alongside Task 7.
 */

// ChatCompletionRequest mirrors proto.ChatCompletionRequest.
type ChatCompletionRequest struct {
	Message       string     `json:"message"`
	ID            string     `json:"id"`
	PID           string     `json:"p_id"`
	EnableContext bool       `json:"enable_context"`
	ChatParam     *ChatParam `json:"chat_param,omitempty"`
	// 可选：backend 签发的短时身份令牌；携带时用于用量账本与可选会话落库。
	IdentityToken string `json:"identity_token,omitempty"`
	// 可选：非 0 时在响应完成后把 user+assistant 最终消息写入 chat_messages（幂等）。
	SessionID uint64 `json:"session_id,omitempty"`
}

// ChatParam mirrors proto.ChatParam (per-request override of chat defaults).
type ChatParam struct {
	Model             string  `json:"model,omitempty"`
	MaxTokens         int     `json:"max_tokens,omitempty"`
	Temperature       float64 `json:"temperature,omitempty"`
	TopP              float64 `json:"top_p,omitempty"`
	PresencePenalty   float64 `json:"presence_penalty,omitempty"`
	FrequencyPenalty  float64 `json:"frequency_penalty,omitempty"`
	BotDesc           string  `json:"bot_desc,omitempty"`
	MinResponseTokens int     `json:"min_response_tokens,omitempty"`
	ContextTTL        int     `json:"context_ttl,omitempty"`
	ContextLen        int     `json:"context_len,omitempty"`
}

// ChatCompletionResponse mirrors proto.ChatCompletionResponse.
type ChatCompletionResponse struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []ChatCompletionChoice `json:"choices"`
	Usage   *Usage                 `json:"usage,omitempty"`
}

type ChatCompletionChoice struct {
	Index        int                   `json:"index"`
	Message      ChatCompletionMessage `json:"message"`
	FinishReason string                `json:"finish_reason"`
}

type ChatCompletionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ---- streaming (chat.completion_stream) ----

// ChatCompletionStreamResponse mirrors proto.ChatCompletionStreamResponse,
// including the business-level source marker (cache/llm).
type ChatCompletionStreamResponse struct {
	ID      string                       `json:"id"`
	Object  string                       `json:"object"`
	Created int64                        `json:"created"`
	Model   string                       `json:"model"`
	Choices []ChatCompletionStreamChoice `json:"choices"`
	Source  string                       `json:"source,omitempty"`
}

type ChatCompletionStreamChoice struct {
	Index        int                             `json:"index"`
	Delta        ChatCompletionStreamChoiceDelta `json:"delta"`
	FinishReason string                          `json:"finish_reason"`
}

type ChatCompletionStreamChoiceDelta struct {
	Content string `json:"content"`
	Role    string `json:"role"`
}
