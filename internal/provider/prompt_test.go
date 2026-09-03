package provider

import (
	"chatgpt-mcp/internal/chatgpt"
	"encoding/json"
	"strings"
	"testing"
)

func TestCompilePromptPreservesRolesAndTextParts(t *testing.T) {
	req := chatCompletionRequest{
		Messages: []message{
			{Role: "system", Content: json.RawMessage(`"follow the rules"`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"line one"},{"type":"text","text":"line two"}]`)},
		},
	}
	protocol := newToolProtocol("test_nonce")
	prompt, _, enabled, err := compilePrompt(req, protocol)
	if err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("tools should not be enabled")
	}
	for _, want := range []string{`"role": "system"`, `"content": "line one\nline two"`, "CHATGPT_MCP_REQUEST_BEGIN"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("compiled prompt does not contain %q:\n%s", want, prompt)
		}
	}
}

func TestCompilePromptRejectsMultimodalContent(t *testing.T) {
	req := chatCompletionRequest{Messages: []message{{
		Role:    "user",
		Content: json.RawMessage(`[{"type":"image_url","image_url":{"url":"https://example.test/a.png"}}]`),
	}}}
	_, _, _, err := compilePrompt(req, newToolProtocol("nonce"))
	if err == nil || !strings.Contains(err.Error(), "unsupported type") {
		t.Fatalf("expected unsupported content error, got %v", err)
	}
}

func TestCompilePromptValidatesResponseFormat(t *testing.T) {
	tests := []struct {
		name   string
		format string
		want   string
	}{
		{name: "not an object", format: `"json_object"`, want: "must be an object"},
		{name: "missing type", format: `{}`, want: "type is required"},
		{name: "unknown type", format: `{"type":"yaml"}`, want: "is not supported"},
		{name: "unknown top-level field", format: `{"type":"json_object","extra":true}`, want: "unsupported field"},
		{name: "schema on json object", format: `{"type":"json_object","json_schema":{}}`, want: "only valid"},
		{name: "missing schema wrapper", format: `{"type":"json_schema"}`, want: "is required"},
		{name: "schema wrapper is array", format: `{"type":"json_schema","json_schema":[]}`, want: "must be an object"},
		{name: "missing schema name", format: `{"type":"json_schema","json_schema":{"schema":{"type":"object"}}}`, want: "name must match"},
		{name: "invalid schema name", format: `{"type":"json_schema","json_schema":{"name":"not valid","schema":{"type":"object"}}}`, want: "name must match"},
		{name: "schema is array", format: `{"type":"json_schema","json_schema":{"name":"answer","schema":[]}}`, want: "must be a JSON object"},
		{name: "strict is not boolean", format: `{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object"},"strict":"true"}}`, want: "strict must be a boolean"},
		{name: "unknown schema wrapper field", format: `{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object"},"extra":true}}`, want: "unsupported field"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := chatCompletionRequest{
				Messages:       []message{{Role: "user", Content: json.RawMessage(`"hello"`)}},
				ResponseFormat: json.RawMessage(test.format),
			}
			_, _, _, err := compilePrompt(req, newToolProtocol("nonce"))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected response_format error containing %q, got %v", test.want, err)
			}
		})
	}
}

func TestCompilePromptDescribesResponseFormat(t *testing.T) {
	tests := []struct {
		name      string
		format    string
		want      string
		doNotWant string
	}{
		{name: "text", format: `{"type":"text"}`, doNotWant: "output only valid JSON"},
		{name: "json object", format: `{"type":"json_object"}`, want: "output only one valid JSON object"},
		{name: "non-strict JSON schema", format: `{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object"}}}`, want: "follow response_format.json_schema.schema as closely as possible", doNotWant: "exactly matches"},
		{name: "strict JSON schema", format: `{"type":"json_schema","json_schema":{"name":"answer","strict":true,"schema":{"type":"object"}}}`, want: "exactly matches response_format.json_schema.schema"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := chatCompletionRequest{
				Messages:       []message{{Role: "user", Content: json.RawMessage(`"hello"`)}},
				ResponseFormat: json.RawMessage(test.format),
			}
			prompt, _, _, err := compilePrompt(req, newToolProtocol("nonce"))
			if err != nil {
				t.Fatal(err)
			}
			if test.want != "" && !strings.Contains(prompt, test.want) {
				t.Fatalf("compiled prompt does not contain %q:\n%s", test.want, prompt)
			}
			if test.doNotWant != "" && strings.Contains(prompt, test.doNotWant) {
				t.Fatalf("compiled prompt unexpectedly contains %q:\n%s", test.doNotWant, prompt)
			}
		})
	}
}

