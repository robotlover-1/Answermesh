package controllers

import (
	"strconv"

	ai_chat_service "ai-chat-backend/services/ai-chat-service"
	"github.com/gin-gonic/gin"
)

// 知识审核/发布：HTTP 层只做登录校验；角色由 backend 签发的 identity token 携带
// （identity.admin_user_ids），最终由 ai-chat-service 强制 admin。

func (chat *ChatService) CreateKnowledge(ctx *gin.Context) {
	var req struct {
		Question   string `json:"question"`
		SubjectKey string `json:"subject_key"`
	}
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(400, gin.H{"status": "Fail", "message": "参数错误", "data": nil})
		return
	}
	token, ok := chat.identityToken(ctx)
	if !ok {
		return
	}
	resp, err := ai_chat_service.CreateKnowledge(ctx.Request.Context(), token, req.Question, req.SubjectKey)
	if err != nil {
		chat.failRPC(ctx, err)
		return
	}
	ctx.JSON(200, gin.H{"status": "Success", "message": "", "data": resp})
}

func (chat *ChatService) SubmitKnowledge(ctx *gin.Context) {
	id, err := parseID(ctx.Param("id"))
	if err != nil || id == 0 {
		ctx.JSON(400, gin.H{"status": "Fail", "message": "参数错误", "data": nil})
		return
	}
	var req struct {
		Answer string `json:"answer"`
	}
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(400, gin.H{"status": "Fail", "message": "参数错误", "data": nil})
		return
	}
	token, ok := chat.identityToken(ctx)
	if !ok {
		return
	}
	resp, err := ai_chat_service.SubmitKnowledge(ctx.Request.Context(), token, id, req.Answer)
	if err != nil {
		chat.failRPC(ctx, err)
		return
	}
	ctx.JSON(200, gin.H{"status": "Success", "message": "", "data": resp})
}

func (chat *ChatService) ReviewKnowledge(ctx *gin.Context) {
	id, err := parseID(ctx.Param("id"))
	if err != nil || id == 0 {
		ctx.JSON(400, gin.H{"status": "Fail", "message": "参数错误", "data": nil})
		return
	}
	v, err := strconv.ParseUint(ctx.Param("version"), 10, 32)
	if err != nil || v == 0 {
		ctx.JSON(400, gin.H{"status": "Fail", "message": "参数错误", "data": nil})
		return
	}
	var req struct {
		Approve bool `json:"approve"`
	}
	if err := ctx.ShouldBindJSON(&req); err != nil {
		ctx.JSON(400, gin.H{"status": "Fail", "message": "参数错误", "data": nil})
		return
	}
	token, ok := chat.identityToken(ctx)
	if !ok {
		return
	}
	resp, err := ai_chat_service.ReviewKnowledge(ctx.Request.Context(), token, id, uint32(v), req.Approve)
	if err != nil {
		chat.failRPC(ctx, err)
		return
	}
	ctx.JSON(200, gin.H{"status": "Success", "message": "", "data": resp})
}

func (chat *ChatService) PublishKnowledge(ctx *gin.Context) {
	id, err := parseID(ctx.Param("id"))
	if err != nil || id == 0 {
		ctx.JSON(400, gin.H{"status": "Fail", "message": "参数错误", "data": nil})
		return
	}
	v, err := strconv.ParseUint(ctx.Param("version"), 10, 32)
	if err != nil || v == 0 {
		ctx.JSON(400, gin.H{"status": "Fail", "message": "参数错误", "data": nil})
		return
	}
	token, ok := chat.identityToken(ctx)
	if !ok {
		return
	}
	resp, err := ai_chat_service.PublishKnowledge(ctx.Request.Context(), token, id, uint32(v))
	if err != nil {
		chat.failRPC(ctx, err)
		return
	}
	ctx.JSON(200, gin.H{"status": "Success", "message": "", "data": resp})
}

func (chat *ChatService) GetPublishedKnowledge(ctx *gin.Context) {
	id, err := parseID(ctx.Param("id"))
	if err != nil || id == 0 {
		ctx.JSON(400, gin.H{"status": "Fail", "message": "参数错误", "data": nil})
		return
	}
	token, ok := chat.identityToken(ctx)
	if !ok {
		return
	}
	resp, err := ai_chat_service.GetPublishedKnowledge(ctx.Request.Context(), token, id)
	if err != nil {
		chat.failRPC(ctx, err)
		return
	}
	ctx.JSON(200, gin.H{"status": "Success", "message": "", "data": resp})
}

func (chat *ChatService) RevokeKnowledge(ctx *gin.Context) {
	id, err := parseID(ctx.Param("id"))
	if err != nil || id == 0 {
		ctx.JSON(400, gin.H{"status": "Fail", "message": "参数错误", "data": nil})
		return
	}
	var req struct {
		ExpectedVersion uint32 `json:"expected_version"`
		Reason          string `json:"reason"`
	}
	if err := ctx.ShouldBindJSON(&req); err != nil && err.Error() != "EOF" {
		ctx.JSON(400, gin.H{"status": "Fail", "message": "参数错误", "data": nil})
		return
	}
	token, ok := chat.identityToken(ctx)
	if !ok {
		return
	}
	resp, err := ai_chat_service.RevokeKnowledge(ctx.Request.Context(), token, id, req.ExpectedVersion, req.Reason)
	if err != nil {
		chat.failRPC(ctx, err)
		return
	}
	ctx.JSON(200, gin.H{"status": "Success", "message": "", "data": resp})
}
