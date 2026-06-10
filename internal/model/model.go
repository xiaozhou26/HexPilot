package model

// OpenAI Responses API 兼容的请求结构
// 完整支持 OpenAI (reasoning, verbosity, text), Anthropic (thinking, effort), Codex 等
type ResponsesRequest struct {
	// ===== 基础字段 =====
	Model       string          `json:"model,omitempty"`
	Input       interface{}     `json:"input"` // string 或 []InputItem
	Stream      bool            `json:"stream,omitempty"`
	MaxOutput   int             `json:"max_output_tokens,omitempty"`
	Temperature float64         `json:"temperature,omitempty"`
	TopP        float64         `json:"top_p,omitempty"`
	Mode        string          `json:"mode,omitempty"` // "react" 或 "plan_execute"，本项目特有

	// ===== 指令/系统提示 =====
	Instructions string `json:"instructions,omitempty"` // OpenAI Responses
	Prompt       string `json:"prompt,omitempty"`       // 别名兼容
	System       string `json:"system,omitempty"`       // Anthropic 兼容

	// ===== OpenAI reasoning 配置 =====
	// "reasoning": {"effort": "minimal|low|medium|high"}
	Reasoning *ReasoningConfig `json:"reasoning,omitempty"`

	// ===== Anthropic thinking 配置 =====
	// "thinking": {"type": "enabled", "budget_tokens": 4000}
	Thinking *ThinkingConfig `json:"thinking,omitempty"`

	// ===== Anthropic effort 配置 =====
	// "effort": "low|medium|high|extra|max"
	Effort string `json:"effort,omitempty"`

	// ===== OpenAI text verbosity =====
	// "text": {"verbosity": "low|medium|high"}
	Text *TextConfig `json:"text,omitempty"`

	// ===== OpenAI verbosity (旧版别名) =====
	Verbosity string `json:"verbosity,omitempty"`

	// ===== 工具 =====
	Tools      []Tool      `json:"tools,omitempty"`
	ToolChoice interface{} `json:"tool_choice,omitempty"`

	// ===== OpenAI allowed_tools (Codex/GPT-5 风格) =====
	AllowedTools []string `json:"allowed_tools,omitempty"`

	// ===== 杂项 =====
	Truncation     string `json:"truncation,omitempty"`
	PreviousRespID string `json:"previous_response_id,omitempty"`

	// ===== 原始请求体（用于透传） =====
	RawBody map[string]interface{} `json:"-"`
}

// OpenAI reasoning 配置
type ReasoningConfig struct {
	Effort string `json:"effort,omitempty"` // "minimal", "low", "medium", "high"
}

// Anthropic thinking 配置
type ThinkingConfig struct {
	Type         string `json:"type"`                   // "enabled" 或 "disabled"
	BudgetTokens int    `json:"budget_tokens,omitempty"` // 思考 token 预算
}

// OpenAI text verbosity 配置
type TextConfig struct {
	Verbosity string `json:"verbosity,omitempty"` // "low", "medium", "high"
}

// ===== 工具定义 =====

type Tool struct {
	Type        string     `json:"type"`
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Parameters  *ToolParam `json:"parameters,omitempty"`
}

type ToolParam struct {
	Type       string                 `json:"type"`
	Properties map[string]interface{} `json:"properties,omitempty"`
	Required   []string               `json:"required,omitempty"`
}

// ===== 输入项 =====

type InputItem struct {
	Type    string `json:"type"` // "message", "tool_call", "tool_result", "file_search_call", "computer_call"
	Role    string `json:"role,omitempty"`
	Content interface{} `json:"content"` // 可以是 string 或 []ContentBlock
}

// ===== 响应结构 =====

type ResponsesResponse struct {
	ID        string           `json:"id"`
	Object    string           `json:"object"`            // "response"
	Status    string           `json:"status"`            // "completed", "in_progress", "failed"
	Output    []OutputItem     `json:"output"`
	Model     string           `json:"model"`
	CreatedAt int64            `json:"created_at,omitempty"`
	Usage     *UsageInfo       `json:"usage,omitempty"`
	Parallel  *ParallelConfig  `json:"parallel,omitempty"` // Codex 支持
}

type OutputItem struct {
	Type      string                 `json:"type"` // "message", "function_call", "function_call_output"
	ID        string                 `json:"id,omitempty"`
	Name      string                 `json:"name,omitempty"`
	Role      string                 `json:"role,omitempty"`
	Content   []ContentBlock         `json:"content,omitempty"`
	CallID    string                 `json:"call_id,omitempty"`
	Arguments map[string]interface{} `json:"arguments,omitempty"`
	Status    string                 `json:"status,omitempty"`
	Output    string                 `json:"output,omitempty"` // function_call_output 的输出
}

type ContentBlock struct {
	Type     string `json:"type"` // "output_text", "thinking", "tool_use", "tool_result", "text"
	Text     string `json:"text,omitempty"`
	Content  string `json:"content,omitempty"` // 兼容格式
}

// ===== 使用信息 =====

type UsageInfo struct {
	InputTokens        int `json:"input_tokens,omitempty"`
	OutputTokens       int `json:"output_tokens,omitempty"`
	TotalTokens        int `json:"total_tokens,omitempty"`
	ReasoningTokens    int `json:"reasoning_tokens,omitempty"`
	CachedTokens       int `json:"cached_tokens,omitempty"`
}

type ParallelConfig struct {
	MaxParallelism int `json:"max_parallelism,omitempty"`
}

// ===== SSE 事件（用于流式响应） =====

type SSEEvent struct {
	Type string      `json:"type"`
	Data interface{} `json:"data,omitempty"`
}
