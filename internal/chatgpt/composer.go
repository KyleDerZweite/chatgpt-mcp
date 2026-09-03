package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-rod/rod"
)

const (
	composerPromptSelector = `#prompt-textarea, [data-testid="prompt-textarea"], textarea[placeholder*="Message"], .ProseMirror[contenteditable="true"], div[contenteditable="true"]`
	composerSendSelector   = `[data-testid="send-button"], [data-testid="composer-send-button"], [data-testid="composer-submit-button"], button[aria-label*="Send"]`
	composerStopSelector   = `[data-testid="stop-button"], [data-testid="composer-stop-button"], button[aria-label="Stop generating"], button[aria-label="Stop response"]`
)

// composerRootJS only accepts a root that contains a visible prompt and a
// visible send/stop control. Generic editors and unrelated file inputs are not
// eligible. Returning null for anything except one unique root makes callers
// fail closed during responsive duplicate or transition states.
const composerRootJS = `function() {
  const visible = el => {
    if (!el || !el.isConnected || el.closest('[hidden], [aria-hidden="true"]')) return false;
    const style = getComputedStyle(el);
    return style.display !== 'none' && style.visibility !== 'hidden' && el.getClientRects().length > 0;
  };
  const prompts = Array.from(new Set(Array.from(document.querySelectorAll(
    '#prompt-textarea, [data-testid="prompt-textarea"], textarea[placeholder*="Message"], .ProseMirror[contenteditable="true"], div[contenteditable="true"]'
  )))).filter(visible);
  const eligible = (prompt, root) => {
    if (prompt.matches('#prompt-textarea, [data-testid="prompt-textarea"]')) return true;
    if (root.matches('[data-testid*="composer" i]')) return true;
    return !!root.querySelector(
      'input[type="file"], [data-testid*="composer" i], button[aria-label*="attach" i], ' +
      'button[aria-label*="upload" i], button[aria-label*="voice" i], [data-testid*="send" i]'
    );
  };
  const roots = [];
  for (const prompt of prompts) {
    const root = prompt.closest('form') || prompt.closest('[data-testid="composer"]') ||
      prompt.closest('[data-testid*="composer" i]');
    if (root && eligible(prompt, root) && !roots.includes(root)) roots.push(root);
  }
  return roots.length === 1 ? roots[0] : null;
}`

const composerTopologyJS = `function() {
  const visible = el => {
    if (!el || !el.isConnected || el.closest('[hidden], [aria-hidden="true"]')) return false;
    const style = getComputedStyle(el);
    return style.display !== 'none' && style.visibility !== 'hidden' && el.getClientRects().length > 0;
  };
  const prompts = Array.from(new Set(Array.from(document.querySelectorAll(
    '#prompt-textarea, [data-testid="prompt-textarea"], textarea[placeholder*="Message"], .ProseMirror[contenteditable="true"], div[contenteditable="true"]'
  )))).filter(visible);
  const eligible = (prompt, root) => {
    if (prompt.matches('#prompt-textarea, [data-testid="prompt-textarea"]')) return true;
    if (root.matches('[data-testid*="composer" i]')) return true;
    return !!root.querySelector(
      'input[type="file"], [data-testid*="composer" i], button[aria-label*="attach" i], ' +
      'button[aria-label*="upload" i], button[aria-label*="voice" i], [data-testid*="send" i]'
    );
  };
  const roots = [];
  for (const prompt of prompts) {
    const root = prompt.closest('form') || prompt.closest('[data-testid="composer"]') ||
      prompt.closest('[data-testid*="composer" i]');
    if (root && eligible(prompt, root) && !roots.includes(root)) roots.push(root);
  }
  return JSON.stringify({rootCount: roots.length, promptCount: prompts.length});
}`

type composerTopology struct {
	RootCount   int `json:"rootCount"`
	PromptCount int `json:"promptCount"`
}

type composerElements struct {
	Root      *rod.Element
	Prompt    *rod.Element
	FileInput *rod.Element
}

var errComposerUnavailable = errors.New("verified ChatGPT composer is not available")

