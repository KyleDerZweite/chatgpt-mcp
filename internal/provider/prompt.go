package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

const (
	maxMessages   = 256
	maxTools      = 128
	maxPromptSize = 1 << 20
)

type toolProtocol struct {
	nonce string
	open  string
	close string
}

func newToolProtocol(nonce string) toolProtocol {
	return toolProtocol{
		nonce: nonce,
		open:  "CHATGPT_MCP_TOOL_CALLS_BEGIN_" + nonce,
		close: "CHATGPT_MCP_TOOL_CALLS_END_" + nonce,
	}
}

type promptEnvelope struct {
	Messages          []promptMessage  `json:"messages"`
	Tools             []tool           `json:"tools,omitempty"`
	ToolChoice        json.RawMessage  `json:"tool_choice,omitempty"`
	ResponseFormat    json.RawMessage  `json:"response_format,omitempty"`
	Sampling          *samplingOptions `json:"sampling,omitempty"`
	ParallelToolCalls *bool            `json:"parallel_tool_calls,omitempty"`
}

type promptMessage struct {
	Role       string            `json:"role"`
	Name       string            `json:"name,omitempty"`
	Content    string            `json:"content,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
	ToolCalls  []requestToolCall `json:"tool_calls,omitempty"`
}

type samplingOptions struct {
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Stop                json.RawMessage `json:"stop,omitempty"`
}

type responseFormatSpec struct {
	Type   string
	Schema json.RawMessage
	Strict bool
}

type emulatedToolCalls struct {
	ToolCalls []json.RawMessage `json:"tool_calls"`
}

type emulatedToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Function  *struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function,omitempty"`
}