func TestNormalizeResponseContent(t *testing.T) {
	strictSchema := `{"type":"json_schema","json_schema":{"name":"answer","strict":true,"schema":{"type":"object","required":["answer"],"additionalProperties":false,"properties":{"answer":{"type":"string"}}}}}`
	nonStrictSchema := `{"type":"json_schema","json_schema":{"name":"answer","strict":false,"schema":{"type":"object","required":["answer"]}}}`
	tests := []struct {
		name    string
		content string
		format  string
		want    string
		wantErr string
	}{
		{name: "text remains unchanged", content: "  ordinary text  ", format: `{"type":"text"}`, want: "  ordinary text  "},
		{name: "JSON object is unfenced and compacted", content: "```json\n{\n  \"answer\": \"yes\"\n}\n```", format: `{"type":"json_object"}`, want: `{"answer":"yes"}`},
		{name: "JSON object rejects array", content: `[]`, format: `{"type":"json_object"}`, wantErr: "requires a JSON object"},
		{name: "JSON object rejects malformed JSON", content: `{not json}`, format: `{"type":"json_object"}`, wantErr: "requires valid JSON"},
		{name: "strict JSON schema accepts match", content: `{ "answer": "yes" }`, format: strictSchema, want: `{"answer":"yes"}`},
		{name: "strict JSON schema rejects mismatch", content: `{"other":true}`, format: strictSchema, wantErr: "does not match"},
		{name: "non-strict JSON schema forwards mismatch", content: `[]`, format: nonStrictSchema, want: `[]`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := normalizeResponseContent(test.content, json.RawMessage(test.format))
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("expected error containing %q, got %v", test.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("normalized content = %q, want %q", got, test.want)
			}
		})
	}
}