func resolveComposerOnce(page *rod.Page, ctx context.Context, requireFileInput bool) (*composerElements, error) {
	page = page.Context(ctx)
	topologyValue, err := page.Eval(composerTopologyJS)
	if err != nil {
		return nil, err
	}
	var topology composerTopology
	if err := decodeJSONString(topologyValue.Value, &topology); err != nil {
		return nil, fmt.Errorf("decode composer topology: %w", err)
	}
	if topology.RootCount == 0 {
		return nil, errComposerUnavailable
	}
	if topology.RootCount != 1 {
		return nil, fmt.Errorf("ambiguous ChatGPT composer: found %d eligible roots", topology.RootCount)
	}

	root, err := page.Sleeper(rod.NotFoundSleeper).ElementByJS(rod.Eval(composerRootJS))
	if err != nil || root == nil {
		return nil, errComposerUnavailable
	}
	root = root.Context(ctx)
	prompts, err := uniqueVisibleElements(root, composerPromptSelector)
	if err != nil {
		return nil, fmt.Errorf("inspect composer prompts: %w", err)
	}
	if len(prompts) != 1 {
		return nil, fmt.Errorf("ambiguous ChatGPT composer: found %d visible prompt inputs in the selected root", len(prompts))
	}
	result := &composerElements{Root: root, Prompt: prompts[0].Context(ctx)}
	if requireFileInput {
		inputs, err := root.Elements(`input[type="file"]`)
		if err != nil {
			return nil, fmt.Errorf("inspect composer file inputs: %w", err)
		}
		if len(inputs) == 0 {
			return nil, errComposerUnavailable
		}
		if len(inputs) > 1 {
			return nil, fmt.Errorf("ambiguous ChatGPT composer: found %d file inputs in the selected root", len(inputs))
		}
		result.FileInput = inputs[0].Context(ctx)
	}
	return result, nil
}

func uniqueVisibleElements(root *rod.Element, selector string) ([]*rod.Element, error) {
	elements, err := root.Elements(selector)
	if err != nil {
		return nil, err
	}
	visible := make([]*rod.Element, 0, len(elements))
	for _, element := range elements {
		ok, err := element.Visible()
		if err != nil {
			return nil, err
		}
		if ok {
			visible = append(visible, element)
		}
	}
	return visible, nil
}

func findComposerSendOnce(ctx context.Context, composer *composerElements) (*rod.Element, error) {
	sends, err := uniqueVisibleElements(composer.Root.Context(ctx), composerSendSelector)
	if err != nil {
		return nil, fmt.Errorf("inspect composer send controls: %w", err)
	}
	if len(sends) == 0 {
		return nil, errComposerUnavailable
	}
	if len(sends) != 1 {
		return nil, fmt.Errorf("ambiguous ChatGPT composer: found %d visible send controls in the selected root", len(sends))
	}
	return sends[0].Context(ctx), nil
}

func (c *Client) findComposer(ctx context.Context, max time.Duration, requireFileInput bool) (*composerElements, error) {
	searchCtx, cancel := context.WithTimeout(ctx, max)
	defer cancel()
	var lastErr error
	for {
		if err := c.session.AssertChatGPTOrigin(searchCtx); err != nil {
			return nil, err
		}
		composer, err := resolveComposerOnce(c.session.Page, searchCtx, requireFileInput)
		if err == nil {
			if err := c.session.AssertChatGPTOrigin(searchCtx); err != nil {
				return nil, err
			}
			return composer, nil
		}
		lastErr = err
		if !errors.Is(err, errComposerUnavailable) {
			return nil, err
		}
		timer := time.NewTimer(300 * time.Millisecond)
		select {
		case <-searchCtx.Done():
			timer.Stop()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("composer not found before timeout: %w", lastErr)
		case <-timer.C:
		}
	}
}

// composerAttachmentStateJS deliberately distinguishes file inputs from
// rendered attachment items. Readiness requires exact per-item filename
// matches; concatenated text and substring matching are never used.
const composerAttachmentStateJS = `function() {
  const visible = el => {
    if (!el || !el.isConnected || el.closest('[hidden], [aria-hidden="true"]')) return false;
    const style = getComputedStyle(el);
    return style.display !== 'none' && style.visibility !== 'hidden' && el.getClientRects().length > 0;
  };
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
    '[data-testid*="file" i][data-testid*="thumbnail" i]'
  ].join(',');
  let items = Array.from(this.querySelectorAll(itemSelector)).filter(visible);
  items = items.filter((el, index) => items.indexOf(el) === index &&
    !items.some(other => other !== el && other.contains(el))
  );
  const itemValues = items.map(el => {
    const raw = [
      el.getAttribute('data-file-name') || '',
      el.getAttribute('title') || '',
      el.getAttribute('aria-label') || '',
      el.innerText || ''
    ];
    const values = [];
    for (const value of raw) {
      for (const line of String(value).split(/\r?\n/)) {
        const trimmed = line.trim();
        if (trimmed && !values.includes(trimmed)) values.push(trimmed);
      }
    }
    return values;
  });
  const inputNames = Array.from(this.querySelectorAll('input[type="file"]')).flatMap(input =>
    Array.from(input.files || []).map(file => file.name)
  );
  const busy = Array.from(this.querySelectorAll(
    '[role="progressbar"], [aria-busy="true"][data-testid*="upload" i], ' +
    '[data-testid*="upload" i][class*="loading" i], [data-testid*="attachment" i][class*="loading" i]'
  )).some(visible);
  const errorElement = Array.from(this.querySelectorAll(
    '[role="alert"], [data-testid*="upload" i][data-state="error"], [data-testid*="attachment" i][data-state="error"]'
  )).find(visible);
  return JSON.stringify({
    items: itemValues,
    inputNames,
    busy,
    error: errorElement ? (errorElement.innerText || 'upload failed').trim() : ''
  });
}`

