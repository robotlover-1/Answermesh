package contract

// Wire-level method names shared by every service. These are the stable keys
// routed by the C server method table (zrpc v2), replacing gRPC full-method
// strings. One source of truth for AnswerMesh.
const (
	MethodChatCompletion       = "chat.completion"
	MethodChatCompletionStream = "chat.completion_stream"
	MethodFilterValidate       = "filter.validate"
	MethodFilterFindAll        = "filter.find_all"

	MethodSessionCreate      = "session.create"
	MethodSessionList        = "session.list"
	MethodSessionDelete      = "session.delete"
	MethodMessageList        = "message.list"
	MethodConversationAppend = "conversation.append"

	MethodKnowledgeCreate       = "knowledge.create"
	MethodKnowledgeSubmit       = "knowledge.submit"
	MethodKnowledgeReview       = "knowledge.review"
	MethodKnowledgePublish      = "knowledge.publish"
	MethodKnowledgeRevoke       = "knowledge.revoke"
	MethodKnowledgeGetPublished = "knowledge.get_published"
)