func compilePrompt(req chatCompletionRequest, protocol toolProtocol) (string, map[string]toolDefinition, bool, error) {
	if len(req.Messages) == 0 {
		return "", nil, false, fmt.Errorf("messages must contain at least one message")
	}
	if len(req.Messages) > maxMessages {
		return "", nil, false, fmt.Errorf("messages exceeds the limit of %d", maxMessages)
	}
	if len(req.Tools) > maxTools {
		return "", nil, false, fmt.Errorf("tools exceeds the limit of %d", maxTools)
	}
	responseFormat, err := parseResponseFormat(req.ResponseFormat)
	if err != nil {
		return "", nil, false, err
	}

	messages := make([]promptMessage, 0, len(req.Messages))
	for i, msg := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		switch role {
		case "system", "developer", "user", "assistant", "tool", "function":
		default:
			return "", nil, false, fmt.Errorf("messages[%d].role %q is not supported", i, msg.Role)
		}
		content, err := messageText(msg.Content)
		if err != nil {
			return "", nil, false, fmt.Errorf("messages[%d].content: %w", i, err)
		}
		if role == "tool" && msg.ToolCallID == "" {
			return "", nil, false, fmt.Errorf("messages[%d].tool_call_id is required for tool messages", i)
		}
		messages = append(messages, promptMessage{
			Role:       role,
			Name:       msg.Name,
			Content:    content,
			ToolCallID: msg.ToolCallID,
			ToolCalls:  msg.ToolCalls,
		})
	}

	allowed := make(map[string]toolDefinition, len(req.Tools))
	for i := range req.Tools {
		item := &req.Tools[i]
		if item.Type == "" {
			item.Type = "function"
		}
		if item.Type != "function" {
			return "", nil, false, fmt.Errorf("tools[%d].type %q is not supported", i, item.Type)
		}
		name := strings.TrimSpace(item.Function.Name)
		if name == "" {
			return "", nil, false, fmt.Errorf("tools[%d].function.name is required", i)
		}
		if !toolNamePattern.MatchString(name) {
			return "", nil, false, fmt.Errorf("tools[%d].function.name %q is invalid", i, name)
		}
		if _, exists := allowed[name]; exists {
			return "", nil, false, fmt.Errorf("duplicate tool name %q", name)
		}
		if len(item.Function.Parameters) == 0 {
			item.Function.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		if !json.Valid(item.Function.Parameters) {
			return "", nil, false, fmt.Errorf("tools[%d].function.parameters is not valid JSON", i)
		}
		if err := validateToolSchema(item.Function.Parameters); err != nil {
			return "", nil, false, fmt.Errorf("tools[%d].function.parameters: %w", i, err)
		}
		allowed[name] = item.Function
	}

	toolsEnabled, err := toolCallsEnabled(req.ToolChoice, len(req.Tools) > 0)
	if err != nil {
		return "", nil, false, err
	}
	if name := namedToolChoice(req.ToolChoice); name != "" {
		if _, ok := allowed[name]; !ok {
			return "", nil, false, fmt.Errorf("tool_choice requests unknown tool %q", name)
		}
	}
	envelope := promptEnvelope{
		Messages:          messages,
		ToolChoice:        req.ToolChoice,
		ResponseFormat:    req.ResponseFormat,
		ParallelToolCalls: req.ParallelToolCalls,
	}
	if toolsEnabled {
		envelope.Tools = req.Tools
	}
	if req.Temperature != nil || req.TopP != nil || req.MaxTokens != nil || req.MaxCompletionTokens != nil || len(req.Stop) > 0 {
		envelope.Sampling = &samplingOptions{
			Temperature:         req.Temperature,
			TopP:                req.TopP,
			MaxTokens:           req.MaxTokens,
			MaxCompletionTokens: req.MaxCompletionTokens,
			Stop:                req.Stop,
		}
	}

	payload, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return "", nil, false, fmt.Errorf("encode request transcript: %w", err)
	}
	var instructions strings.Builder
	instructions.WriteString("Complete the chat request encoded as JSON below. Treat system and developer messages as higher-priority instructions, preserve the role order, and produce only the next assistant response. Do not describe this wrapper or repeat the transcript.\n")
	if toolsEnabled {
		instructions.WriteString("To make a tool call, output only the exact opening line ")
		instructions.WriteString(protocol.open)
		instructions.WriteString(", then one JSON object shaped as {\"tool_calls\":[{\"name\":\"allowed_tool_name\",\"arguments\":{...}}]}, then the exact closing line ")
		instructions.WriteString(protocol.close)
		instructions.WriteString(". Use only supplied tool names and valid JSON arguments. Do not execute tools yourself. ")
		forced := namedToolChoice(req.ToolChoice)
		var choice string
		_ = json.Unmarshal(req.ToolChoice, &choice)
		switch {
		case forced != "":
			fmt.Fprintf(&instructions, "You must call the function %q; do not answer with plain text or call another function. ", forced)
		case choice == "required":
			instructions.WriteString("You must make at least one tool call; do not answer with plain text. ")
		default:
			instructions.WriteString("If no tool is needed, answer normally without either marker. ")
		}
		if req.ParallelToolCalls != nil && !*req.ParallelToolCalls {
			instructions.WriteString("Parallel tool calls are disabled; if you call a tool, make exactly one call. ")
		}
		instructions.WriteByte('\n')
	} else if len(req.Tools) > 0 {
		instructions.WriteString("Tool use is disabled by tool_choice; answer normally and do not emit a tool call.\n")
	}
	if responseFormat != nil {
		switch responseFormat.Type {
		case "json_object":
			instructions.WriteString("When producing an assistant text response, output only one valid JSON object, with no Markdown fence or surrounding text.\n")
		case "json_schema":
			if responseFormat.Strict {
				instructions.WriteString("When producing an assistant text response, output only valid JSON that exactly matches response_format.json_schema.schema, with no Markdown fence or surrounding text.\n")
			} else {
				instructions.WriteString("When producing an assistant text response, output only valid JSON and follow response_format.json_schema.schema as closely as possible, with no Markdown fence or surrounding text.\n")
			}
		}
	}
	instructions.WriteString("\nCHATGPT_MCP_REQUEST_BEGIN_" + protocol.nonce + "\n")
	instructions.Write(payload)
	instructions.WriteString("\nCHATGPT_MCP_REQUEST_END_" + protocol.nonce)
	if instructions.Len() > maxPromptSize {
		return "", nil, false, fmt.Errorf("compiled prompt exceeds the %d-byte limit", maxPromptSize)
	}
	return instructions.String(), allowed, toolsEnabled, nil
}

