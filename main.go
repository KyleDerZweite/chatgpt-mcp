package main

import (
	"context"
	"log"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"chatgpt-mcp/internal/browser"
	"chatgpt-mcp/internal/chatgpt"
	"chatgpt-mcp/internal/config"
)

const (
	serverName    = "chatgpt-mcp"
	serverVersion = "0.1.0"
)

type askInput struct {
	Prompt         string `json:"prompt" jsonschema:"The prompt to send to ChatGPT"`
	Model          string `json:"model,omitempty" jsonschema:"Optional model, e.g. gpt-5, gpt-5-thinking, gpt-5-pro, o3"`
	TimeoutMinutes int    `json:"timeout_minutes,omitempty" jsonschema:"Maximum wait in minutes before giving up"`
}

type replyInput struct {
	Prompt         string `json:"prompt" jsonschema:"Follow-up prompt in the current conversation"`
	TimeoutMinutes int    `json:"timeout_minutes,omitempty" jsonschema:"Maximum wait in minutes before giving up"`
}

type uploadInput struct {
	FilePaths      []string `json:"file_paths" jsonschema:"Absolute file paths to upload"`
	Prompt         string   `json:"prompt,omitempty" jsonschema:"Optional prompt to send with the files"`
	TimeoutMinutes int      `json:"timeout_minutes,omitempty" jsonschema:"Maximum wait in minutes before giving up"`
}

func main() {
	cfg := config.Load()
	sess := browser.New(cfg)
	client := chatgpt.New(cfg, sess)

	server := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: serverVersion}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "chatgpt_ask",
		Description: "Send a prompt to ChatGPT and wait for the complete response. Pass model to switch model first (e.g. gpt-5, gpt-5-thinking, gpt-5-pro, o3).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in askInput) (*mcp.CallToolResult, chatgpt.AskResult, error) {
		return nil, *client.Ask(in.Prompt, in.Model, in.TimeoutMinutes), nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "chatgpt_reply",
		Description: "Send a follow-up prompt in the current ChatGPT conversation and wait for the complete response.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in replyInput) (*mcp.CallToolResult, chatgpt.AskResult, error) {
		timeout := time.Duration(in.TimeoutMinutes) * time.Minute
		return nil, *client.Reply(in.Prompt, timeout), nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "chatgpt_new_chat",
		Description: "Start a fresh ChatGPT conversation, resetting the tracked conversation state.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, chatgpt.Simple, error) {
		return nil, *client.NewChat(), nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "chatgpt_upload",
		Description: "Upload files to the current ChatGPT conversation, optionally with a prompt, and wait for the response.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in uploadInput) (*mcp.CallToolResult, chatgpt.AskResult, error) {
		return nil, *client.Upload(in.FilePaths, in.Prompt, in.TimeoutMinutes), nil
	})

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
