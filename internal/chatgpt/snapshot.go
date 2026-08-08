package chatgpt

import (
	"encoding/json"
	"regexp"
	"strings"
)

type snapshot struct {
	TurnCount     int    `json:"turnCount"`
	LastTurnCopy  bool   `json:"lastTurnCopy"`
	IsThinkingUI  bool   `json:"isThinking"`
	ThinkingText  string `json:"thinkingText"`
	TurnText      string `json:"turnText"`
	MarkdownText  string `json:"markdownText"`
	AssistantText string `json:"assistantText"`
}

const snapshotJS = `function() {
  const turns = document.querySelectorAll('[data-testid^="conversation-turn-"]');
  const out = {
    turnCount: turns.length,
    lastTurnCopy: false,
    isThinking: false,
    thinkingText: null,
    turnText: null,
    markdownText: null,
    assistantText: null
  };
  if (turns.length >= 2) {
    const lt = turns[turns.length - 1];
    out.lastTurnCopy = !!lt.querySelector('[data-testid="copy-turn-action-button"]');
    out.isThinking = !!lt.querySelector('[class*="thinking"],[class*="reasoning"],[data-testid*="thinking"]');
    const tEl = lt.querySelector('[data-testid*="thinking"],[class*="thinking"],[class*="reasoning"]');
    out.thinkingText = tEl ? tEl.innerText : null;
    const md = lt.querySelector('.markdown, .prose, [class*="markdown"]');
    out.markdownText = md ? md.innerText : null;
    out.turnText = lt.innerText;
  }
  const as = document.querySelectorAll('[data-message-author-role="assistant"]');
  if (as.length > 0) {
    out.assistantText = as[as.length - 1].innerText || null;
  }
  return JSON.stringify(out);
}`

func (c *Client) snapshot() (snapshot, error) {
	var out snapshot
	obj, err := c.session.Page.Eval(snapshotJS)
	if err != nil {
		return out, err
	}
	raw, err := obj.Value.MarshalJSON()
	if err != nil {
		return out, err
	}
	var str string
	if err := json.Unmarshal(raw, &str); err != nil {
		return out, err
	}
	err = json.Unmarshal([]byte(str), &out)
	return out, err
}

var thinkingWord = regexp.MustCompile(`(?i)\b(thinking|reasoning)\b`)

func (s snapshot) isThinking() bool {
	if s.IsThinkingUI || s.ThinkingText != "" {
		return true
	}
	if len(s.TurnText) < 500 && thinkingWord.MatchString(s.TurnText) {
		return true
	}
	return false
}

var phrasesToRemove = []string{
	"ChatGPT said:",
	"ChatGPT said",
	"Pro thinking",
	"Answer now",
	"Extended thinking",
	"Show thinking",
	"Hide thinking",
	"Reasoning",
	"Thinking...",
	"Thinking\u2026",
}

var leadingTiming = regexp.MustCompile(`^\d+\s*(seconds?|secs?|minutes?)\s*`)

func cleanResponse(raw string) string {
	if raw == "" {
		return ""
	}
	text := raw
	for _, phrase := range phrasesToRemove {
		text = strings.ReplaceAll(text, phrase, " ")
	}
	text = leadingTiming.ReplaceAllString(text, " ")
	text = strings.Join(strings.Fields(text), " ")
	return strings.TrimSpace(text)
}

func (s snapshot) response() string {
	if best := cleanResponse(s.TurnText); best != "" {
		return best
	}
	if best := cleanResponse(s.MarkdownText); best != "" && len(best) >= 3 {
		return best
	}
	if best := cleanResponse(s.AssistantText); best != "" && len(best) >= 3 {
		return best
	}
	return ""
}
