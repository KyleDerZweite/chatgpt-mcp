package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-rod/rod/lib/proto"

	"chatgpt-mcp/internal/browser"
)

var errConversationTransitionPending = errors.New("new conversation route appeared before the submitted user turn")

const submissionStartTimeout = 5 * time.Second

type submittedTurnState struct {
	UserCount int        `json:"userCount"`
	MessageID string     `json:"messageId"`
	Text      string     `json:"text"`
	Items     [][]string `json:"items"`
}

type submissionObservation struct {
	messageID string
	err       error
}

type submissionObserver struct {
	result    chan submissionObservation
	done      chan struct{}
	cancel    context.CancelFunc
	accepting atomic.Bool
}

var submittedMessageIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)

const submittedTurnStateJS = `function() {
  if (location.protocol !== 'https:' || location.hostname !== 'chatgpt.com' || location.port !== '' || location.origin !== 'https://chatgpt.com') {
    throw new Error('untrusted browser origin');
  }
  const visible = el => {
    if (!el || !el.isConnected || el.closest('[hidden], [aria-hidden="true"]')) return false;
    const style = getComputedStyle(el);
    return style.display !== 'none' && style.visibility !== 'hidden' && el.getClientRects().length > 0;
  };
  const prompts = Array.from(document.querySelectorAll(
    '#prompt-textarea, [data-testid="prompt-textarea"], form textarea[placeholder*="Message"], ' +
		'form .ProseMirror[contenteditable="true"], form div[contenteditable="true"], [data-testid*="composer" i] textarea[placeholder*="Message"], ' +
		'[data-testid*="composer" i] .ProseMirror[contenteditable="true"], [data-testid*="composer" i] div[contenteditable="true"]'
  )).filter(visible);
  const eligible = (prompt, root) => prompt.matches('#prompt-textarea, [data-testid="prompt-textarea"]') ||
    root.matches('[data-testid*="composer" i]') || !!root.querySelector(
      'input[type="file"], [data-testid*="composer" i], button[aria-label*="attach" i], ' +
      'button[aria-label*="upload" i], button[aria-label*="voice" i], [data-testid*="send" i]'
    );
  const composerRoots = [];
  for (const prompt of prompts) {
    const root = prompt.closest('form') || prompt.closest('[data-testid*="composer" i]');
    if (root && eligible(prompt, root) && !composerRoots.includes(root)) composerRoots.push(root);
  }
  if (composerRoots.length !== 1) throw new Error('ambiguous ChatGPT composer while verifying submitted turn');
  const conversation = composerRoots[0].closest('main, [role="main"]');
  if (!conversation || !visible(conversation)) throw new Error('active ChatGPT conversation container not found');

  const turns = Array.from(conversation.querySelectorAll('[data-testid^="conversation-turn-"]')).filter(visible);
  const users = turns.flatMap(turn =>
    Array.from(turn.querySelectorAll('[data-message-author-role="user"]')).filter(visible)
  );
  const user = users.length ? users[users.length - 1] : null;
  if (!user) return JSON.stringify({userCount: 0, messageId: '', text: '', items: []});
	const turn = user.closest('[data-testid^="conversation-turn-"]');
	const turnMessageNodes = turn ? Array.from(turn.querySelectorAll('[data-message-id]')) : [];
	const messageNode = user.matches('[data-message-id]') ? user :
		(user.closest('[data-message-id]') || user.querySelector('[data-message-id]') ||
		 (turn && turn.matches('[data-message-id]') ? turn : null) ||
		 (turnMessageNodes.length === 1 ? turnMessageNodes[0] : null));
	const messageId = messageNode ? (messageNode.getAttribute('data-message-id') || '') : '';

  const itemSelector = [
    '[data-file-name]',
    '[data-testid*="attachment" i][data-testid*="chip" i]',
    '[data-testid*="attachment" i][data-testid*="pill" i]',
    '[data-testid*="attachment" i][data-testid*="preview" i]',
    '[data-testid*="attachment" i][data-testid*="item" i]',
    '[data-testid*="file" i][data-testid*="chip" i]',
    '[data-testid*="file" i][data-testid*="pill" i]',
    '[data-testid*="file" i][data-testid*="preview" i]',
    '[data-testid*="file" i][data-testid*="item" i]',
    '[data-testid*="file" i][data-testid*="thumbnail" i]',
    '[role="group"][aria-label][class*="file-tile"]',
    '[role="group"][aria-label]:has([data-testid="library-file-icon"])'
  ].join(',');
  let items = Array.from(user.querySelectorAll(itemSelector)).filter(visible);
  items = items.filter((el, index) => items.indexOf(el) === index &&
    !items.some(other => other !== el && other.contains(el))
  );
  const itemValues = items.map(el => {
    const values = [];
    for (const raw of [
      el.getAttribute('data-file-name') || '', el.getAttribute('title') || '',
      el.getAttribute('aria-label') || '', el.innerText || ''
    ]) {
      for (const line of String(raw).split(/\r?\n/)) {
        const trimmed = line.trim();
        if (trimmed && !values.includes(trimmed)) values.push(trimmed);
      }
    }
    return values;
  });
  const clone = user.cloneNode(true);
  for (const node of Array.from(clone.querySelectorAll(
    itemSelector + ', button, script, style, svg, template, [hidden], [aria-hidden="true"], [role="status"], [role="progressbar"]'
  ))) node.remove();
  return JSON.stringify({userCount: users.length, messageId, text: (clone.textContent || '').trim(), items: itemValues});
}`

