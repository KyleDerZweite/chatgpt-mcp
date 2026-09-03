package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"

	"chatgpt-mcp/internal/browser"
	"chatgpt-mcp/internal/config"
)

var (
	convIDRe = regexp.MustCompile(`/c/([0-9a-fA-F-]{8,})`)
	modelRe  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,62}[A-Za-z0-9])?$`)
)

type AskResult struct {
	Response       string `json:"response" jsonschema:"The complete ChatGPT response as Markdown-compatible text"`
	RawResponse    string `json:"-"`
	ElapsedSeconds int64  `json:"elapsed_seconds" jsonschema:"Total request time in seconds"`
	Model          string `json:"model,omitempty" jsonschema:"Model label verified from the ChatGPT UI, if available"`
	ChatID         string `json:"chat_id,omitempty" jsonschema:"Conversation id from the URL"`
	PollCount      int    `json:"poll_count" jsonschema:"Completion checks performed"`
}

type Simple struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

type Client struct {
	cfg     *config.Config
	session *browser.Session
	model   string
	chatID  string
	dirty   bool
	dirtyBy string
}

type clientState struct {
	model   string
	chatID  string
	dirty   bool
	dirtyBy string
}

func New(cfg *config.Config, session *browser.Session) *Client {
	cleanupStaleUploadSnapshots()
	return &Client{cfg: cfg, session: session}
}

func (c *Client) ensureReady(ctx context.Context) error {
	if err := c.session.EnsureContext(ctx); err != nil {
		return err
	}
	if err := c.session.AssertChatGPTOrigin(ctx); err != nil {
		return err
	}
	if err := c.session.WaitReadyContext(ctx, 60*time.Second); err != nil {
		c.session.DebugShotContext(ctx, "not-ready")
		return err
	}
	return c.session.AssertChatGPTOrigin(ctx)
}

func (c *Client) requireClean() error {
	if !c.dirty {
		return nil
	}
	if c.dirtyBy == "" {
		return fmt.Errorf("browser state is quarantined after an incomplete operation; call chatgpt_new_chat before continuing")
	}
	return fmt.Errorf("browser state is quarantined after an incomplete operation (%s); call chatgpt_new_chat before continuing", c.dirtyBy)
}

func (c *Client) markQuarantined(cause error) {
	c.dirty = true
	c.dirtyBy = cause.Error()
}

// quarantineAfterMutation prevents a timed-out/failed turn or stale upload
// chip from contaminating the next queued transaction. Cleanup is best effort;
// only a successfully loaded new chat clears the quarantine.
func (c *Client) quarantineAfterMutation(cause error) {
	c.markQuarantined(cause)
	recoveryCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if c.session == nil || c.session.Page == nil || c.session.AssertChatGPTOrigin(recoveryCtx) != nil {
		return
	}
	composer, err := resolveComposerOnce(c.session.Page, recoveryCtx, false)
	if err != nil {
		return
	}
	_, _ = composer.Root.Context(recoveryCtx).Eval(cleanupComposerJS)
}

const cleanupComposerJS = `function() {
  const visible = el => {
    if (!el) return false;
    const style = getComputedStyle(el);
    const rect = el.getBoundingClientRect();
    return style.display !== 'none' && style.visibility !== 'hidden' && rect.width > 0 && rect.height > 0;
  };
  const stopSelectors = [
    '[data-testid="stop-button"]',
    '[data-testid="composer-stop-button"]',
    'button[aria-label="Stop generating"]',
    'button[aria-label="Stop response"]'
  ];
  for (const selector of stopSelectors) {
    const button = Array.from(this.querySelectorAll(selector)).find(visible);
    if (button) button.click();
  }
	const composers = Array.from(this.querySelectorAll(
		'#prompt-textarea, [data-testid="prompt-textarea"], textarea[placeholder*="Message"], .ProseMirror[contenteditable="true"], div[contenteditable="true"]'
	)).filter(visible);
	if (composers.length !== 1) return false;
	const composer = composers[0];
	{
    const removeSelectors = [
      '[data-testid*="remove" i][data-testid*="attachment" i]',
      '[data-testid*="remove" i][data-testid*="file" i]',
      'button[aria-label*="Remove file" i]',
      'button[aria-label*="Remove attachment" i]'
    ];
    for (const selector of removeSelectors) {
			for (const button of Array.from(this.querySelectorAll(selector))) button.click();
    }
		for (const input of Array.from(this.querySelectorAll('input[type="file"]'))) {
			input.value = '';
			input.dispatchEvent(new Event('change', {bubbles: true}));
		}
	}
  if (composer) {
    if ('value' in composer) {
      const descriptor = Object.getOwnPropertyDescriptor(Object.getPrototypeOf(composer), 'value');
      if (descriptor && descriptor.set) descriptor.set.call(composer, '');
      else composer.value = '';
    } else {
      composer.replaceChildren();
    }
    composer.dispatchEvent(new InputEvent('input', {bubbles: true, inputType: 'deleteContentBackward'}));
    composer.dispatchEvent(new Event('change', {bubbles: true}));
  }
	return true;
}`

func (c *Client) Ask(ctx context.Context, prompt, model string, timeoutMinutes int) (*AskResult, error) {
	start := time.Now()
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("prompt must not be empty")
	}
	if timeoutMinutes < 0 {
		return nil, fmt.Errorf("timeout_minutes must not be negative")
	}
	timeout := c.timeout(timeoutMinutes)
	operationCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := c.ensureReady(operationCtx); err != nil {
		return nil, err
	}
	if err := c.requireClean(); err != nil {
		return nil, err
	}
	if model == "" {
		if err := c.ensureConversationBinding(operationCtx, false); err != nil {
			c.markQuarantined(err)
			return nil, err
		}
	}
	if model != "" {
		if !modelRe.MatchString(model) {
			return nil, fmt.Errorf("invalid model name %q", model)
		}
		if err := c.switchModel(operationCtx, model); err != nil {
			return nil, fmt.Errorf("model switch: %w", err)
		}
	} else {
		c.refreshModel(operationCtx)
	}
	result, err := c.ask(operationCtx, prompt, start, model)
	if err != nil {
		if model != "" {
			c.markQuarantined(err)
		}
		return nil, err
	}
	if model != "" {
		c.dirty = false
		c.dirtyBy = ""
	}
	return result, nil
}

func (c *Client) Reply(ctx context.Context, prompt string, timeoutMinutes int) (*AskResult, error) {
	start := time.Now()
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("prompt must not be empty")
	}
	if timeoutMinutes < 0 {
		return nil, fmt.Errorf("timeout_minutes must not be negative")
	}
	timeout := c.timeout(timeoutMinutes)
	operationCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := c.ensureReady(operationCtx); err != nil {
		return nil, err
	}
	if err := c.requireClean(); err != nil {
		return nil, err
	}
	if err := c.ensureConversationBinding(operationCtx, true); err != nil {
		c.markQuarantined(err)
		return nil, err
	}
	c.refreshModel(operationCtx)
	return c.ask(operationCtx, prompt, start, "")
}

// Complete handles one stateless provider request. OpenAI-compatible requests
// carry their full transcript, so each call uses a fresh ChatGPT conversation.
// The previously tracked MCP conversation is synchronously restored before the
// shared browser-operation gate is released.
func (c *Client) Complete(ctx context.Context, prompt, model string, timeout time.Duration) (result *AskResult, err error) {
	start := time.Now()
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("prompt must not be empty")
	}
	if model != "" && !modelRe.MatchString(model) {
		return nil, fmt.Errorf("invalid model name %q", model)
	}
	timeout = c.completionTimeout(timeout)
	operationCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	saved := clientState{model: c.model, chatID: c.chatID, dirty: c.dirty, dirtyBy: c.dirtyBy}
	defer func() {
		restoreCtx, restoreCancel := context.WithTimeout(context.Background(), time.Minute)
		restoreErr := c.restoreAfterProvider(restoreCtx, saved)
		restoreCancel()
		if restoreErr == nil {
			return
		}
		c.session.Invalidate()
		c.model = ""
		c.chatID = ""
		wrapped := fmt.Errorf("restore MCP browser state after provider completion: %w", restoreErr)
		c.markQuarantined(wrapped)
		result = nil
		err = errors.Join(err, wrapped)
	}()
	if err := c.session.RecoverContext(operationCtx); err != nil {
		return nil, fmt.Errorf("prepare provider browser session: %w", err)
	}
	if err := c.session.WaitReadyContext(operationCtx, 60*time.Second); err != nil {
		return nil, err
	}

	c.model = ""
	c.chatID = ""
	c.dirty = true
	c.dirtyBy = "provider conversation did not complete"
	if model != "" {
		if err := c.switchModel(operationCtx, model); err != nil {
			return nil, fmt.Errorf("model switch: %w", err)
		}
	} else {
		if err := c.session.NavigateContext(operationCtx, browser.ChatURL); err != nil {
			return nil, err
		}
		if err := c.session.WaitReadyContext(operationCtx, 60*time.Second); err != nil {
			return nil, err
		}
		c.refreshModel(operationCtx)
	}

	result, err = c.ask(operationCtx, prompt, start, model)
	if err != nil {
		return nil, err
	}
	c.dirty = false
	c.dirtyBy = ""
	return result, nil
}

func (c *Client) completionTimeout(requested time.Duration) time.Duration {
	if requested <= 0 {
		return c.timeout(0)
	}
	maximum := c.timeout(c.cfg.MaxTimeoutMinutes)
	if requested > maximum {
		return maximum
	}
	return requested
}

func (c *Client) restoreAfterProvider(ctx context.Context, saved clientState) error {
	if err := c.session.RecoverContext(ctx); err != nil {
		return err
	}
	destination := browser.ChatURL
	if !saved.dirty && saved.chatID != "" {
		destination = conversationURL(saved.chatID)
	}
	if err := c.session.NavigateContext(ctx, destination); err != nil {
		return err
	}
	if err := c.session.WaitReadyContext(ctx, 60*time.Second); err != nil {
		return err
	}
	if err := c.verifyRestoredBrowserState(ctx, saved); err != nil {
		return err
	}
	if !saved.dirty && saved.model != "" {
		actual, err := c.currentModel(ctx)
		if err != nil {
			return fmt.Errorf("verify restored model: %w", err)
		}
		if !modelStateMatches(saved.model, actual) {
			return fmt.Errorf("restored conversation model changed from %q to id=%q label=%q", saved.model, actual.ID, actual.Label)
		}
	}
	c.model = saved.model
	c.chatID = saved.chatID
	c.dirty = saved.dirty
	c.dirtyBy = saved.dirtyBy
	return nil
}

func (c *Client) verifyRestoredBrowserState(ctx context.Context, saved clientState) error {
	if err := c.session.AssertChatGPTOrigin(ctx); err != nil {
		return err
	}
	composer, err := c.findComposer(ctx, 30*time.Second, false)
	if err != nil {
		return fmt.Errorf("verify restored composer: %w", err)
	}
	attachments, err := c.readComposerAttachmentState(ctx, composer)
	if err != nil {
		return fmt.Errorf("inspect restored composer: %w", err)
	}
	if err := requireEmptyComposer(attachments); err != nil {
		return fmt.Errorf("restored composer is not clean: %w", err)
	}
	draft, err := composerText(composer.Prompt.Context(ctx))
	if err != nil {
		return fmt.Errorf("inspect restored prompt draft: %w", err)
	}
	if normalizeComposerText(draft) != "" {
		return fmt.Errorf("restored composer still contains a prompt draft")
	}
	state, err := c.snapshotState(ctx)
	if err != nil {
		return fmt.Errorf("inspect restored conversation: %w", err)
	}
	urlString, err := c.pageURL(ctx)
	if err != nil {
		return err
	}
	return validateRestoredConversation(saved, urlString, state)
}

func validateRestoredConversation(saved clientState, urlString string, state snapshot) error {
	if state.IsGenerating {
		return fmt.Errorf("restored conversation still reports an active response")
	}
	if !saved.dirty && saved.chatID != "" {
		match := convIDRe.FindStringSubmatch(urlString)
		if len(match) != 2 || match[1] != saved.chatID {
			return fmt.Errorf("restored conversation is %q, want %q", urlString, saved.chatID)
		}
		return nil
	}
	if convIDRe.MatchString(urlString) || state.TurnCount != 0 || state.AssistantCount != 0 {
		return fmt.Errorf("restored new-chat state is not empty (url=%q turns=%d assistants=%d)", urlString, state.TurnCount, state.AssistantCount)
	}
	return nil
}

func conversationURL(chatID string) string {
	return browser.ChatURL + "/c/" + url.PathEscape(chatID)
}

func (c *Client) ask(ctx context.Context, prompt string, start time.Time, expectedModel string) (*AskResult, error) {
	composer, err := c.findComposer(ctx, 30*time.Second, false)
	if err != nil {
		return nil, fmt.Errorf("resolve composer: %w", err)
	}
	attachments, err := c.readComposerAttachmentState(ctx, composer)
	if err != nil {
		return nil, fmt.Errorf("inspect composer attachments: %w", err)
	}
	if err := requireEmptyComposer(attachments); err != nil {
		c.markQuarantined(err)
		return nil, err
	}
	before, err := c.snapshotState(ctx)
	if err != nil {
		return nil, fmt.Errorf("capture pre-send state: %w", err)
	}
	if before.IsGenerating {
		err := fmt.Errorf("ChatGPT already has a response in progress; call chatgpt_new_chat before continuing")
		c.markQuarantined(err)
		return nil, err
	}
	transaction, err := c.newConversationTransaction(ctx, prompt, nil)
	if err != nil {
		return nil, err
	}
	defer transaction.close()
	if err := c.session.AssertChatGPTOrigin(ctx); err != nil {
		return nil, err
	}
	if err := c.sendPromptToUI(ctx, transaction, composer, prompt, nil); err != nil {
		c.session.DebugShotContext(ctx, "send-failed")
		err = fmt.Errorf("send: %w", err)
		c.quarantineAfterMutation(err)
		return nil, err
	}
	result, err := c.pollAsk(ctx, start, before.marker(), expectedModel, transaction)
	if err != nil {
		c.quarantineAfterMutation(err)
		return nil, err
	}
	return result, nil
}

func (c *Client) pollAsk(ctx context.Context, start time.Time, before turnMarker, expectedModel string, transaction *conversationTransaction) (*AskResult, error) {
	poll := newPoll(before)
	for {
		if err := transaction.verify(c, ctx, true); err != nil {
			if errors.Is(err, errConversationTransitionPending) {
				if err := sleepContext(ctx, 100*time.Millisecond); err != nil {
					return nil, err
				}
				continue
			}
			return nil, err
		}
		if err := c.session.AssertChatGPTOrigin(ctx); err != nil {
			return nil, err
		}
		state, err := c.snapshotState(ctx)
		if err != nil {
			c.session.DebugShotContext(ctx, "snapshot-failed")
			return nil, fmt.Errorf("snapshot: %w", err)
		}
		if poll.complete(state) {
			if err := transaction.verify(c, ctx, true); err != nil {
				return nil, err
			}
			verified, err := c.snapshotState(ctx)
			if err != nil {
				return nil, fmt.Errorf("verify completed response: %w", err)
			}
			if !sameCompletedSnapshot(state, verified) {
				poll.invalidate()
				continue
			}
			if err := transaction.verify(c, ctx, true); err != nil {
				return nil, err
			}
			answer, err := c.snapshot(ctx)
			if err != nil {
				return nil, fmt.Errorf("extract completed response: %w", err)
			}
			if answer.marker().key() != verified.marker().key() || answer.ContentVersion != verified.ContentVersion {
				poll.invalidate()
				continue
			}
			if err := transaction.verify(c, ctx, true); err != nil {
				return nil, err
			}
			confirmed, err := c.snapshotState(ctx)
			if err != nil {
				return nil, fmt.Errorf("recheck completed response: %w", err)
			}
			if !sameCompletedSnapshot(verified, confirmed) {
				poll.invalidate()
				continue
			}
			text := answer.response()
			if text == "" {
				return nil, fmt.Errorf("ChatGPT completed a new assistant turn with no extractable response")
			}
			if err := c.verifyResultModel(ctx, expectedModel); err != nil {
				return nil, err
			}
			c.chatID = transaction.id
			return &AskResult{
				Response:       text,
				RawResponse:    strings.TrimSpace(answer.ResponseText),
				ElapsedSeconds: int64(time.Since(start).Seconds()),
				Model:          c.model,
				ChatID:         c.chatID,
				PollCount:      poll.checks,
			}, nil
		}

		wait := poll.wait()
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			if ctx.Err() == context.DeadlineExceeded {
				elapsed := time.Since(start).Round(time.Millisecond)
				return nil, fmt.Errorf("no new completed assistant response after %s: %w", elapsed, ctx.Err())
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Client) NewChat(ctx context.Context) (*Simple, error) {
	operationCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	// Recovery must not require the current UI to have a usable composer: a
	// failed upload, modal, or closed tab is precisely why NewChat may be needed.
	if err := c.session.RecoverContext(operationCtx); err != nil {
		return nil, err
	}
	if err := c.session.AssertChatGPTOrigin(operationCtx); err != nil {
		return nil, err
	}
	// Treat navigation as a recovery boundary. Do not clear quarantine until a
	// fresh composer on the trusted origin is confirmed ready.
	c.dirty = true
	c.dirtyBy = "new-chat navigation did not complete"
	if err := c.session.NavigateContext(operationCtx, browser.ChatURL); err != nil {
		return nil, err
	}
	if err := c.session.WaitReadyContext(operationCtx, 60*time.Second); err != nil {
		return nil, err
	}
	if err := c.session.AssertChatGPTOrigin(operationCtx); err != nil {
		return nil, err
	}
	composer, err := c.findComposer(operationCtx, 30*time.Second, false)
	if err != nil {
		return nil, fmt.Errorf("verify new-chat composer: %w", err)
	}
	attachments, err := c.readComposerAttachmentState(operationCtx, composer)
	if err != nil {
		return nil, fmt.Errorf("inspect new-chat composer: %w", err)
	}
	if err := requireEmptyComposer(attachments); err != nil {
		return nil, fmt.Errorf("new chat did not establish a clean composer: %w", err)
	}
	state, err := c.snapshotState(operationCtx)
	if err != nil {
		return nil, fmt.Errorf("verify new-chat state: %w", err)
	}
	if state.IsGenerating {
		return nil, fmt.Errorf("new chat still reports an active response")
	}
	urlString, err := c.pageURL(operationCtx)
	if err != nil {
		return nil, fmt.Errorf("verify new-chat URL: %w", err)
	}
	if convIDRe.MatchString(urlString) || state.TurnCount != 0 || state.AssistantCount != 0 {
		return nil, fmt.Errorf("new chat did not establish an empty conversation (url=%q turns=%d assistants=%d)", urlString, state.TurnCount, state.AssistantCount)
	}
	c.chatID = ""
	c.model = ""
	c.dirty = false
	c.dirtyBy = ""
	c.refreshModel(operationCtx)
	return &Simple{Success: true, Message: "new conversation started"}, nil
}

func (c *Client) Upload(ctx context.Context, paths []string, prompt string, timeoutMinutes int) (*AskResult, error) {
	start := time.Now()
	if timeoutMinutes < 0 {
		return nil, fmt.Errorf("timeout_minutes must not be negative")
	}
	timeout := c.timeout(timeoutMinutes)
	operationCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	staged, displayNames, cleanup, err := c.prepareUploadPaths(operationCtx, paths)
	if err != nil {
		return nil, err
	}
	var fileInput *rod.Element
	defer func() {
		if fileInput != nil {
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 2*time.Second)
			if c.session.AssertChatGPTOrigin(releaseCtx) == nil {
				_ = fileInput.Context(releaseCtx).SetFiles(nil)
			}
			releaseCancel()
		}
		cleanup()
	}()
	if err := c.ensureReady(operationCtx); err != nil {
		return nil, err
	}
	if err := c.requireClean(); err != nil {
		return nil, err
	}
	if err := c.ensureConversationBinding(operationCtx, false); err != nil {
		c.markQuarantined(err)
		return nil, err
	}
	c.refreshModel(operationCtx)

	composer, err := c.findComposer(operationCtx, 30*time.Second, true)
	if err != nil {
		return nil, fmt.Errorf("resolve upload composer: %w", err)
	}
	baseline, err := c.readComposerAttachmentState(operationCtx, composer)
	if err != nil {
		return nil, fmt.Errorf("inspect upload baseline: %w", err)
	}
	if err := requireEmptyComposer(baseline); err != nil {
		c.markQuarantined(err)
		return nil, err
	}
	before, err := c.snapshotState(operationCtx)
	if err != nil {
		return nil, fmt.Errorf("capture pre-send state: %w", err)
	}
	if before.IsGenerating {
		err := fmt.Errorf("ChatGPT already has a response in progress; call chatgpt_new_chat before continuing")
		c.markQuarantined(err)
		return nil, err
	}
	if err := c.session.AssertChatGPTOrigin(operationCtx); err != nil {
		return nil, err
	}
	transaction, err := c.newConversationTransaction(operationCtx, prompt, displayNames)
	if err != nil {
		return nil, err
	}
	defer transaction.close()
	if err := transaction.verify(c, operationCtx, false); err != nil {
		return nil, err
	}
	fileInput = composer.FileInput
	if err := c.session.AssertChatGPTOrigin(operationCtx); err != nil {
		return nil, err
	}
	if err := fileInput.SetFiles(staged); err != nil {
		err = fmt.Errorf("set files: %w", err)
		c.quarantineAfterMutation(err)
		return nil, err
	}
	if err := c.waitForUploadsReady(operationCtx, composer, displayNames); err != nil {
		c.quarantineAfterMutation(err)
		return nil, err
	}
	if err := c.session.AssertChatGPTOrigin(operationCtx); err != nil {
		c.quarantineAfterMutation(err)
		return nil, err
	}
	if err := c.sendPromptToUI(operationCtx, transaction, composer, prompt, displayNames); err != nil {
		err = fmt.Errorf("send: %w", err)
		c.quarantineAfterMutation(err)
		return nil, err
	}
	result, err := c.pollAsk(operationCtx, start, before.marker(), "", transaction)
	if err != nil {
		c.quarantineAfterMutation(err)
		return nil, err
	}
	return result, nil
}

func (c *Client) timeout(minutes int) time.Duration {
	defaultMinutes := c.cfg.DefaultTimeoutMinutes
	if defaultMinutes <= 0 {
		defaultMinutes = 30
	}
	maxMinutes := c.cfg.MaxTimeoutMinutes
	if maxMinutes <= 0 {
		maxMinutes = 120
	}
	if defaultMinutes > maxMinutes {
		defaultMinutes = maxMinutes
	}
	if minutes <= 0 {
		minutes = defaultMinutes
	}
	if minutes > maxMinutes {
		minutes = maxMinutes
	}
	const maxDurationMinutes = int64((1<<63 - 1) / int64(time.Minute))
	if int64(minutes) > maxDurationMinutes {
		minutes = int(maxDurationMinutes)
	}
	return time.Duration(minutes) * time.Minute
}

func (c *Client) switchModel(ctx context.Context, requested string) error {
	if err := c.session.AssertChatGPTOrigin(ctx); err != nil {
		return err
	}
	c.model = ""
	c.chatID = ""
	// Navigation can create a new conversation or silently select a fallback.
	// Quarantine that state until the requested model has been observed
	// consistently; callers can recover with NewChat if verification fails.
	c.dirty = true
	c.dirtyBy = fmt.Sprintf("model switch to %q did not complete", requested)
	modelURL := browser.ModelURLBase + url.QueryEscape(requested)
	if err := c.session.NavigateContext(ctx, modelURL); err != nil {
		return err
	}
	if err := c.session.AssertChatGPTOrigin(ctx); err != nil {
		return err
	}
	if err := c.session.WaitReadyContext(ctx, 60*time.Second); err != nil {
		return err
	}
	verifyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var last modelState
	var lastErr error
	matchedKey := ""
	var matchedSince time.Time
	for {
		state, err := c.currentModel(verifyCtx)
		if err == nil {
			last = state
			if modelStateMatches(requested, state) {
				key := strings.ToLower(state.ID + "\x00" + state.Label)
				if key == matchedKey {
					if time.Since(matchedSince) >= 2*time.Second {
						c.model = state.display()
						c.dirtyBy = fmt.Sprintf("request after model switch to %q did not complete", requested)
						return nil
					}
				} else {
					matchedKey = key
					matchedSince = time.Now()
				}
			} else {
				matchedKey = ""
				matchedSince = time.Time{}
			}
		} else {
			lastErr = err
			matchedKey = ""
			matchedSince = time.Time{}
		}
		if err := sleepContext(verifyCtx, 250*time.Millisecond); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if last.ID != "" || last.Label != "" {
				return fmt.Errorf("requested %q but the ChatGPT UI reports id=%q label=%q (the account may have fallen back)", requested, last.ID, last.Label)
			}
			if lastErr == nil {
				return fmt.Errorf("cannot verify selected model: no selected-model indicator appeared")
			}
			return fmt.Errorf("cannot verify selected model: %w", lastErr)
		}
	}
}

func (c *Client) refreshModel(ctx context.Context) {
	actual, err := c.currentModel(ctx)
	if err == nil {
		c.model = actual.display()
	} else {
		c.model = ""
	}
}

func (c *Client) verifyResultModel(ctx context.Context, expected string) error {
	actual, err := c.currentModel(ctx)
	if err != nil {
		c.model = ""
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if expected != "" {
			return fmt.Errorf("verify model after response: %w", err)
		}
		return nil
	}
	if expected != "" && !modelStateMatches(expected, actual) {
		c.model = ""
		return fmt.Errorf("requested model %q changed or fell back during the response; UI reports id=%q label=%q", expected, actual.ID, actual.Label)
	}
	c.model = actual.display()
	return nil
}

const currentModelJS = `function() {
	if (location.protocol !== 'https:' || location.hostname !== 'chatgpt.com' || location.port !== '' || location.origin !== 'https://chatgpt.com') {
		throw new Error('untrusted browser origin');
	}
  const visible = el => {
    if (!el || !el.isConnected || el.closest('[hidden], [aria-hidden="true"]')) return false;
    const style = getComputedStyle(el);
    return style.display !== 'none' && style.visibility !== 'hidden' && el.getClientRects().length > 0;
  };
  const selectors = [
    '[data-testid="model-switcher-dropdown-button"]',
    '[data-testid="model-selector"]',
    'button[aria-label*="current model" i]',
    'button[aria-label*="model selector" i]',
    '[data-model][aria-selected="true"]',
    '[data-model][aria-pressed="true"]'
  ];
  let candidates = Array.from(new Set(selectors.flatMap(selector =>
    Array.from(document.querySelectorAll(selector))
  ))).filter(visible);
  candidates = candidates.filter(candidate => !candidates.some(other =>
    other !== candidate && other.contains(candidate)
  ));
  if (candidates.length !== 1) {
    return {id: '', label: '', ambiguous: candidates.length > 1, controlCount: candidates.length};
  }
  const control = candidates[0];
	const selectedCandidates = Array.from(control.querySelectorAll(
	  '[data-model][aria-selected="true"], [data-model][aria-pressed="true"]'
	)).filter(visible);
	if (selectedCandidates.length > 1) {
	  return {id: '', label: '', ambiguous: true, controlCount: selectedCandidates.length};
	}
	const selected = selectedCandidates.length === 1 ? selectedCandidates[0] : null;
	const id = ((control.getAttribute('data-model') || (selected && selected.getAttribute('data-model'))) || '').trim();
	const labels = [];
	const addLabel = value => {
		value = (value || '').trim();
		if (value && !labels.includes(value)) labels.push(value);
	};
	addLabel(control.getAttribute('aria-label'));
	addLabel(control.getAttribute('title'));
	if (selected) {
		addLabel(selected.getAttribute('aria-label'));
		addLabel(selected.getAttribute('title'));
		addLabel(selected.innerText);
	}
	addLabel(control.innerText);
	return {id, label: labels[0] || '', labels, ambiguous: false, controlCount: 1};
}`

type modelState struct {
	ID           string   `json:"id"`
	Label        string   `json:"label"`
	Labels       []string `json:"labels"`
	Ambiguous    bool     `json:"ambiguous"`
	ControlCount int      `json:"controlCount"`
}

func (s modelState) display() string {
	if strings.TrimSpace(s.ID) != "" {
		return strings.TrimSpace(s.ID)
	}
	return strings.TrimSpace(s.Label)
}

func (c *Client) currentModel(ctx context.Context) (modelState, error) {
	if err := c.session.AssertChatGPTOrigin(ctx); err != nil {
		return modelState{}, err
	}
	obj, err := c.session.Page.Context(ctx).Eval(currentModelJS)
	if err != nil {
		return modelState{}, err
	}
	if err := c.session.AssertChatGPTOrigin(ctx); err != nil {
		return modelState{}, err
	}
	raw, err := obj.Value.MarshalJSON()
	if err != nil {
		return modelState{}, err
	}
	var state modelState
	if err := json.Unmarshal(raw, &state); err != nil {
		return modelState{}, err
	}
	state.ID = strings.TrimSpace(state.ID)
	state.Label = strings.TrimSpace(state.Label)
	labels := make([]string, 0, len(state.Labels)+1)
	seen := make(map[string]struct{}, len(state.Labels)+1)
	for _, label := range append([]string{state.Label}, state.Labels...) {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		key := strings.ToLower(label)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		labels = append(labels, label)
	}
	state.Labels = labels
	if state.Label == "" && len(labels) > 0 {
		state.Label = labels[0]
	}
	if state.Ambiguous {
		return modelState{}, fmt.Errorf("multiple visible current-model controls were found")
	}
	if state.ID == "" && state.Label == "" {
		return modelState{}, fmt.Errorf("no unique visible selected-model id or label was found")
	}
	return state, nil
}

func modelStateMatches(requested string, actual modelState) bool {
	if actual.ID != "" {
		return strings.EqualFold(strings.TrimSpace(requested), strings.TrimSpace(actual.ID))
	}
	labels := actual.Labels
	if len(labels) == 0 && actual.Label != "" {
		labels = []string{actual.Label}
	}
	for _, label := range labels {
		if modelMatches(requested, label) {
			return true
		}
	}
	return false
}

func modelMatches(requested, actual string) bool {
	requestedTokens := modelTokens(requested)
	actualTokens := modelTokens(actual)
	return len(requestedTokens) > 0 && len(actualTokens) > 0 && strings.Join(requestedTokens, "|") == strings.Join(actualTokens, "|")
}

func modelTokens(value string) []string {
	value = strings.ToLower(strings.ReplaceAll(value, "chatgpt", "gpt"))
	fields := strings.FieldsFunc(value, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	ignored := map[string]bool{
		"button": true, "current": true, "dropdown": true, "model": true,
		"selected": true, "selector": true, "switch": true, "switcher": true,
		"is": true, "the": true,
	}
	out := fields[:0]
	for _, field := range fields {
		if !ignored[field] {
			out = append(out, field)
		}
	}
	return out
}

func (c *Client) sendPromptToUI(ctx context.Context, transaction *conversationTransaction, composer *composerElements, text string, expectedAttachments []string) error {
	if err := transaction.verify(c, ctx, false); err != nil {
		return err
	}
	if err := c.session.AssertChatGPTOrigin(ctx); err != nil {
		return err
	}
	if err := c.verifyComposerAttachments(ctx, composer, expectedAttachments); err != nil {
		return err
	}
	if err := replaceComposerText(composer.Prompt.Context(ctx), text); err != nil {
		return err
	}
	if err := c.verifyComposerAttachments(ctx, composer, expectedAttachments); err != nil {
		return err
	}

	buttonCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		if err := transaction.verify(c, buttonCtx, false); err != nil {
			return err
		}
		if err := c.session.AssertChatGPTOrigin(buttonCtx); err != nil {
			return err
		}
		if err := c.verifyComposerAttachments(buttonCtx, composer, expectedAttachments); err != nil {
			return err
		}
		button, err := findComposerSendOnce(buttonCtx, composer)
		if errors.Is(err, errComposerUnavailable) {
			if err := sleepContext(buttonCtx, 100*time.Millisecond); err != nil {
				return fmt.Errorf("composer send control did not appear: %w", err)
			}
			continue
		}
		if err != nil {
			return err
		}
		ready, err := composerSendReady(button)
		if err != nil {
			return fmt.Errorf("inspect composer send control: %w", err)
		}
		if ready {
			if err := transaction.verify(c, buttonCtx, false); err != nil {
				return err
			}
			if err := c.session.AssertChatGPTOrigin(buttonCtx); err != nil {
				return err
			}
			if err := transaction.armSubmissionObserver(c, buttonCtx, ctx); err != nil {
				return err
			}
			if err := transaction.beginSubmission(); err != nil {
				return err
			}
			return c.clickCurrentComposerSend(buttonCtx, text, expectedAttachments)
		}
		if err := sleepContext(buttonCtx, 100*time.Millisecond); err != nil {
			return fmt.Errorf("composer send control did not become ready: %w", err)
		}
	}
}

// clickCurrentComposerSend re-resolves the composer and its send control after
// submission observation is armed. The live page may replace the button as it
// changes from voice mode to send mode; retaining the earlier element handle
// would then fail even though exactly one valid send control is visible.
func (c *Client) clickCurrentComposerSend(ctx context.Context, expectedText string, expectedAttachments []string) error {
	for {
		if err := c.session.AssertChatGPTOrigin(ctx); err != nil {
			return err
		}
		composer, err := resolveComposerOnce(c.session.Page, ctx, false)
		if errors.Is(err, errComposerUnavailable) {
			if err := sleepContext(ctx, 50*time.Millisecond); err != nil {
				return fmt.Errorf("composer disappeared before submission: %w", err)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("re-resolve composer before submission: %w", err)
		}
		actual, err := composerText(composer.Prompt.Context(ctx))
		if err != nil {
			return fmt.Errorf("verify prompt immediately before submission: %w", err)
		}
		if normalizeComposerText(actual) != normalizeComposerText(expectedText) {
			return fmt.Errorf("prompt composer changed before submission")
		}
		if err := c.verifyComposerAttachments(ctx, composer, expectedAttachments); err != nil {
			return err
		}
		button, err := findComposerSendOnce(ctx, composer)
		if errors.Is(err, errComposerUnavailable) {
			if err := sleepContext(ctx, 50*time.Millisecond); err != nil {
				return fmt.Errorf("composer send control disappeared before submission: %w", err)
			}
			continue
		}
		if err != nil {
			return err
		}
		ready, err := composerSendReady(button)
		if err != nil {
			var notFound *rod.ElementNotFoundError
			if errors.As(err, &notFound) {
				continue
			}
			return fmt.Errorf("inspect current composer send control: %w", err)
		}
		if !ready {
			if err := sleepContext(ctx, 50*time.Millisecond); err != nil {
				return fmt.Errorf("current composer send control did not become ready: %w", err)
			}
			continue
		}
		// ChatGPT replaces the voice/send button while the composer state is
		// settling, which can invalidate even a just-resolved element before
		// Rod finishes its hover-and-click sequence. Once the unique send button
		// is verified ready, activate the exact verified prompt with a physical
		// Enter key instead; multiline content was inserted directly and does not
		// depend on Enter for its line breaks.
		keys, err := composer.Prompt.Context(ctx).KeyActions()
		if err != nil {
			var notFound *rod.ElementNotFoundError
			if errors.As(err, &notFound) {
				continue
			}
			return fmt.Errorf("focus composer for submission: %w", err)
		}
		if err := keys.Press(input.Enter).Do(); err != nil {
			// A keyboard transport error can occur after keydown, so never retry:
			// delivery is ambiguous and a second Enter could duplicate the prompt.
			return fmt.Errorf("submit composer with Enter: %w", err)
		}
		return nil
	}
}

func (c *Client) verifyComposerAttachments(ctx context.Context, composer *composerElements, expected []string) error {
	state, err := c.readComposerAttachmentState(ctx, composer)
	if err != nil {
		return fmt.Errorf("inspect composer attachments: %w", err)
	}
	if expected == nil {
		return requireEmptyComposer(state)
	}
	return matchExpectedAttachments(state, expected)
}

func composerSendReady(button *rod.Element) (bool, error) {
	disabled, err := button.Disabled()
	if err != nil {
		return false, err
	}
	ariaDisabled, err := button.Attribute("aria-disabled")
	if err != nil {
		return false, err
	}
	return !disabled && (ariaDisabled == nil || !strings.EqualFold(*ariaDisabled, "true")), nil
}

const selectComposerContentsJS = `function() {
  this.focus();
  if (typeof this.select === 'function') {
    this.select();
    return true;
  }
  if (this.isContentEditable || this.getAttribute('contenteditable') === 'true') {
    const range = document.createRange();
    range.selectNodeContents(this);
    const selection = window.getSelection();
    selection.removeAllRanges();
    selection.addRange(range);
    return true;
  }
  return false;
}`

func replaceComposerText(element *rod.Element, text string) error {
	selected, err := element.Eval(selectComposerContentsJS)
	if err != nil {
		return fmt.Errorf("select existing prompt: %w", err)
	}
	if !selected.Value.Bool() {
		return fmt.Errorf("prompt input is neither a text control nor contenteditable")
	}
	keys, err := element.KeyActions()
	if err != nil {
		return fmt.Errorf("focus prompt input: %w", err)
	}
	if err := keys.Press(input.Backspace).Do(); err != nil {
		return fmt.Errorf("clear existing prompt: %w", err)
	}
	cleared, err := composerText(element)
	if err != nil {
		return fmt.Errorf("verify cleared prompt: %w", err)
	}
	if normalizeComposerText(cleared) != "" {
		return fmt.Errorf("could not clear an existing prompt draft")
	}
	if err := element.Input(text); err != nil {
		return fmt.Errorf("type prompt: %w", err)
	}
	actual, err := composerText(element)
	if err != nil {
		return fmt.Errorf("verify prompt: %w", err)
	}
	if normalizeComposerText(actual) != normalizeComposerText(text) {
		return fmt.Errorf("prompt composer content did not match the requested prompt")
	}
	return nil
}

// composerText reads editor content structurally instead of relying on
// innerText. Chromium inserts an extra layout newline between ProseMirror
// paragraphs when computing innerText, so a prompt containing one LF can be
// reported with two. Joining explicit block nodes ourselves preserves blank
// paragraphs while remaining independent of their CSS margins.
const composerTextJS = `function() {
  if ('value' in this && typeof this.value === 'string') return this.value;

  const blockTags = new Set([
    'ADDRESS', 'ARTICLE', 'ASIDE', 'BLOCKQUOTE', 'DD', 'DIV', 'DL', 'DT',
    'FIGCAPTION', 'FIGURE', 'FOOTER', 'H1', 'H2', 'H3', 'H4', 'H5', 'H6',
    'HEADER', 'HGROUP', 'LI', 'MAIN', 'NAV', 'OL', 'P', 'PRE', 'SECTION',
    'TABLE', 'TBODY', 'TD', 'TFOOT', 'TH', 'THEAD', 'TR', 'UL'
  ]);
  const isBlock = node => node && node.nodeType === Node.ELEMENT_NODE && blockTags.has(node.tagName);
  const isPlaceholderBreak = node => {
    if (!node.classList || !node.classList.contains('ProseMirror-trailingBreak')) {
      const parent = node.parentElement;
      if (!parent || !isBlock(parent)) return false;
      return Array.from(parent.childNodes).every(sibling => {
        if (sibling === node) return true;
        if (sibling.nodeType === Node.TEXT_NODE) {
          return (sibling.nodeValue || '').replace(/\u200b/g, '').trim() === '';
        }
        return sibling.nodeType === Node.ELEMENT_NODE && sibling.tagName === 'BR';
      });
    }
    return true;
  };
  const read = node => {
    if (node.nodeType === Node.TEXT_NODE) return node.nodeValue || '';
    if (node.nodeType !== Node.ELEMENT_NODE) return '';
    if (node.tagName === 'BR') return isPlaceholderBreak(node) ? '' : '\n';

    const blocks = [];
    let inline = '';
    for (const child of node.childNodes) {
      if (isBlock(child)) {
        if (inline !== '' && !(blocks.length > 0 && /^\s+$/.test(inline))) blocks.push(inline);
        inline = '';
        blocks.push(read(child));
      } else {
        inline += read(child);
      }
    }
    if (inline !== '' && !(blocks.length > 0 && /^\s+$/.test(inline))) blocks.push(inline);
    if (blocks.length === 0) return inline;
    return blocks.join('\n');
  };
  return read(this);
}`

func composerText(element *rod.Element) (string, error) {
	value, err := element.Eval(composerTextJS)
	if err != nil {
		return "", err
	}
	return value.Value.Str(), nil
}

func normalizeComposerText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	// ProseMirror converts leading and repeated ordinary spaces to NBSP text
	// nodes so the browser preserves indentation. Treat that representation as
	// the ordinary spaces that were requested when verifying the composed text.
	value = strings.ReplaceAll(value, "\u00a0", " ")
	return strings.TrimSpace(value)
}

func (c *Client) waitForUploadsReady(ctx context.Context, composer *composerElements, expectedNames []string) error {
	readyConsecutive := 0
	var lastErr error
	for {
		if err := c.session.AssertChatGPTOrigin(ctx); err != nil {
			return err
		}
		state, err := c.readComposerAttachmentState(ctx, composer)
		if err != nil {
			return fmt.Errorf("inspect upload state: %w", err)
		}
		if state.Error != "" {
			return fmt.Errorf("ChatGPT reported an upload error: %s", state.Error)
		}
		matchErr := matchExpectedAttachments(state, expectedNames)
		button, buttonErr := findComposerSendOnce(ctx, composer)
		ready := false
		if buttonErr == nil {
			ready, buttonErr = composerSendReady(button)
		}
		if buttonErr != nil && !errors.Is(buttonErr, errComposerUnavailable) {
			return fmt.Errorf("inspect upload send control: %w", buttonErr)
		}
		if matchErr == nil && ready {
			readyConsecutive++
			if readyConsecutive >= 2 {
				return nil
			}
		} else {
			readyConsecutive = 0
			if matchErr != nil {
				lastErr = matchErr
			} else {
				lastErr = fmt.Errorf("composer send control is not ready")
			}
		}
		if err := sleepContext(ctx, 250*time.Millisecond); err != nil {
			return fmt.Errorf("file upload did not become ready (%v): %w", lastErr, err)
		}
	}
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Client) ensureConversationBinding(ctx context.Context, requireExisting bool) error {
	urlString, err := c.pageURL(ctx)
	if err != nil {
		return err
	}
	current := ""
	if match := convIDRe.FindStringSubmatch(urlString); len(match) == 2 {
		current = match[1]
	}
	if c.chatID == "" {
		if current == "" && requireExisting {
			return fmt.Errorf("no existing ChatGPT conversation is tracked; use chatgpt_ask first")
		}
		c.chatID = current
		return nil
	}
	if current != c.chatID {
		return fmt.Errorf("browser conversation changed from %q to %q; call chatgpt_new_chat before continuing", c.chatID, current)
	}
	return nil
}

type conversationTransaction struct {
	id                string
	prompt            string
	attachments       []string
	beforeUserCount   int
	requiresNew       bool
	targetID          proto.TargetTargetID
	observer          *submissionObserver
	observedMessageID string
}

func (transaction *conversationTransaction) verify(c *Client, ctx context.Context, allowNewConversation bool) error {
	if c.session == nil || c.session.Page == nil || c.session.Page.TargetID != transaction.targetID {
		return fmt.Errorf("browser target changed during the operation")
	}
	urlString, err := c.pageURL(ctx)
	if err != nil {
		return err
	}
	current := ""
	if match := convIDRe.FindStringSubmatch(urlString); len(match) == 2 {
		current = match[1]
	}
	if transaction.id != "" {
		if current != transaction.id {
			return fmt.Errorf("browser conversation changed during the operation from %q to %q", transaction.id, current)
		}
		return nil
	}
	if current == "" {
		if transaction.requiresNew && allowNewConversation {
			return errConversationTransitionPending
		}
		return nil
	}
	if !allowNewConversation {
		return fmt.Errorf("browser navigated to conversation %q before the operation created a new chat", current)
	}
	if err := transaction.verifyTransitionEvidence(c, ctx); err != nil {
		return err
	}
	transaction.id = current
	return nil
}

func (c *Client) pageURL(ctx context.Context) (string, error) {
	if err := c.session.AssertChatGPTOrigin(ctx); err != nil {
		return "", err
	}
	obj, err := c.session.Page.Context(ctx).Eval(`function() {
		if (location.protocol !== 'https:' || location.hostname !== 'chatgpt.com' || location.port !== '' || location.origin !== 'https://chatgpt.com') {
			throw new Error('untrusted browser origin');
		}
		return location.href;
	}`)
	if err != nil {
		return "", err
	}
	if err := c.session.AssertChatGPTOrigin(ctx); err != nil {
		return "", err
	}
	raw, err := obj.Value.MarshalJSON()
	if err != nil {
		return "", err
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return value, nil
}
