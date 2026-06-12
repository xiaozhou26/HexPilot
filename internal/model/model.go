package model

import "strings"

// ResponsesRequest is the external /v1/responses request shape accepted by HexPilot.
type ResponsesRequest struct {
	Model       string      `json:"model,omitempty"`
	Input       interface{} `json:"input,omitempty"`
	Stream      bool        `json:"stream,omitempty"`
	MaxOutput   int         `json:"max_output_tokens,omitempty"`
	Temperature float64     `json:"temperature,omitempty"`
	TopP        float64     `json:"top_p,omitempty"`
	Mode        string      `json:"mode,omitempty"`

	Instructions string `json:"instructions,omitempty"`
	Prompt       string `json:"prompt,omitempty"`
	System       string `json:"system,omitempty"`

	Reasoning *ReasoningConfig `json:"reasoning,omitempty"`
	Thinking  *ThinkingConfig  `json:"thinking,omitempty"`
	Effort    string           `json:"effort,omitempty"`

	Text      *TextConfig `json:"text,omitempty"`
	Verbosity string      `json:"verbosity,omitempty"`

	Tools        []Tool      `json:"tools,omitempty"`
	ToolChoice   interface{} `json:"tool_choice,omitempty"`
	AllowedTools []string    `json:"allowed_tools,omitempty"`

	Truncation        string            `json:"truncation,omitempty"`
	PreviousRespID    string            `json:"previous_response_id,omitempty"`
	Store             bool              `json:"store,omitempty"`
	ParallelToolCalls bool              `json:"parallel_tool_calls,omitempty"`
	User              string            `json:"user,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
	Include           []string          `json:"include,omitempty"`
	StreamOptions     *StreamOptions    `json:"stream_options,omitempty"`

	UpstreamNativeTools bool   `json:"-"`
	ResponseID          string `json:"-"`

	RawBody map[string]interface{} `json:"-"`
}

func (r *ResponsesRequest) IsThinkingEnabled() bool {
	if r.Reasoning != nil {
		switch strings.ToLower(r.Reasoning.Effort) {
		case "high", "extra", "max", "maximum":
			return true
		}
	}
	if r.Thinking != nil && strings.ToLower(r.Thinking.Type) == "enabled" {
		return true
	}
	switch strings.ToLower(r.Effort) {
	case "high", "extra", "max", "maximum":
		return true
	}
	return false
}

type ReasoningConfig struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type ThinkingConfig struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type TextConfig struct {
	Verbosity string      `json:"verbosity,omitempty"`
	Format    *TextFormat `json:"format,omitempty"`
}

type TextFormat struct {
	Type   string      `json:"type"`
	Name   string      `json:"name,omitempty"`
	Schema interface{} `json:"schema,omitempty"`
	Strict *bool       `json:"strict,omitempty"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

type Tool struct {
	Type        string                 `json:"type"`
	Name        string                 `json:"name,omitempty"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
	Strict      *bool                  `json:"strict,omitempty"`
}

type InputItem struct {
	ID        string      `json:"id,omitempty"`
	Type      string      `json:"type,omitempty"`
	Role      string      `json:"role,omitempty"`
	Content   interface{} `json:"content,omitempty"`
	CallID    string      `json:"call_id,omitempty"`
	Name      string      `json:"name,omitempty"`
	Arguments interface{} `json:"arguments,omitempty"`
	Output    interface{} `json:"output,omitempty"`
	Status    string      `json:"status,omitempty"`
}

type ResponsesResponse struct {
	ID        string       `json:"id"`
	Object    string       `json:"object"`
	CreatedAt int64        `json:"created_at,omitempty"`
	Status    string       `json:"status"`
	Error     interface{}  `json:"error,omitempty"`
	Output    []OutputItem `json:"output"`

	Instructions       *string           `json:"instructions,omitempty"`
	MaxOutputTokens    *int              `json:"max_output_tokens,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
	Model              string            `json:"model"`
	ParallelToolCalls  bool              `json:"parallel_tool_calls"`
	PreviousResponseID *string           `json:"previous_response_id,omitempty"`
	Reasoning          *ReasoningConfig  `json:"reasoning,omitempty"`
	Store              *bool             `json:"store,omitempty"`
	Temperature        *float64          `json:"temperature,omitempty"`
	Text               *TextConfig       `json:"text,omitempty"`
	ToolChoice         interface{}       `json:"tool_choice,omitempty"`
	Tools              []Tool            `json:"tools,omitempty"`
	TopP               *float64          `json:"top_p,omitempty"`
	Truncation         string            `json:"truncation,omitempty"`
	Usage              *UsageInfo        `json:"usage,omitempty"`
	User               *string           `json:"user,omitempty"`
	OutputText         string            `json:"output_text,omitempty"`
	Parallel           *ParallelConfig   `json:"parallel,omitempty"`
}

type OutputItem struct {
	Type      string         `json:"type"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Role      string         `json:"role,omitempty"`
	Content   []ContentBlock `json:"content,omitempty"`
	CallID    string         `json:"call_id,omitempty"`
	Arguments interface{}    `json:"arguments,omitempty"`
	Status    string         `json:"status,omitempty"`
	Output    string         `json:"output,omitempty"`
}

type ContentBlock struct {
	Type        string        `json:"type"`
	Text        string        `json:"text,omitempty"`
	Content     string        `json:"content,omitempty"`
	Annotations []interface{} `json:"annotations,omitempty"`
	Logprobs    []interface{} `json:"logprobs,omitempty"`
}

type UsageInfo struct {
	InputTokens         int                 `json:"input_tokens,omitempty"`
	InputTokensDetails  *InputTokensDetail  `json:"input_tokens_details,omitempty"`
	OutputTokens        int                 `json:"output_tokens,omitempty"`
	OutputTokensDetails *OutputTokensDetail `json:"output_tokens_details,omitempty"`
	TotalTokens         int                 `json:"total_tokens,omitempty"`
}

type InputTokensDetail struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
}

type OutputTokensDetail struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

type ParallelConfig struct {
	MaxParallelism int `json:"max_parallelism,omitempty"`
}

type SSEEvent struct {
	Type string      `json:"type"`
	Data interface{} `json:"data,omitempty"`
}