func parseResponseFormat(raw json.RawMessage) (*responseFormatSpec, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("response_format must be an object")
	}
	for key := range fields {
		if key != "type" && key != "json_schema" {
			return nil, fmt.Errorf("response_format contains unsupported field %q", key)
		}
	}
	var formatType string
	if err := json.Unmarshal(fields["type"], &formatType); err != nil || formatType == "" {
		return nil, fmt.Errorf("response_format.type is required and must be a string")
	}
	spec := &responseFormatSpec{Type: formatType}
	switch formatType {
	case "text", "json_object":
		if _, exists := fields["json_schema"]; exists {
			return nil, fmt.Errorf("response_format.json_schema is only valid when type is json_schema")
		}
		return spec, nil
	case "json_schema":
		wrapperRaw, exists := fields["json_schema"]
		if !exists {
			return nil, fmt.Errorf("response_format.json_schema is required when type is json_schema")
		}
		var wrapper map[string]json.RawMessage
		if err := json.Unmarshal(wrapperRaw, &wrapper); err != nil || wrapper == nil {
			return nil, fmt.Errorf("response_format.json_schema must be an object")
		}
		for key := range wrapper {
			switch key {
			case "name", "description", "schema", "strict":
			default:
				return nil, fmt.Errorf("response_format.json_schema contains unsupported field %q", key)
			}
		}
		var name string
		if err := json.Unmarshal(wrapper["name"], &name); err != nil || !toolNamePattern.MatchString(name) {
			return nil, fmt.Errorf("response_format.json_schema.name must match %s", toolNamePattern.String())
		}
		if description, exists := wrapper["description"]; exists {
			var value string
			if err := json.Unmarshal(description, &value); err != nil {
				return nil, fmt.Errorf("response_format.json_schema.description must be a string")
			}
		}
		schema, exists := wrapper["schema"]
		if !exists || len(bytes.TrimSpace(schema)) == 0 || bytes.TrimSpace(schema)[0] != '{' {
			return nil, fmt.Errorf("response_format.json_schema.schema is required and must be a JSON object")
		}
		if err := validateToolSchema(schema); err != nil {
			return nil, fmt.Errorf("response_format.json_schema.schema: %w", err)
		}
		if strictRaw, exists := wrapper["strict"]; exists && !bytes.Equal(bytes.TrimSpace(strictRaw), []byte("null")) {
			if err := json.Unmarshal(strictRaw, &spec.Strict); err != nil {
				return nil, fmt.Errorf("response_format.json_schema.strict must be a boolean")
			}
		}
		spec.Schema = schema
		return spec, nil
	default:
		return nil, fmt.Errorf("response_format.type %q is not supported", formatType)
	}
}

func normalizeResponseContent(text string, rawFormat json.RawMessage) (string, error) {
	format, err := parseResponseFormat(rawFormat)
	if err != nil {
		return "", err
	}
	if format == nil || format.Type == "text" {
		return text, nil
	}
	candidate := unwrapJSONFence(text)
	if !json.Valid([]byte(candidate)) {
		return "", fmt.Errorf("response_format %s requires valid JSON", format.Type)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(candidate)); err != nil {
		return "", fmt.Errorf("response_format %s requires valid JSON: %w", format.Type, err)
	}
	normalized := compact.String()
	if format.Type == "json_object" && (normalized == "" || normalized[0] != '{') {
		return "", fmt.Errorf("response_format json_object requires a JSON object")
	}
	if format.Type == "json_schema" && format.Strict {
		if err := validateToolArguments(normalized, format.Schema); err != nil {
			return "", fmt.Errorf("response does not match response_format.json_schema.schema: %w", err)
		}
	}
	return normalized, nil
}