func (c *Client) submittedTurnState(ctx context.Context) (submittedTurnState, error) {
	var state submittedTurnState
	if err := c.session.AssertChatGPTOrigin(ctx); err != nil {
		return state, err
	}
	value, err := c.session.Page.Context(ctx).Eval(submittedTurnStateJS)
	if err != nil {
		return state, err
	}
	if err := c.session.AssertChatGPTOrigin(ctx); err != nil {
		return state, err
	}
	if err := decodeJSONString(value.Value, &state); err != nil {
		return state, err
	}
	return state, nil
}

func (c *Client) newConversationTransaction(ctx context.Context, prompt string, attachments []string) (*conversationTransaction, error) {
	transaction := &conversationTransaction{
		id:          c.chatID,
		prompt:      normalizeSubmittedText(prompt),
		attachments: append([]string(nil), attachments...),
		targetID:    c.session.Page.TargetID,
	}
	state, err := c.submittedTurnState(ctx)
	if err != nil {
		return nil, fmt.Errorf("capture pre-send user-turn state: %w", err)
	}
	transaction.beforeUserCount = state.UserCount
	transaction.beforeUserMessageID = state.MessageID
	if transaction.id != "" {
		return transaction, nil
	}
	transaction.requiresNew = true
	return transaction, nil
}

