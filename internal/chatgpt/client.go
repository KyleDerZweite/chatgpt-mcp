package chatgpt

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"

	"chatgpt-mcp/internal/browser"
	"chatgpt-mcp/internal/config"
)

var (
	convIDRe = regexp.MustCompile(`/c/([0-9a-fA-F-]{8,})`)
	modelRe  = regexp.MustCompile(`^[A-Za-z0-9.-]{1,64}$`)
)

var sendButtonSelectors = []string{
	`[data-testid="send-button"]`,
	`button[aria-label*="Send"]`,
	`button[data-testid="composer-send-button"]`,
}

type AskResult struct {
	Response       string `json:"response" jsonschema:"The full ChatGPT response text"`
	ElapsedSeconds int64  `json:"elapsed_seconds" jsonschema:"total request time in seconds"`
	Model          string `json:"model" jsonschema:"model used for this exchange"`
	ChatID         string `json:"chat_id,omitempty" jsonschema:"conversation id from the URL"`
	PollCount      int    `json:"poll_count" jsonschema:"completion checks performed"`
	Error          string `json:"error,omitempty" jsonschema:"error message if the request failed"`
}

type Simple struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
}

type Client struct {
	cfg     *config.Config
	session *browser.Session
	model   string
	chatID  string
}

func New(cfg *config.Config, session *browser.Session) *Client {
	return &Client{cfg: cfg, session: session}
}

func (c *Client) ensureReady() error {
	if err := c.session.Ensure(); err != nil {
		return err
	}
	if err := c.session.WaitReady(60 * time.Second); err != nil {
		c.session.DebugShot("not-ready")
		return err
	}
	return nil
}

func (c *Client) Ask(prompt, model string, timeout int) *AskResult {
	start := time.Now()
	if err := c.ensureReady(); err != nil {
		return &AskResult{Error: err.Error()}
	}
	if model != "" {
		if !modelRe.MatchString(model) {
			return &AskResult{Error: "invalid model name: " + model}
		}
		if err := c.switchModel(model); err != nil {
			return &AskResult{Error: "model switch: " + err.Error()}
		}
	}
	return c.ask(prompt, time.Duration(timeout)*time.Minute, start)
}

func (c *Client) Reply(prompt string, timeout time.Duration) *AskResult {
	start := time.Now()
	if err := c.ensureReady(); err != nil {
		return &AskResult{Error: err.Error()}
	}
	return c.ask(prompt, timeout, start)
}

func (c *Client) ask(prompt string, timeout time.Duration, start time.Time) *AskResult {
	if timeout <= 0 {
		timeout = time.Duration(c.cfg.DefaultTimeoutMinutes) * time.Minute
	}
	if max := time.Duration(c.cfg.MaxTimeoutMinutes) * time.Minute; timeout > max {
		timeout = max
	}

	if err := c.sendPromptToUI(prompt); err != nil {
		c.session.DebugShot("send-failed")
		return &AskResult{Error: "send: " + err.Error()}
	}
	return c.pollAsk(timeout, start)
}

func (c *Client) pollAsk(timeout time.Duration, start time.Time) *AskResult {
	urlStr, err := c.pageURL()
	if err == nil {
		if m := convIDRe.FindStringSubmatch(urlStr); len(m) == 2 {
			c.chatID = m[1]
		}
	}

	poll := newPoll(timeout)
	for {
		snap, err := c.snapshot()
		if err != nil {
			c.session.DebugShot("snapshot-failed")
			return &AskResult{Error: "snapshot: " + err.Error()}
		}
		if poll.complete(snap) {
			text := snap.response()
			if text == "" {
				text = "(empty response)"
			}
			return &AskResult{
				Response:       text,
				ElapsedSeconds: int64(time.Since(start).Seconds()),
				Model:          c.model,
				ChatID:         c.chatID,
				PollCount:      poll.checks,
			}
		}
		if poll.expired() {
			c.session.DebugShot("timeout")
			return &AskResult{
				Error:          fmt.Sprintf("no response within %s (conversation may still be generating)", timeout),
				ElapsedSeconds: int64(time.Since(start).Seconds()),
				Model:          c.model,
				ChatID:         c.chatID,
				PollCount:      poll.checks,
			}
		}
		time.Sleep(poll.wait())
	}
}

func (c *Client) NewChat() *Simple {
	if err := c.ensureReady(); err != nil {
		return &Simple{Error: err.Error()}
	}
	if err := c.session.Navigate(browser.ChatURL); err != nil {
		return &Simple{Error: err.Error()}
	}
	c.chatID = ""
	return &Simple{Success: true, Message: "new conversation started"}
}

func (c *Client) Upload(paths []string, prompt string, timeout int) *AskResult {
	start := time.Now()
	if len(paths) == 0 {
		return &AskResult{Error: "no files given"}
	}
	if err := c.ensureReady(); err != nil {
		return &AskResult{Error: err.Error()}
	}

	fileInput, err := c.findElement([]string{`input[type="file"]`}, 15*time.Second)
	if err != nil {
		return &AskResult{Error: "file input not found: " + err.Error()}
	}
	if err := fileInput.SetFiles(paths); err != nil {
		return &AskResult{Error: "set files: " + err.Error()}
	}
	time.Sleep(2 * time.Second)

	if prompt != "" {
		if err := c.sendPromptToUI(prompt); err != nil {
			return &AskResult{Error: "send: " + err.Error()}
		}
	} else {
		if err := c.sendPromptToUI(""); err != nil {
			return &AskResult{Error: "send: " + err.Error()}
		}
	}
	return c.pollAsk(time.Duration(timeout)*time.Minute, start)
}

func (c *Client) switchModel(model string) error {
	if err := c.session.Navigate(browser.ModelURLBase + model); err != nil {
		return err
	}
	c.model = model
	return c.session.WaitReady(60 * time.Second)
}

func (c *Client) sendPromptToUI(text string) error {
	el, err := c.findElement(browser.PromptTextareaSelectors, 30*time.Second)
	if err != nil {
		return fmt.Errorf("prompt input not found: %w", err)
	}
	if err := el.Input(text); err != nil {
		return fmt.Errorf("type prompt: %w", err)
	}

	btn, err := c.findElement(sendButtonSelectors, 5*time.Second)
	if err == nil {
		disabled, derr := btn.Disabled()
		if derr == nil && !disabled {
			if err := btn.Click(proto.InputMouseButtonLeft, 1); err == nil {
				return nil
			}
		}
	}
	ka, err := el.KeyActions()
	if err != nil {
		return err
	}
	return ka.Press(input.Enter).Do()
}

func (c *Client) findElement(selectors []string, max time.Duration) (*rod.Element, error) {
	deadline := time.Now().Add(max)
	var lastErr error
	for time.Now().Before(deadline) {
		for _, sel := range selectors {
			el, err := c.session.Page.Element(sel)
			if err != nil {
				lastErr = err
				continue
			}
			visible, verr := el.Visible()
			if verr != nil {
				return el, nil
			}
			if visible {
				return el, nil
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return nil, lastErr
}

func (c *Client) pageURL() (string, error) {
	obj, err := c.session.Page.Eval("function() { return location.href; }")
	if err != nil {
		return "", err
	}
	raw, err := obj.Value.MarshalJSON()
	if err != nil {
		return "", err
	}
	var u string
	if err := json.Unmarshal(raw, &u); err != nil {
		return "", err
	}
	return u, nil
}
