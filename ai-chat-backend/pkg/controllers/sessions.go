package controllers

import (
	"errors"
	"strconv"

	"ai-chat-backend/pkg/config"
	"ai-chat-backend/pkg/identity"
	"ai-chat-backend/pkg/users"
	ai_chat_service "ai-chat-backend/services/ai-chat-service"
	zrpc "echo-zrpc-go"
	"echo-zrpc-go/contract"
	"github.com/gin-gonic/gin"
)

// 会话/消息 HTTP API：身份来自 AuthMiddleware 的 device session → users 表，
// 再由 backend 用共享密钥签成短时 identity token 传给 ai-chat-service。
// 前端提交的任何 user_id/tenant_id 都不会被使用。

func (chat *ChatService) authUser(ctx *gin.Context) (*users.User, bool) {
	cnf := config.GetConfig()
	if !cnf.Auth.Enabled {
		ctx.JSON(401, gin.H{"status": "Fail", "message": "需要登录", "data": nil})
		return nil, false
	}
	deviceID := ctx.GetString("device_id")
	if deviceID == "" {
		ctx.JSON(401, gin.H{"status": "Fail", "message": "未登录或登录已过期", "data": nil})
		return nil, false
	}
	user, err := users.GetByDeviceID(deviceID)
	if err != nil {
		chat.log.Error(err)
		ctx.JSON(500, gin.H{"status": "Fail", "message": "用户服务异常", "data": nil})
		return nil, false
	}
	if user == nil {
		user, err = users.UpsertByDeviceID(deviceID, cnf.Auth.InitQuota)
		if err != nil {
			chat.log.Error(err)
			ctx.JSON(500, gin.H{"status": "Fail", "message": "用户服务异常", "data": nil})
			return nil, false
		}
	}
	if user == nil || user.ID <= 0 {
		ctx.JSON(401, gin.H{"status": "Fail", "message": "用户身份无效", "data": nil})
		return nil, false
	}
	return user, true
}

// mintIdentityToken 用于聊天主链路：身份不可用时返回空串，聊天本身继续（仅不记用量/历史）。
func (chat *ChatService) mintIdentityToken(ctx *gin.Context) string {
	cnf := config.GetConfig()
	if !cnf.Auth.Enabled {
		return ""
	}
	deviceID := ctx.GetString("device_id")
	if deviceID == "" {
		return ""
	}
	user, err := users.GetByDeviceID(deviceID)
	if err != nil {
		chat.log.Error(err)
		return ""
	}
	if user == nil {
		user, err = users.UpsertByDeviceID(deviceID, cnf.Auth.InitQuota)
		if err != nil || user == nil {
			chat.log.Error(err)
			return ""
		}
	}
	token, err := identity.TokenForUser(uint64(user.ID))
	if err != nil {
		chat.log.Error(err)
		return ""
	}
	return token
}

func (chat *ChatService) identityToken(ctx *gin.Context) (string, bool) {
	user, ok := chat.authUser(ctx)
	if !ok {
		return "", false
	}
	token, err := identity.TokenForUser(uint64(user.ID))
	if err != nil {
		chat.log.Error(err)
		ctx.JSON(500, gin.H{"status": "Fail", "message": "身份签发失败", "data": nil})
		return "", false
	}
	return token, true
}

func (chat *ChatService) failRPC(ctx *gin.Context, err error) {
	code, msg := 500, "服务暂不可用"
	var se *zrpc.StatusError
	if errors.As(err, &se) {
		switch se.Code {
		case zrpc.CodeUnauthenticated:
			code, msg = 401, "身份校验失败"
		case zrpc.CodeNotFound:
			code, msg = 404, "会话不存在"
		case zrpc.CodeInvalidArgument:
			code, msg = 400, "参数错误"
		case zrpc.CodeDeadlineExceeded:
			code, msg = 504, "请求超时"
		case zrpc.CodeUnavailable, zrpc.CodeCancelled:
			code, msg = 503, "服务暂不可用"
		}
	}
	chat.log.Error(err)
	ctx.JSON(code, gin.H{"status": "Fail", "message": msg, "data": nil})
}