// waitForSubmissionStart bounds the gap between activating the composer and
// polling for a response. Composer clearing and stop-button transitions are
// intentionally not evidence: both can be transient even when ChatGPT ignores
// an activation. A causal conversation request or a newly rendered matching
// user turn is strong enough to proceed. The caller must not activate again on
// timeout because delivery may still be ambiguous.
func (transaction *conversationTransaction) waitForSubmissionStart(c *Client, ctx context.Context) error {
	waitCtx, cancel := context.WithTimeout(ctx, submissionStartTimeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error

	for {
		if err := transaction.verify(c, waitCtx, true); err != nil {
			if !errors.Is(err, errConversationTransitionPending) {
				if waitCtx.Err() == nil {
					return fmt.Errorf("verify browser state while awaiting prompt activation: %w", err)
				}
				lastErr = err
			}
		} else if transaction.requiresNew && transaction.id != "" {
			// A new-chat route is only adopted after verifyTransitionEvidence
			// correlates it with the exact observed request and rendered turn.
			return nil
		}

		messageID, err := transaction.observedSubmissionID(waitCtx)
		if err == nil && messageID != "" && transaction.attachments == nil {
			return nil
		}
		if err != nil && !errors.Is(err, errConversationTransitionPending) {
			lastErr = err
		}

		state, err := c.submittedTurnState(waitCtx)
		if err == nil {
			if transaction.renderedSubmissionStarted(state) {
				return nil
			}
		} else {
			lastErr = err
		}

		select {
		case <-waitCtx.Done():
			if err := ctx.Err(); err != nil {
				return err
			}
			if lastErr != nil {
				return fmt.Errorf("prompt activation was not acknowledged within %s (last observation error: %v)", submissionStartTimeout, lastErr)
			}
			return fmt.Errorf("prompt activation was not acknowledged within %s", submissionStartTimeout)
		case <-ticker.C:
		}
	}
}

func (transaction *conversationTransaction) renderedSubmissionStarted(state submittedTurnState) bool {
	// Rendered text/count changes are not causal evidence because virtualized
	// history can hydrate while a request is in flight. Require the exact user
	// message id observed in the outgoing conversation request.
	if transaction.observedMessageID == "" ||
		!submittedMessageIDPattern.MatchString(state.MessageID) ||
		state.MessageID != transaction.observedMessageID ||
		state.MessageID == transaction.beforeUserMessageID ||
		(transaction.requiresNew && state.UserCount <= transaction.beforeUserCount) ||
		normalizeSubmittedText(state.Text) != transaction.prompt {
		return false
	}
	attachments := composerAttachmentState{Items: state.Items}
	if transaction.attachments == nil {
		return len(state.Items) == 0
	}
	return matchExpectedAttachments(attachments, transaction.attachments) == nil
}

func (transaction *conversationTransaction) verifyTransitionEvidence(c *Client, ctx context.Context) error {
	messageID, err := transaction.observedSubmissionID(ctx)
	if err != nil {
		return err
	}
	state, err := c.submittedTurnState(ctx)
	if err != nil {
		return err
	}
	if state.UserCount <= transaction.beforeUserCount {
		return errConversationTransitionPending
	}
	if normalizeSubmittedText(state.Text) != transaction.prompt {
		return fmt.Errorf("new conversation does not contain the prompt submitted by this operation")
	}
	if state.MessageID == "" || state.MessageID != messageID {
		return fmt.Errorf("new conversation user turn is not the message emitted by this operation")
	}
	attachmentState := composerAttachmentState{Items: state.Items}
	if transaction.attachments == nil {
		if len(state.Items) != 0 {
			return fmt.Errorf("new conversation user turn contains unexpected attachments")
		}
		return nil
	}
	if err := matchExpectedAttachments(attachmentState, transaction.attachments); err != nil {
		return fmt.Errorf("new conversation attachment evidence: %w", err)
	}
	return nil
}

// armSubmissionObserver starts a CDP network listener immediately before the
// composer activation. A matching request acknowledges both new and existing
// conversation submissions; new-chat route adoption additionally requires its
// user-message id to appear in the rendered user turn. Matching text alone is
// intentionally insufficient for route adoption because an existing
// conversation can contain the same prompt.
func (transaction *conversationTransaction) armSubmissionObserver(c *Client, setupCtx, lifetimeCtx context.Context) error {
	if transaction.observer != nil {
		return fmt.Errorf("submission observer is already armed")
	}
	if c.session == nil || c.session.Page == nil || c.session.Page.TargetID != transaction.targetID {
		return fmt.Errorf("browser target changed before submission")
	}
	maxPostDataSize := 8 << 20
	if err := (proto.NetworkEnable{MaxPostDataSize: &maxPostDataSize}).Call(c.session.Page.Context(setupCtx)); err != nil {
		return fmt.Errorf("enable causal submission tracking: %w", err)
	}

	observerCtx, cancel := context.WithTimeout(lifetimeCtx, 15*time.Second)
	observer := &submissionObserver{
		result: make(chan submissionObservation, 1),
		done:   make(chan struct{}),
		cancel: cancel,
	}
	transaction.observer = observer
	page := c.session.Page.Context(observerCtx)
	wait := page.EachEvent(func(event *proto.NetworkRequestWillBeSent) bool {
		if !observer.accepting.Load() {
			return false
		}
		messageID, ok := submittedMessageIDFromRequest(event, transaction.prompt)
		if !ok {
			return false
		}
		observer.result <- submissionObservation{messageID: messageID}
		return true
	})
	go func() {
		defer close(observer.done)
		wait()
		if observerCtx.Err() != nil {
			select {
			case observer.result <- submissionObservation{err: fmt.Errorf("capture submission identity: %w", observerCtx.Err())}:
			default:
			}
		}
	}()
	return nil
}

func (transaction *conversationTransaction) beginSubmission() error {
	if transaction.observer == nil {
		return fmt.Errorf("submission observer is not armed")
	}
	transaction.observer.accepting.Store(true)
	return nil
}

func (transaction *conversationTransaction) observedSubmissionID(ctx context.Context) (string, error) {
	if transaction.observedMessageID != "" {
		return transaction.observedMessageID, nil
	}
	if transaction.observer == nil {
		return "", fmt.Errorf("submission observer is not armed")
	}
	select {
	case observation := <-transaction.observer.result:
		if observation.err != nil {
			return "", observation.err
		}
		if observation.messageID == "" {
			return "", fmt.Errorf("submission did not expose a message identity")
		}
		transaction.observedMessageID = observation.messageID
		return observation.messageID, nil
	case <-transaction.observer.done:
		select {
		case observation := <-transaction.observer.result:
			if observation.err != nil {
				return "", observation.err
			}
			transaction.observedMessageID = observation.messageID
			return observation.messageID, nil
		default:
			return "", fmt.Errorf("submission ended without a message identity")
		}
	case <-ctx.Done():
		return "", ctx.Err()
	default:
		return "", errConversationTransitionPending
	}
}

func (transaction *conversationTransaction) close() {
	if transaction.observer != nil && transaction.observer.cancel != nil {
		transaction.observer.cancel()
	}
}

func submittedMessageIDFromRequest(event *proto.NetworkRequestWillBeSent, expectedPrompt string) (string, bool) {
	if event == nil || event.Request == nil || !strings.EqualFold(event.Request.Method, "POST") || event.Request.PostData == "" {
		return "", false
	}
	u, err := url.Parse(event.Request.URL)
	if err != nil || browser.ValidateChatGPTURL(event.Request.URL) != nil ||
		!strings.HasSuffix(strings.TrimSuffix(strings.ToLower(u.Path), "/"), "/conversation") {
		return "", false
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(event.Request.PostData), &payload); err != nil {
		return "", false
	}
	messages, ok := payload["messages"].([]any)
	if !ok {
		return "", false
	}
	// Only the newest outgoing user message can acknowledge this activation.
	// Older messages may be replayed as request history and can legitimately
	// contain the same text, so recursively accepting any match is ambiguous.
	for index := len(messages) - 1; index >= 0; index-- {
		rawMessage := messages[index]
		message, ok := rawMessage.(map[string]any)
		if !ok || messageAuthorRole(message) != "user" {
			continue
		}
		if !containsExactSubmittedText(message["content"], expectedPrompt) {
			return "", false
		}
		messageID, _ := message["id"].(string)
		if !submittedMessageIDPattern.MatchString(messageID) {
			return "", false
		}
		return messageID, true
	}
	return "", false
}

func messageAuthorRole(message map[string]any) string {
	author, _ := message["author"].(map[string]any)
	role, _ := author["role"].(string)
	return strings.ToLower(strings.TrimSpace(role))
}

func containsExactSubmittedText(value any, expected string) bool {
	content, ok := value.(map[string]any)
	if !ok {
		return false
	}
	parts, ok := content["parts"].([]any)
	if !ok {
		return false
	}
	for _, rawPart := range parts {
		switch part := rawPart.(type) {
		case string:
			if normalizeSubmittedText(part) == expected {
				return true
			}
		case map[string]any:
			// Some multimodal payloads wrap a textual part explicitly. Never
			// recurse into metadata such as content_type or asset pointers.
			if text, ok := part["text"].(string); ok && normalizeSubmittedText(text) == expected {
				return true
			}
		}
	}
	return false
}

func normalizeSubmittedText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	// ProseMirror represents indentation with NBSP characters in both the DOM
	// and the outgoing conversation payload. Compare that browser encoding to
	// the ordinary spaces supplied by the caller.
	value = strings.ReplaceAll(value, "\u00a0", " ")
	return strings.TrimSpace(value)
}