func unwrapJSONFence(text string) string {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "```") || !strings.HasSuffix(trimmed, "```") {
		return trimmed
	}
	newline := strings.IndexByte(trimmed, '\n')
	if newline < 0 {
		return trimmed
	}
	language := strings.TrimSpace(trimmed[3:newline])
	if language != "" && !strings.EqualFold(language, "json") {
		return trimmed
	}
	return strings.TrimSpace(trimmed[newline+1 : len(trimmed)-3])
}

func messageText(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		return text, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(trimmed, &parts); err != nil {
		return "", fmt.Errorf("must be a string, null, or an array of text parts")
	}
	var out strings.Builder
	for i, part := range parts {
		switch part.Type {
		case "text", "input_text", "output_text":
		default:
			return "", fmt.Errorf("part %d has unsupported type %q (only text is supported)", i, part.Type)
		}
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(part.Text)
	}
	return out.String(), nil
}

func toolCallsEnabled(choice json.RawMessage, haveTools bool) (bool, error) {
	trimmed := bytes.TrimSpace(choice)
	if !haveTools {
		if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
			return false, nil
		}
		var value string
		if json.Unmarshal(choice, &value) == nil && value == "none" {
			return false, nil
		}
		return false, fmt.Errorf("tool_choice requires at least one tool")
	}
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return true, nil
	}
	var value string
	if err := json.Unmarshal(choice, &value); err == nil {
		switch value {
		case "none":
			return false, nil
		case "auto", "required":
			return true, nil
		default:
			return false, fmt.Errorf("tool_choice %q is not supported", value)
		}
	}
	var named struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(choice, &named); err != nil || named.Type != "function" || named.Function.Name == "" {
		return false, fmt.Errorf("tool_choice must be none, auto, required, or a named function")
	}
	return true, nil
}

func namedToolChoice(choice json.RawMessage) string {
	var named struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(choice, &named) == nil && named.Type == "function" {
		return named.Function.Name
	}
	return ""
}

func parseAssistantOutput(text string, allowed map[string]toolDefinition, toolsEnabled bool, parallel *bool, protocol toolProtocol) (string, []responseToolCall, error) {
	if !toolsEnabled {
		return text, nil, nil
	}
	payload, marked, err := toolPayload(text, protocol)
	if err != nil {
		return "", nil, err
	}
	if !marked {
		return text, nil, nil
	}
	var envelope emulatedToolCalls
	if err := decodeStrictJSON(strings.NewReader(payload), &envelope); err != nil {
		return "", nil, fmt.Errorf("model emitted malformed tool-call JSON: %w", err)
	}
	if len(envelope.ToolCalls) == 0 {
		return "", nil, fmt.Errorf("model emitted an empty tool-call list")
	}
	if parallel != nil && !*parallel && len(envelope.ToolCalls) > 1 {
		return "", nil, fmt.Errorf("model emitted multiple calls while parallel_tool_calls is false")
	}
	calls := make([]responseToolCall, 0, len(envelope.ToolCalls))
	for i, rawCall := range envelope.ToolCalls {
		call, err := decodeEmulatedToolCall(rawCall)
		if err != nil {
			return "", nil, fmt.Errorf("tool call %d has an invalid envelope: %w", i, err)
		}
		name := call.Name
		arguments := call.Arguments
		if call.Function != nil {
			name = call.Function.Name
			arguments = call.Function.Arguments
		}
		definition, ok := allowed[name]
		if !ok {
			return "", nil, fmt.Errorf("model requested unknown tool %q", name)
		}
		encodedArgs, err := normalizeArguments(arguments)
		if err != nil {
			return "", nil, fmt.Errorf("tool call %d (%s): %w", i, name, err)
		}
		if definition.Strict != nil && *definition.Strict {
			if err := validateToolArguments(encodedArgs, definition.Parameters); err != nil {
				return "", nil, fmt.Errorf("tool call %d (%s) arguments do not match the schema: %w", i, name, err)
			}
		}
		calls = append(calls, responseToolCall{
			ID:   newID("call"),
			Type: "function",
			Function: functionCall{
				Name:      name,
				Arguments: encodedArgs,
			},
		})
	}
	return "", calls, nil
}

