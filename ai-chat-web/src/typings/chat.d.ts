declare namespace Chat {

	interface Chat {
		dateTime: string
		text: string
		inversion?: boolean
		error?: boolean
		loading?: boolean
		conversationOptions?: ConversationRequest | null
		requestOptions: { prompt: string; options?: ConversationRequest | null }
		source?: string
		tokensUsed?: number
		tokensSaved?: number
	}

	interface History {
		title: string
		isEdit: boolean
		uuid: number
	}

	interface ChatState {
		active: number | null
		usingContext: boolean;
		history: History[]
		chat: { uuid: number; data: Chat[] }[]
	}

	interface ConversationRequest {
		conversationId?: string
		parentMessageId?: string
	}

	interface ConversationResponse {
		conversationId: string
		detail: {
			choices: { finish_reason: string; index: number; logprobs: any; text: string }[]
			created: number
			id: string
			model: string
			object: string
			usage: { completion_tokens: number; prompt_tokens: number; total_tokens: number }
		}
		id: string
		parentMessageId: string
		role: string
		// text：本段全文，**只在末帧**下发（供前端对齐与重试基准）。
		// 中间帧 text 为空、内容走 delta —— 后端不再每帧发累积全文，否则单帧载荷随回答
		// 长度线性增长、整条流总流量 O(n²)。前端因此必须自己按 delta 累积。
		text: string
		delta?: string
		// 后端 ChatMessage 实际下发（见 ai-chat-backend/pkg/controllers/chat.go）：
		// source 区分 cache/llm，tokens 用于前端展示用量。此前靠 JSON.parse 的 any 绕过检查。
		source?: string
		tokensUsed?: number
		tokensSaved?: number
	}
}
