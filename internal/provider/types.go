package provider

import "encoding/json"

type chatCompletionRequest struct {
	Model               string          `json:"model"`
	Messages            []message       `json:"messages"`
	Stream              bool            `json:"stream"`
	StreamOptions       streamOptions   `json:"stream_options"`
	Tools               []tool          `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls"`
	ResponseFormat      json.RawMessage `json:"response_format"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	MaxTokens           *int            `json:"max_tokens"`
	MaxCompletionTokens *int            `json:"max_completion_tokens"`
	Stop                json.RawMessage `json:"stop"`
	N                   *int            `json:"n"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type message struct {
	Role       string            `json:"role"`
	Name       string            `json:"name,omitempty"`
	Content    json.RawMessage   `json:"content"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
	ToolCalls  []requestToolCall `json:"tool_calls,omitempty"`
}

type requestToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type tool struct {
	Type     string         `json:"type"`
	Function toolDefinition `json:"function"`
}

type toolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type responseToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

type responseMessage struct {
	Role      string             `json:"role"`
	Content   *string            `json:"content"`
	ToolCalls []responseToolCall `json:"tool_calls,omitempty"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type apiError struct {
	Error apiErrorDetail `json:"error"`
}

type apiErrorDetail struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}