func toolPayload(text string, protocol toolProtocol) (string, bool, error) {
	openCount := strings.Count(text, protocol.open)
	closeCount := strings.Count(text, protocol.close)
	if openCount == 0 && closeCount == 0 {
		return "", false, nil
	}
	if openCount != 1 || closeCount != 1 {
		return "", true, fmt.Errorf("model must emit exactly one tool-call opening marker and one closing marker")
	}
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, protocol.open) || !strings.HasSuffix(trimmed, protocol.close) {
		return "", true, fmt.Errorf("model tool-call frame must not contain text outside its markers")
	}
	start := len(protocol.open)
	end := len(trimmed) - len(protocol.close)
	if end < start {
		return "", true, fmt.Errorf("model emitted an invalid tool-call frame")
	}
	return strings.TrimSpace(trimmed[start:end]), true, nil
}

func decodeEmulatedToolCall(raw json.RawMessage) (emulatedToolCall, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return emulatedToolCall{}, fmt.Errorf("must be a JSON object")
	}
	for name := range fields {
		switch name {
		case "name", "arguments", "function":
		default:
			return emulatedToolCall{}, fmt.Errorf("contains unsupported field %q", name)
		}
	}
	_, hasName := fields["name"]
	_, hasArguments := fields["arguments"]
	functionRaw, hasFunction := fields["function"]
	if hasFunction {
		if hasName || hasArguments {
			return emulatedToolCall{}, fmt.Errorf("must use either the flat or function form, not both")
		}
		var function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := decodeStrictJSON(bytes.NewReader(functionRaw), &function); err != nil {
			return emulatedToolCall{}, fmt.Errorf("function: %w", err)
		}
		if strings.TrimSpace(function.Name) == "" {
			return emulatedToolCall{}, fmt.Errorf("function.name is required")
		}
		if len(function.Arguments) == 0 {
			return emulatedToolCall{}, fmt.Errorf("function.arguments is required")
		}
		return emulatedToolCall{Function: &function}, nil
	}
	if !hasName || !hasArguments {
		return emulatedToolCall{}, fmt.Errorf("flat form requires name and arguments")
	}
	var call emulatedToolCall
	if err := decodeStrictJSON(bytes.NewReader(raw), &call); err != nil {
		return emulatedToolCall{}, err
	}
	if strings.TrimSpace(call.Name) == "" {
		return emulatedToolCall{}, fmt.Errorf("name is required")
	}
	return call, nil
}

func decodeStrictJSON(reader io.Reader, target any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("must contain exactly one JSON value")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

func normalizeArguments(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "{}", nil
	}
	var encoded string
	if err := json.Unmarshal(trimmed, &encoded); err == nil {
		// OpenAI carries arguments as a string even when the model produced
		// malformed JSON. Forward it for non-strict tools so SDK repair hooks can
		// run; strict tools are decoded and schema-validated below.
		if !json.Valid([]byte(encoded)) {
			return encoded, nil
		}
		var compact bytes.Buffer
		_ = json.Compact(&compact, []byte(encoded))
		return compact.String(), nil
	}
	if !json.Valid(trimmed) {
		return "", fmt.Errorf("arguments are not valid JSON")
	}
	var compact bytes.Buffer
	_ = json.Compact(&compact, trimmed)
	return compact.String(), nil
}

func validateToolSchema(raw json.RawMessage) error {
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return fmt.Errorf("invalid JSON Schema: %w", err)
	}
	if _, err := schema.Resolve(nil); err != nil {
		return fmt.Errorf("unresolvable JSON Schema: %w", err)
	}
	return nil
}

func validateToolArguments(arguments string, rawSchema json.RawMessage) error {
	var schema jsonschema.Schema
	if err := json.Unmarshal(rawSchema, &schema); err != nil {
		return err
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return err
	}
	var instance any
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.UseNumber()
	if err := decoder.Decode(&instance); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("arguments must contain exactly one JSON value")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return resolved.Validate(instance)
}