func TestResultOutputAppliesResponseFormat(t *testing.T) {
	req := chatCompletionRequest{ResponseFormat: json.RawMessage(`{"type":"json_object"}`)}
	content, calls, err := (&Server{}).resultOutput(
		&chatgpt.AskResult{Response: "```json\n{ \"answer\": true }\n```"},
		nil,
		false,
		req,
		newToolProtocol("nonce"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if content != `{"answer":true}` || len(calls) != 0 {
		t.Fatalf("unexpected formatted result: content=%q calls=%#v", content, calls)
	}
}

func TestResultOutputUsesUnescapedRawResponseForProtocols(t *testing.T) {
	protocol := newToolProtocol("nonce_with_underscores")
	raw := protocol.open + `{"tool_calls":[{"name":"my_tool","arguments":{"snake_key":"snake_value"}}]}` + protocol.close
	markdownEscaped := strings.ReplaceAll(raw, "_", `\_`)
	content, calls, err := (&Server{}).resultOutput(
		&chatgpt.AskResult{Response: markdownEscaped, RawResponse: raw},
		map[string]toolDefinition{
			"my_tool": {Name: "my_tool", Parameters: json.RawMessage(`{"type":"object"}`)},
		},
		true,
		chatCompletionRequest{},
		protocol,
	)
	if err != nil {
		t.Fatal(err)
	}
	if content != "" || len(calls) != 1 {
		t.Fatalf("unexpected output: content=%q calls=%#v", content, calls)
	}
	if calls[0].Function.Name != "my_tool" || calls[0].Function.Arguments != `{"snake_key":"snake_value"}` {
		t.Fatalf("raw protocol was not preserved: %#v", calls[0])
	}
}

func TestResultOutputUsesUnescapedRawResponseForPlainText(t *testing.T) {
	content, calls, err := (&Server{}).resultOutput(
		&chatgpt.AskResult{Response: `SINGLE\_PROVIDER\_OK`, RawResponse: "SINGLE_PROVIDER_OK"},
		nil,
		false,
		chatCompletionRequest{},
		newToolProtocol("nonce"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if content != "SINGLE_PROVIDER_OK" || len(calls) != 0 {
		t.Fatalf("unexpected plain-text result: content=%q calls=%#v", content, calls)
	}
}

func TestResultOutputUsesRawResponseForStructuredJSON(t *testing.T) {
	req := chatCompletionRequest{ResponseFormat: json.RawMessage(`{"type":"json_object"}`)}
	content, calls, err := (&Server{}).resultOutput(
		&chatgpt.AskResult{Response: `{"snake\_key":"snake\_value"}`, RawResponse: `{"snake_key":"snake_value"}`},
		nil,
		false,
		req,
		newToolProtocol("nonce"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if content != `{"snake_key":"snake_value"}` || len(calls) != 0 {
		t.Fatalf("unexpected structured result: content=%q calls=%#v", content, calls)
	}
}

func TestParseAssistantToolCalls(t *testing.T) {
	protocol := newToolProtocol("nonce")
	text := protocol.open + `
{"tool_calls":[{"name":"read_file","arguments":{"path":"README.md"}}]}
` + protocol.close
	content, calls, err := parseAssistantOutput(text, map[string]toolDefinition{
		"read_file": {Name: "read_file", Parameters: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}`)},
	}, true, nil, protocol)
	if err != nil {
		t.Fatal(err)
	}
	if content != "" || len(calls) != 1 {
		t.Fatalf("unexpected parsed output: content=%q calls=%#v", content, calls)
	}
	if calls[0].Function.Name != "read_file" || calls[0].Function.Arguments != `{"path":"README.md"}` {
		t.Fatalf("unexpected tool call: %#v", calls[0])
	}
	if calls[0].ID == "" || calls[0].Type != "function" {
		t.Fatalf("missing server-generated metadata: %#v", calls[0])
	}
}

func TestParseAssistantToolCallsRequiresMatchingNonceAndAllowedName(t *testing.T) {
	protocol := newToolProtocol("expected")
	wrong := newToolProtocol("wrong")
	text := wrong.open + `{"tool_calls":[{"name":"shell","arguments":{}}]}` + wrong.close
	content, calls, err := parseAssistantOutput(text, map[string]toolDefinition{"shell": {Name: "shell", Parameters: json.RawMessage(`{"type":"object"}`)}}, true, nil, protocol)
	if err != nil || content != text || len(calls) != 0 {
		t.Fatalf("wrong nonce should remain ordinary text: content=%q calls=%v err=%v", content, calls, err)
	}

	text = protocol.open + `{"tool_calls":[{"name":"unknown","arguments":{}}]}` + protocol.close
	_, _, err = parseAssistantOutput(text, map[string]toolDefinition{"shell": {Name: "shell", Parameters: json.RawMessage(`{"type":"object"}`)}}, true, nil, protocol)
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("expected unknown tool error, got %v", err)
	}
}

func TestParseAssistantToolCallsRejectsNonExactFrames(t *testing.T) {
	protocol := newToolProtocol("nonce")
	payload := `{"tool_calls":[{"name":"shell","arguments":{}}]}`
	frame := protocol.open + payload + protocol.close
	for _, test := range []struct {
		name string
		text string
	}{
		{name: "prefix", text: "explanation\n" + frame},
		{name: "suffix", text: frame + "\nexplanation"},
		{name: "duplicate frame", text: frame + "\n" + frame},
		{name: "duplicate opening marker", text: protocol.open + protocol.open + payload + protocol.close},
		{name: "duplicate closing marker", text: protocol.open + payload + protocol.close + protocol.close},
		{name: "missing closing marker", text: protocol.open + payload},
		{name: "closing marker only", text: protocol.close},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := parseAssistantOutput(test.text, map[string]toolDefinition{
				"shell": {Name: "shell", Parameters: json.RawMessage(`{"type":"object"}`)},
			}, true, nil, protocol)
			if err == nil {
				t.Fatalf("expected malformed tool frame to be rejected: %q", test.text)
			}
		})
	}
}

func TestParseAssistantToolCallsRequiresExactEnvelopeShape(t *testing.T) {
	protocol := newToolProtocol("nonce")
	definition := map[string]toolDefinition{
		"shell": {Name: "shell", Parameters: json.RawMessage(`{"type":"object"}`)},
	}
	for _, test := range []struct {
		name    string
		payload string
	}{
		{name: "unknown envelope field", payload: `{"tool_calls":[{"name":"shell","arguments":{}}],"extra":true}`},
		{name: "trailing JSON value", payload: `{"tool_calls":[{"name":"shell","arguments":{}}]} {}`},
		{name: "unknown call field", payload: `{"tool_calls":[{"name":"shell","arguments":{},"extra":true}]}`},
		{name: "flat missing name", payload: `{"tool_calls":[{"arguments":{}}]}`},
		{name: "flat missing arguments", payload: `{"tool_calls":[{"name":"shell"}]}`},
		{name: "mixed flat and function forms", payload: `{"tool_calls":[{"name":"shell","arguments":{},"function":{"name":"shell","arguments":{}}}]}`},
		{name: "function missing name", payload: `{"tool_calls":[{"function":{"arguments":{}}}]}`},
		{name: "function missing arguments", payload: `{"tool_calls":[{"function":{"name":"shell"}}]}`},
		{name: "unknown function field", payload: `{"tool_calls":[{"function":{"name":"shell","arguments":{},"extra":true}}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			text := protocol.open + test.payload + protocol.close
			_, _, err := parseAssistantOutput(text, definition, true, nil, protocol)
			if err == nil {
				t.Fatalf("expected invalid tool-call envelope to be rejected: %s", test.payload)
			}
		})
	}
}

func TestParseAssistantToolCallsAcceptsExactFunctionEnvelope(t *testing.T) {
	protocol := newToolProtocol("nonce")
	text := protocol.open + `{"tool_calls":[{"function":{"name":"shell","arguments":{"command":"pwd"}}}]}` + protocol.close
	_, calls, err := parseAssistantOutput(text, map[string]toolDefinition{
		"shell": {Name: "shell", Parameters: json.RawMessage(`{"type":"object"}`)},
	}, true, nil, protocol)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Function.Name != "shell" || calls[0].Function.Arguments != `{"command":"pwd"}` {
		t.Fatalf("unexpected function-form tool call: %#v", calls)
	}
}

func TestParseAssistantToolCallsValidatesSchema(t *testing.T) {
	protocol := newToolProtocol("nonce")
	text := protocol.open + `{"tool_calls":[{"name":"read_file","arguments":{}}]}` + protocol.close
	strict := true
	_, _, err := parseAssistantOutput(text, map[string]toolDefinition{
		"read_file": {Name: "read_file", Parameters: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}`), Strict: &strict},
	}, true, nil, protocol)
	if err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("expected schema validation error, got %v", err)
	}
}

func TestParseAssistantToolCallsDoesNotEnforceNonStrictSchema(t *testing.T) {
	protocol := newToolProtocol("nonce")
	text := protocol.open + `{"tool_calls":[{"name":"read_file","arguments":{}}]}` + protocol.close
	strict := false
	for _, test := range []struct {
		name   string
		strict *bool
	}{
		{name: "omitted"},
		{name: "false", strict: &strict},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, calls, err := parseAssistantOutput(text, map[string]toolDefinition{
				"read_file": {Name: "read_file", Parameters: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}`), Strict: test.strict},
			}, true, nil, protocol)
			if err != nil || len(calls) != 1 {
				t.Fatalf("non-strict tool call should be forwarded: calls=%#v err=%v", calls, err)
			}
		})
	}
}

func TestParseAssistantToolCallsForwardsRepairableNonStrictArguments(t *testing.T) {
	protocol := newToolProtocol("nonce")
	definition := map[string]toolDefinition{
		"read_file": {Name: "read_file", Parameters: json.RawMessage(`{"type":"object"}`)},
	}
	for _, test := range []struct {
		name      string
		arguments string
		want      string
	}{
		{name: "malformed JSON string", arguments: `"{not-json"`, want: `{not-json`},
		{name: "raw array", arguments: `[1, 2]`, want: `[1,2]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			text := protocol.open + `{"tool_calls":[{"name":"read_file","arguments":` + test.arguments + `}]}` + protocol.close
			_, calls, err := parseAssistantOutput(text, definition, true, nil, protocol)
			if err != nil || len(calls) != 1 || calls[0].Function.Arguments != test.want {
				t.Fatalf("repairable arguments were not forwarded: calls=%#v err=%v", calls, err)
			}
		})
	}
}

func TestParseAssistantToolCallsStrictRejectsMalformedArgumentString(t *testing.T) {
	protocol := newToolProtocol("nonce")
	strict := true
	text := protocol.open + `{"tool_calls":[{"name":"read_file","arguments":"{not-json"}]}` + protocol.close
	_, _, err := parseAssistantOutput(text, map[string]toolDefinition{
		"read_file": {Name: "read_file", Parameters: json.RawMessage(`{"type":"object"}`), Strict: &strict},
	}, true, nil, protocol)
	if err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("strict malformed arguments should fail validation, got %v", err)
	}
}

func TestParseAssistantToolCallsStrictRejectsTrailingArgumentJSON(t *testing.T) {
	protocol := newToolProtocol("nonce")
	strict := true
	for _, arguments := range []string{`{} {}`, `{} trailing`} {
		t.Run(arguments, func(t *testing.T) {
			encoded, err := json.Marshal(arguments)
			if err != nil {
				t.Fatal(err)
			}
			text := protocol.open + `{"tool_calls":[{"name":"read_file","arguments":` + string(encoded) + `}]}` + protocol.close
			_, _, err = parseAssistantOutput(text, map[string]toolDefinition{
				"read_file": {Name: "read_file", Parameters: json.RawMessage(`{"type":"object"}`), Strict: &strict},
			}, true, nil, protocol)
			if err == nil || !strings.Contains(err.Error(), "schema") {
				t.Fatalf("strict trailing arguments should fail validation, got %v", err)
			}
		})
	}
}

func TestCompilePromptValidatesToolChoice(t *testing.T) {
	req := chatCompletionRequest{
		Messages:   []message{{Role: "user", Content: json.RawMessage(`"hello"`)}},
		Tools:      []tool{{Type: "function", Function: toolDefinition{Name: "known", Parameters: json.RawMessage(`{"type":"object"}`)}}},
		ToolChoice: json.RawMessage(`{"type":"function","function":{"name":"missing"}}`),
	}
	_, _, _, err := compilePrompt(req, newToolProtocol("nonce"))
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("expected unknown named tool error, got %v", err)
	}
}

func TestCompilePromptDescribesToolChoiceConstraints(t *testing.T) {
	parallel := false
	tests := []struct {
		name      string
		choice    json.RawMessage
		parallel  *bool
		want      string
		doNotWant string
	}{
		{name: "auto", want: "If no tool is needed, answer normally"},
		{name: "null uses auto", choice: json.RawMessage(`null`), want: "If no tool is needed, answer normally"},
		{name: "required", choice: json.RawMessage(`"required"`), want: "You must make at least one tool call", doNotWant: "If no tool is needed"},
		{name: "named", choice: json.RawMessage(`{"type":"function","function":{"name":"read_file"}}`), want: `You must call the function "read_file"`, doNotWant: "If no tool is needed"},
		{name: "parallel disabled", parallel: &parallel, want: "Parallel tool calls are disabled; if you call a tool, make exactly one call"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := chatCompletionRequest{
				Messages:          []message{{Role: "user", Content: json.RawMessage(`"hello"`)}},
				Tools:             []tool{{Type: "function", Function: toolDefinition{Name: "read_file", Parameters: json.RawMessage(`{"type":"object"}`)}}},
				ToolChoice:        test.choice,
				ParallelToolCalls: test.parallel,
			}
			prompt, _, _, err := compilePrompt(req, newToolProtocol("nonce"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(prompt, test.want) {
				t.Fatalf("compiled prompt does not contain %q:\n%s", test.want, prompt)
			}
			if test.doNotWant != "" && strings.Contains(prompt, test.doNotWant) {
				t.Fatalf("compiled prompt unexpectedly contains %q:\n%s", test.doNotWant, prompt)
			}
		})
	}
}

func TestCompilePromptTreatsNullToolChoiceAsOmittedWithoutTools(t *testing.T) {
	req := chatCompletionRequest{
		Messages:   []message{{Role: "user", Content: json.RawMessage(`"hello"`)}},
		ToolChoice: json.RawMessage(`null`),
	}
	if _, _, enabled, err := compilePrompt(req, newToolProtocol("nonce")); err != nil || enabled {
		t.Fatalf("null tool_choice should behave like omission without tools: enabled=%v err=%v", enabled, err)
	}
}

func TestValidateRequestRejectsExplicitNZero(t *testing.T) {
	var explicitZero chatCompletionRequest
	if err := json.Unmarshal([]byte(`{"n":0}`), &explicitZero); err != nil {
		t.Fatal(err)
	}
	if err := validateRequest(explicitZero); err == nil || !strings.Contains(err.Error(), "n values other than 1") {
		t.Fatalf("expected n=0 validation error, got %v", err)
	}

	var omitted chatCompletionRequest
	if err := json.Unmarshal([]byte(`{}`), &omitted); err != nil {
		t.Fatal(err)
	}
	if err := validateRequest(omitted); err != nil {
		t.Fatalf("omitted n should use the API default: %v", err)
	}
}