func parseID(raw string) (uint64, error) {
	return strconv.ParseUint(raw, 10, 64)
}

func parseLimit(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 20
	}
	if n > 100 {
		return 100
	}
	return n
}

func (chat *ChatService) CreateSession(ctx *gin.Context) {
	var req struct {
		Title string `json:"title"`
	}
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(400, gin.H{"status": "Fail", "message": "参数错误", "data": nil})
		return
	}
	token, ok := chat.identityToken(ctx)
	if !ok {
		return
	}
	resp, err := ai_chat_service.CreateSession(ctx.Request.Context(), token, req.Title)
	if err != nil {
		chat.failRPC(ctx, err)
		return
	}
	ctx.JSON(200, gin.H{"status": "Success", "message": "", "data": resp})
}

func (chat *ChatService) ListSessions(ctx *gin.Context) {
	token, ok := chat.identityToken(ctx)
	if !ok {
		return
	}
	beforeID, _ := parseID(ctx.Query("before_id"))
	resp, err := ai_chat_service.ListSessions(ctx.Request.Context(), token, beforeID, parseLimit(ctx.Query("limit")))
	if err != nil {
		chat.failRPC(ctx, err)
		return
	}
	ctx.JSON(200, gin.H{"status": "Success", "message": "", "data": resp})
}

func (chat *ChatService) DeleteSession(ctx *gin.Context) {
	id, err := parseID(ctx.Param("id"))
	if err != nil || id == 0 {
		ctx.JSON(400, gin.H{"status": "Fail", "message": "参数错误", "data": nil})
		return
	}
	token, ok := chat.identityToken(ctx)
	if !ok {
		return
	}
	resp, err := ai_chat_service.DeleteSession(ctx.Request.Context(), token, id)
	if err != nil {
		chat.failRPC(ctx, err)
		return
	}
	ctx.JSON(200, gin.H{"status": "Success", "message": "", "data": resp})
}

func (chat *ChatService) ListMessages(ctx *gin.Context) {
	id, err := parseID(ctx.Param("id"))
	if err != nil || id == 0 {
		ctx.JSON(400, gin.H{"status": "Fail", "message": "参数错误", "data": nil})
		return
	}
	token, ok := chat.identityToken(ctx)
	if !ok {
		return
	}
	beforeID, _ := parseID(ctx.Query("before_id"))
	resp, err := ai_chat_service.ListMessages(ctx.Request.Context(), token, id, beforeID, parseLimit(ctx.Query("limit")))
	if err != nil {
		chat.failRPC(ctx, err)
		return
	}
	ctx.JSON(200, gin.H{"status": "Success", "message": "", "data": resp})
}

func (chat *ChatService) AppendConversation(ctx *gin.Context) {
	id, err := parseID(ctx.Param("id"))
	if err != nil || id == 0 {
		ctx.JSON(400, gin.H{"status": "Fail", "message": "参数错误", "data": nil})
		return
	}
	var req struct {
		RequestID        string `json:"request_id"`
		UserContent      string `json:"user_content"`
		UserTokens       int    `json:"user_tokens"`
		AssistantContent string `json:"assistant_content"`
		AssistantTokens  int    `json:"assistant_tokens"`
		Source           string `json:"source"`
	}
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(400, gin.H{"status": "Fail", "message": "参数错误", "data": nil})
		return
	}
	token, ok := chat.identityToken(ctx)
	if !ok {
		return
	}
	resp, err := ai_chat_service.AppendConversation(ctx.Request.Context(), &contract.AppendConversationRequest{
		IdentityToken: token, SessionID: id, RequestID: req.RequestID,
		UserContent: req.UserContent, UserTokens: req.UserTokens,
		AssistantContent: req.AssistantContent, AssistantTokens: req.AssistantTokens, Source: req.Source,
	})
	if err != nil {
		chat.failRPC(ctx, err)
		return
	}
	ctx.JSON(200, gin.H{"status": "Success", "message": "", "data": resp})
}