type composerAttachmentState struct {
	Items      [][]string `json:"items"`
	InputNames []string   `json:"inputNames"`
	Busy       bool       `json:"busy"`
	Error      string     `json:"error"`
}

func readComposerAttachmentStateUnchecked(ctx context.Context, composer *composerElements, script string) (composerAttachmentState, error) {
	var state composerAttachmentState
	value, err := composer.Root.Context(ctx).Eval(script)
	if err != nil {
		return state, err
	}
	if err := decodeJSONString(value.Value, &state); err != nil {
		return state, err
	}
	return state, nil
}

func (c *Client) readComposerAttachmentState(ctx context.Context, composer *composerElements) (composerAttachmentState, error) {
	if err := c.session.AssertChatGPTOrigin(ctx); err != nil {
		return composerAttachmentState{}, err
	}
	guarded := `function() {
	  if (location.protocol !== 'https:' || location.hostname !== 'chatgpt.com' || location.port !== '' || location.origin !== 'https://chatgpt.com') {
	    throw new Error('untrusted browser origin');
	  }
	  return (` + composerAttachmentStateJS + `).call(this);
	}`
	state, err := readComposerAttachmentStateUnchecked(ctx, composer, guarded)
	if originErr := c.session.AssertChatGPTOrigin(ctx); originErr != nil {
		return composerAttachmentState{}, originErr
	}
	return state, err
}

func decodeJSONString(value interface{ MarshalJSON() ([]byte, error) }, target any) error {
	raw, err := value.MarshalJSON()
	if err != nil {
		return err
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return err
	}
	return json.Unmarshal([]byte(encoded), target)
}

func requireEmptyComposer(state composerAttachmentState) error {
	if state.Error != "" {
		return fmt.Errorf("composer reports an attachment error: %s", state.Error)
	}
	if state.Busy {
		return fmt.Errorf("composer already has an upload in progress")
	}
	if len(state.Items) != 0 || len(state.InputNames) != 0 {
		return fmt.Errorf("composer already contains attachments; call chatgpt_new_chat before continuing")
	}
	return nil
}

func matchExpectedAttachments(state composerAttachmentState, expected []string) error {
	if state.Error != "" {
		return fmt.Errorf("ChatGPT reported an upload error: %s", state.Error)
	}
	if state.Busy {
		return fmt.Errorf("attachments are still uploading")
	}
	if len(state.InputNames) != 0 && !sameFilenameSet(state.InputNames, expected) {
		return fmt.Errorf("composer file input contains files other than the requested set")
	}
	if len(state.Items) != len(expected) {
		return fmt.Errorf("composer exposes %d attachment items; expected %d", len(state.Items), len(expected))
	}
	matched := make([]bool, len(state.Items))
	for _, expectedName := range expected {
		found := false
		for index, values := range state.Items {
			if matched[index] {
				continue
			}
			for _, value := range values {
				if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(expectedName)) {
					matched[index] = true
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			return fmt.Errorf("attachment %q is not represented by an exact composer item", expectedName)
		}
	}
	return nil
}

func sameFilenameSet(actual, expected []string) bool {
	if len(actual) != len(expected) {
		return false
	}
	used := make([]bool, len(actual))
	for _, expectedName := range expected {
		found := false
		for index, actualName := range actual {
			if !used[index] && strings.EqualFold(strings.TrimSpace(actualName), strings.TrimSpace(expectedName)) {
				used[index] = true
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
