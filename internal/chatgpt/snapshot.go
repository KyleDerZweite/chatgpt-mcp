package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"chatgpt-mcp/internal/browser"
)

type snapshot struct {
	TurnCount           int    `json:"turnCount"`
	AssistantCount      int    `json:"assistantCount"`
	LastAssistantID     string `json:"lastAssistantId"`
	IsGenerating        bool   `json:"isGenerating"`
	HasResponseContent  bool   `json:"hasResponseContent"`
	TerminalSignal      bool   `json:"terminalSignal"`
	ResearchWorkflow    bool   `json:"researchWorkflow"`
	ContentVersion      string `json:"contentVersion"`
	ResponseMarkdown    string `json:"responseMarkdown"`
	ResponseText        string `json:"responseText"`
	HasSemanticMarkdown bool   `json:"hasSemanticMarkdown"`
}

// snapshotStateJS intentionally does no response serialization. Long-running
// operations poll this small state object and extract the answer only after a
// stable, explicit terminal signal appears.
const snapshotVisibilityJS = `
  function hiddenByClosedDetails(el) {
    for (let current = el; current && current.parentElement; current = current.parentElement) {
      const parent = current.parentElement;
      if (parent.tagName && parent.tagName.toLowerCase() === 'details' && !parent.open) {
        const summary = Array.from(parent.children).find(child =>
          child.tagName && child.tagName.toLowerCase() === 'summary'
        );
        if (!summary || (current !== summary && !summary.contains(current))) return true;
      }
    }
    return false;
  }
  function clippedOrZeroBox(el, style) {
    // Explicit clipping can hide arbitrary descendants. Treat any such subtree
    // as non-answer UI rather than attempting incomplete pixel geometry.
    const clip = String(style.clip || '').toLowerCase().replace(/\s+/g, '');
    if (clip && clip !== 'auto') return true;
    const clipPath = String(style.clipPath || '').toLowerCase().replace(/\s+/g, '');
    if (clipPath && clipPath !== 'none') return true;
    const rect = el.getBoundingClientRect();
    const clipsX = style.overflowX === 'hidden' || style.overflowX === 'clip';
    const clipsY = style.overflowY === 'hidden' || style.overflowY === 'clip';
    return (rect.width <= 0 && clipsX) || (rect.height <= 0 && clipsY);
  }
  function presentationHidden(el) {
    if (!el || !el.isConnected || el.closest('[hidden], [aria-hidden="true"]') || hiddenByClosedDetails(el)) return true;
    for (let current = el; current && current.nodeType === Node.ELEMENT_NODE; current = current.parentElement) {
      const style = getComputedStyle(current);
      if (style.display === 'none' || style.visibility === 'hidden' || style.visibility === 'collapse' ||
          Number.parseFloat(style.opacity) === 0 || style.contentVisibility === 'hidden' || clippedOrZeroBox(current, style)) {
        return true;
      }
    }
    return false;
  }
  function visible(el) {
    return !presentationHidden(el) && Array.from(el.getClientRects()).some(rect => rect.width > 0 && rect.height > 0);
  }
`

const snapshotStateJS = `function() {` + snapshotVisibilityJS + `
  function excludedStateNode(node) {
    if (!node || node.nodeType !== Node.ELEMENT_NODE || presentationHidden(node)) return true;
    const tag = node.tagName.toLowerCase();
    if (['button', 'script', 'style', 'svg', 'template'].includes(tag)) return true;
    if (node.closest('[role="status"], [role="progressbar"]')) return true;
    const testid = (node.getAttribute('data-testid') || '').toLowerCase();
    if (/thinking|reasoning|research-progress|status|turn-action|copy-turn|feedback/.test(testid)) return true;
    return !!node.closest(
      '[data-testid*="thinking" i], [data-testid*="reasoning" i], ' +
      '[data-testid*="research-progress" i], [data-testid*="status" i]'
    );
  }
  function excludedContent(el, assistant) {
    const excluded = el.closest(
      '[data-testid*="thinking" i], [data-testid*="reasoning" i], ' +
      '[data-testid*="research-progress" i], [data-testid*="status" i], [role="status"]'
    );
    return !!(excluded && excluded !== assistant && assistant.contains(excluded));
  }
  function contentBlocks(assistant) {
    if (!assistant) return [];
    const selectors = ['[data-message-content]', '.markdown.prose', '.markdown', '.prose'];
    for (const selector of selectors) {
      const found = Array.from(assistant.querySelectorAll(selector)).filter(el => visible(el) && !excludedContent(el, assistant));
      if (found.length) {
        return found.filter((el, index) => found.indexOf(el) === index && !found.some(other => other !== el && other.contains(el)));
      }
    }
    return [];
  }
	function activeConversationRoot() {
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
	  if (composerRoots.length !== 1) throw new Error('ambiguous ChatGPT composer while reading conversation');
	  const conversation = composerRoots[0].closest('main, [role="main"]');
	  if (!conversation || !visible(conversation)) throw new Error('active ChatGPT conversation container not found');
	  return conversation;
	}
  function contentVersion(blocks) {
		const canonical = block => {
		  const liveNodes = [block].concat(Array.from(block.querySelectorAll('*')));
		  const clone = block.cloneNode(true);
		  const cloneNodes = [clone].concat(Array.from(clone.querySelectorAll('*')));
		  for (let index = liveNodes.length - 1; index > 0; index--) {
		    if (excludedStateNode(liveNodes[index])) cloneNodes[index].remove();
		  }
		  return clone.innerHTML || '';
		};
		const value = blocks.map(canonical).join('\u241e');
    let first = 2166136261;
    let second = 2246822507;
    for (let index = 0; index < value.length; index++) {
      const code = value.charCodeAt(index);
      first = Math.imul(first ^ code, 16777619);
      second = Math.imul(second ^ code, 3266489917);
    }
    return value.length + ':' + (first >>> 0).toString(16) + ':' + (second >>> 0).toString(16);
  }
	function contentText(block) {
	  const read = node => {
	    if (node.nodeType === Node.TEXT_NODE) return node.nodeValue || '';
	    if (node.nodeType !== Node.ELEMENT_NODE || excludedStateNode(node)) return '';
	    return Array.from(node.childNodes).map(read).join('');
	  };
	  return read(block).trim();
	}
	const conversation = activeConversationRoot();
	const turns = Array.from(conversation.querySelectorAll('[data-testid^="conversation-turn-"]')).filter(visible);
	const assistants = turns.flatMap(turn =>
	  Array.from(turn.querySelectorAll('[data-message-author-role="assistant"]')).filter(visible)
	);
  const assistant = assistants.length ? assistants[assistants.length - 1] : null;
  const turn = assistant && (assistant.closest('[data-testid^="conversation-turn-"]') || assistant.closest('[data-message-id]'));
  const identity = assistant && (
    assistant.getAttribute('data-message-id') ||
    (assistant.closest('[data-message-id]') && assistant.closest('[data-message-id]').getAttribute('data-message-id')) ||
    (turn && (turn.getAttribute('data-testid') || turn.id)) ||
    assistant.id || ''
  );
  const blocks = contentBlocks(assistant);
  const hasContent = blocks.some(el => contentText(el) !== '' ||
    Array.from(el.querySelectorAll('img[src]')).some(image => visible(image) && !excludedStateNode(image))
  );
  const stopSelectors = [
    '[data-testid="stop-button"]',
    '[data-testid="composer-stop-button"]',
    'button[aria-label="Stop generating"]',
    'button[aria-label="Stop response"]'
  ];
	const stopVisible = stopSelectors.some(selector => Array.from(conversation.querySelectorAll(selector)).some(visible));
  const streamingSelector = '[data-is-streaming="true"], [aria-busy="true"], [data-testid*="streaming" i]';
  const streaming = !!(assistant && (
    assistant.matches('[data-is-streaming="true"], [aria-busy="true"]') ||
    Array.from(assistant.querySelectorAll(streamingSelector)).some(visible) ||
    (turn && (turn.matches('[data-is-streaming="true"], [aria-busy="true"]') ||
      Array.from(turn.querySelectorAll(streamingSelector)).some(visible)))
  ));
  const researchRunning = !!(turn && Array.from(turn.querySelectorAll(
    '[data-testid*="research" i][aria-busy="true"], [data-testid*="research" i][data-state="running"], [role="progressbar"]'
  )).some(visible));
	// Deep-research has no public, durable browser-DOM completion contract. A
	// recognized research turn or explicitly selected/running research control
	// is therefore rejected by the Go layer instead of being mistaken for a
	// completed ordinary response during a quiet phase.
	const researchTurnSelector =
	  '[data-testid*="deep-research" i], [data-testid*="research" i], [data-research-state], [data-research-id]';
	const researchTurn = !!(turn && (
	  (turn.matches(researchTurnSelector) && visible(turn)) ||
	  Array.from(turn.querySelectorAll(researchTurnSelector)).some(visible)
	));
	const activeResearchControl = Array.from(conversation.querySelectorAll(
	  '[data-testid*="research-progress" i], [data-testid*="research" i][aria-busy="true"], ' +
	  '[data-testid*="research" i][aria-pressed="true"], [data-testid*="research" i][aria-selected="true"], ' +
	  '[data-testid*="research" i][aria-checked="true"], input[data-testid*="research" i]:checked, ' +
	  '[data-testid*="research" i][data-active="true"], [data-testid*="research" i][data-state="active"], ' +
	  '[data-testid*="research" i][data-state="selected"], [data-testid*="research" i][data-state="on"], ' +
	  '[data-testid*="research" i][data-state="running"], [data-research-state="active" i], ' +
	  '[data-research-state="pending" i], [data-research-state="running" i], ' +
	  '[data-mode*="research" i][aria-pressed="true"], [data-mode*="research" i][aria-selected="true"], ' +
	  '[data-tool*="research" i][aria-pressed="true"], [aria-label*="deep research" i][aria-pressed="true"], ' +
	  '[aria-label*="deep research" i][aria-checked="true"]'
	)).some(visible);
  const terminalAction = !!(turn && Array.from(turn.querySelectorAll(
    '[data-testid="copy-turn-action-button"], [data-testid*="copy" i][data-testid*="turn" i], ' +
    '[data-testid*="thumbs" i], [data-testid*="regenerate" i], button[aria-label="Copy"]'
  )).some(visible));
  const explicitFinished = !!([assistant, turn].filter(Boolean).some(el =>
    el.hasAttribute('data-is-streaming') && el.getAttribute('data-is-streaming') === 'false'
  ));
  return JSON.stringify({
    turnCount: turns.length,
    assistantCount: assistants.length,
    lastAssistantId: identity || '',
    isGenerating: stopVisible || streaming || researchRunning,
    hasResponseContent: hasContent,
    terminalSignal: terminalAction || explicitFinished,
		researchWorkflow: researchTurn || activeResearchControl,
    contentVersion: contentVersion(blocks)
  });
}`

// snapshotJS extracts only known answer-content nodes from the newest
// assistant message. It deliberately fails closed when those nodes are absent
// instead of returning the whole turn (which also contains controls/status UI).
const snapshotJS = `function() {` + snapshotVisibilityJS + `
  function hiddenOrAction(node) {
    if (!node || node.nodeType !== Node.ELEMENT_NODE) return false;
    if (presentationHidden(node)) return true;
    const tag = node.tagName.toLowerCase();
    if (['button', 'script', 'style', 'svg', 'template'].includes(tag)) return true;
    if (node.closest('[role="status"], [role="progressbar"]')) return true;
    const testid = (node.getAttribute('data-testid') || '').toLowerCase();
    if (/thinking|reasoning|research-progress|status|turn-action|copy-turn|feedback/.test(testid)) return true;
    const excludedAncestor = node.closest(
      '[data-testid*="thinking" i], [data-testid*="reasoning" i], ' +
      '[data-testid*="research-progress" i], [data-testid*="status" i]'
    );
    if (excludedAncestor) return true;
    return false;
  }
  function excludedContent(el, assistant) {
    const excluded = el.closest(
      '[data-testid*="thinking" i], [data-testid*="reasoning" i], ' +
      '[data-testid*="research-progress" i], [data-testid*="status" i], [role="status"]'
    );
    return !!(excluded && excluded !== assistant && assistant.contains(excluded));
  }
  function contentBlocks(assistant) {
    if (!assistant) return [];
    const selectors = ['[data-message-content]', '.markdown.prose', '.markdown', '.prose'];
    for (const selector of selectors) {
      const found = Array.from(assistant.querySelectorAll(selector)).filter(el => visible(el) && !excludedContent(el, assistant));
      if (found.length) {
        return found.filter((el, index) => found.indexOf(el) === index && !found.some(other => other !== el && other.contains(el)));
      }
    }
    return [];
  }
	function activeConversationRoot() {
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
	  if (composerRoots.length !== 1) throw new Error('ambiguous ChatGPT composer while reading conversation');
	  const conversation = composerRoots[0].closest('main, [role="main"]');
	  if (!conversation || !visible(conversation)) throw new Error('active ChatGPT conversation container not found');
	  return conversation;
	}
  function children(node) {
    return Array.from(node.childNodes || []).map(render).join('');
  }
	function escapeText(text) {
	  return String(text || '')
	    .replace(/\\/g, '\\\\')
	    .split(String.fromCharCode(96)).join('\\' + String.fromCharCode(96))
	    .replace(/([*_\[\]<>~])/g, '\\$1')
	    .replace(/(^|\n)([ \t]*)(#{1,6}|[-+>]|\d+[.)])(?=\s)/g, function(_, line, indent, marker) {
	      if (/^\d/.test(marker)) {
	        return line + indent + marker.slice(0, -1) + '\\' + marker.slice(-1);
	      }
	      return line + indent + '\\' + marker;
	    })
	    .replace(/(^|\n)([ \t]*)(-{3,})(?=[ \t]*(?:\n|$))/g, '$1$2\\$3')
	    .replace(/(^|\n)([ \t]*)(={2,})(?=[ \t]*(?:\n|$))/g, '$1$2\\$3');
	}
  function inlineCode(text) {
    const tick = String.fromCharCode(96);
    let width = 1;
    const runs = text.match(new RegExp(tick + '+', 'g')) || [];
    for (const run of runs) width = Math.max(width, run.length + 1);
    const fence = tick.repeat(width);
    const pad = /^\s|\s$/.test(text) ? ' ' : '';
    return fence + pad + text + pad + fence;
  }
	function filteredText(node) {
		if (node.nodeType === Node.TEXT_NODE) return node.nodeValue || '';
		if (node.nodeType !== Node.ELEMENT_NODE || hiddenOrAction(node)) return '';
		if (node.tagName && node.tagName.toLowerCase() === 'br') return '\n';
		return Array.from(node.childNodes).map(filteredText).join('');
	}
	function markdownDestination(value) {
		return String(value || '').replace(/[\u0000-\u0020\u007f()<>\\]/g, function(character) {
			return '%' + character.charCodeAt(0).toString(16).padStart(2, '0').toUpperCase();
		});
	}
  function table(node) {
		const rows = Array.from(node.querySelectorAll('tr')).filter(row => visible(row) && !hiddenOrAction(row)).map(row =>
			Array.from(row.querySelectorAll(':scope > th, :scope > td')).filter(cell => visible(cell) && !hiddenOrAction(cell)).map(cell =>
        children(cell).trim().replace(/\|/g, '\\|').replace(/\r?\n/g, '<br>')
      )
    ).filter(row => row.length > 0);
    if (rows.length === 0) return '';
    const width = Math.max.apply(null, rows.map(row => row.length));
    for (const row of rows) while (row.length < width) row.push('');
    const line = row => '| ' + row.join(' | ') + ' |\n';
    let out = line(rows[0]) + line(Array(width).fill('---'));
    for (let i = 1; i < rows.length; i++) out += line(rows[i]);
    return out + '\n';
  }
  function list(node, ordered) {
		const items = Array.from(node.children).filter(child => child.tagName && child.tagName.toLowerCase() === 'li' && visible(child) && !hiddenOrAction(child));
		const reversed = ordered && node.hasAttribute('reversed');
		const parsedStart = ordered && node.hasAttribute('start') ? Number.parseInt(node.getAttribute('start'), 10) : NaN;
		let ordinal = Number.isFinite(parsedStart) ? parsedStart : (reversed ? items.length : 1);
		return items.map(item => {
			if (ordered && item.hasAttribute('value')) {
				const itemValue = Number.parseInt(item.getAttribute('value'), 10);
				if (Number.isFinite(itemValue)) ordinal = itemValue;
			}
			const prefix = ordered ? String(ordinal) + '. ' : '- ';
			if (ordered) ordinal += reversed ? -1 : 1;

			const chunks = [];
			let direct = '';
			for (const child of item.childNodes) {
				const tag = child.nodeType === Node.ELEMENT_NODE && child.tagName ? child.tagName.toLowerCase() : '';
				if (tag === 'ul' || tag === 'ol') {
					const directText = direct.trim();
					if (directText) chunks.push({nested: false, text: directText});
					direct = '';
					const nestedText = render(child).trim();
					if (nestedText) chunks.push({nested: true, text: nestedText});
					continue;
				}
				direct += render(child);
			}
			const directText = direct.trim();
			if (directText) chunks.push({nested: false, text: directText});

			const indent = ' '.repeat(prefix.length);
			let out = '';
			for (const chunk of chunks) {
				const indented = chunk.text.replace(/\n/g, '\n' + indent);
				if (out === '') {
					out = chunk.nested ? prefix.trimEnd() + '\n' + indent + indented : prefix + indented;
				} else {
					out += '\n' + indent + indented;
				}
			}
			return (out || prefix.trimEnd()) + '\n';
		}).join('') + '\n';
  }
  function render(node) {
		if (node.nodeType === Node.TEXT_NODE) return escapeText(node.nodeValue || '');
    if (node.nodeType !== Node.ELEMENT_NODE || hiddenOrAction(node)) return '';
    const tag = node.tagName.toLowerCase();
    if (tag === 'br') return '\n';
    if (tag === 'hr') return '\n---\n\n';
    if (tag === 'p') return children(node).trim() + '\n\n';
    if (/^h[1-6]$/.test(tag)) return '#'.repeat(Number(tag[1])) + ' ' + children(node).trim() + '\n\n';
    if (tag === 'strong' || tag === 'b') return '**' + children(node) + '**';
    if (tag === 'em' || tag === 'i') return '_' + children(node) + '_';
    if (tag === 'del' || tag === 's') return '~~' + children(node) + '~~';
		if (tag === 'code' && (!node.parentElement || node.parentElement.tagName.toLowerCase() !== 'pre')) return inlineCode(filteredText(node));
    if (tag === 'pre') {
			const code = Array.from(node.querySelectorAll('code')).find(candidate => visible(candidate) && !hiddenOrAction(candidate)) || node;
      const match = (code.className || '').match(/(?:^|\s)language-([^\s]+)/);
      const language = match ? match[1] : '';
			const text = filteredText(code).replace(/\n$/, '');
      const tick = String.fromCharCode(96);
      const runs = text.match(new RegExp(tick + '+', 'g')) || [];
      let width = 3;
      for (const run of runs) width = Math.max(width, run.length + 1);
      const fence = tick.repeat(width);
      return fence + language + '\n' + text + '\n' + fence + '\n\n';
    }
    if (tag === 'a') {
      const label = children(node).trim() || node.getAttribute('href') || '';
      const href = node.getAttribute('href') || '';
			return href ? '[' + label + '](' + markdownDestination(href) + ')' : label;
    }
    if (tag === 'img') {
      const src = node.getAttribute('src') || '';
      const alt = node.getAttribute('alt') || '';
			const safeAlt = alt.replace(/\\/g, '\\\\').replace(/([\[\]])/g, '\\$1');
			return src ? '![' + safeAlt + '](' + markdownDestination(src) + ')' : '';
    }
    if (tag === 'blockquote') {
      return children(node).trim().split(/\r?\n/).map(line => '> ' + line).join('\n') + '\n\n';
    }
    if (tag === 'ul') return list(node, false);
    if (tag === 'ol') return list(node, true);
    if (tag === 'table') return table(node);
    return children(node);
  }

  function contentVersion(blocks) {
		const canonical = block => {
		  const liveNodes = [block].concat(Array.from(block.querySelectorAll('*')));
		  const clone = block.cloneNode(true);
		  const cloneNodes = [clone].concat(Array.from(clone.querySelectorAll('*')));
		  for (let index = liveNodes.length - 1; index > 0; index--) {
		    if (hiddenOrAction(liveNodes[index])) cloneNodes[index].remove();
		  }
		  return clone.innerHTML || '';
		};
		const value = blocks.map(canonical).join('\u241e');
    let first = 2166136261;
    let second = 2246822507;
    for (let index = 0; index < value.length; index++) {
      const code = value.charCodeAt(index);
      first = Math.imul(first ^ code, 16777619);
      second = Math.imul(second ^ code, 3266489917);
    }
    return value.length + ':' + (first >>> 0).toString(16) + ':' + (second >>> 0).toString(16);
  }
	function rawContent(blocks) {
		const blockTags = new Set(['address', 'article', 'blockquote', 'div', 'dl', 'fieldset', 'figure',
			'footer', 'form', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'header', 'hr', 'li', 'main',
			'nav', 'ol', 'p', 'pre', 'section', 'table', 'tr', 'ul']);
		const read = node => {
			if (node.nodeType === Node.TEXT_NODE) return node.nodeValue || '';
			if (node.nodeType !== Node.ELEMENT_NODE || hiddenOrAction(node)) return '';
			const tag = node.tagName.toLowerCase();
			if (tag === 'br') return '\n';
			let text = Array.from(node.childNodes).map(read).join('');
			if (blockTags.has(tag)) text += '\n';
			return text;
		};
		return blocks.map(read).map(text => text.trim()).filter(Boolean).join('\n\n');
	}
	function hasSemanticMarkdown(blocks) {
		const paragraphs = blocks.flatMap(block => [block].concat(Array.from(block.querySelectorAll('p')))).filter(node =>
			node.tagName && node.tagName.toLowerCase() === 'p' && !hiddenOrAction(node)
		);
		if (paragraphs.length > 1) return true;
		const semanticTags = new Set([
			'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'strong', 'b', 'em', 'i', 'del', 's',
			'code', 'pre', 'a', 'img', 'blockquote', 'ul', 'ol', 'table', 'hr'
		]);
		return blocks.some(block => [block].concat(Array.from(block.querySelectorAll('*'))).some(node => {
			if (hiddenOrAction(node)) return false;
			const tag = node.tagName.toLowerCase();
			if (!semanticTags.has(tag)) return false;
			if (tag === 'a' && !node.getAttribute('href')) return false;
			if (tag === 'img' && !node.getAttribute('src')) return false;
			return render(node).trim() !== '';
		}));
	}
	const conversation = activeConversationRoot();
	const turns = Array.from(conversation.querySelectorAll('[data-testid^="conversation-turn-"]')).filter(visible);
	const assistants = turns.flatMap(turn =>
	  Array.from(turn.querySelectorAll('[data-message-author-role="assistant"]')).filter(visible)
	);
  const assistant = assistants.length ? assistants[assistants.length - 1] : null;
  const turn = assistant && (assistant.closest('[data-testid^="conversation-turn-"]') || assistant.closest('[data-message-id]'));
  const identity = assistant && (
    assistant.getAttribute('data-message-id') ||
    (assistant.closest('[data-message-id]') && assistant.closest('[data-message-id]').getAttribute('data-message-id')) ||
    (turn && (turn.getAttribute('data-testid') || turn.id)) ||
    assistant.id || ''
  );
  const blocks = contentBlocks(assistant);
  const markdown = blocks.map(render).map(text => text.trim()).filter(Boolean).join('\n\n');
	const raw = rawContent(blocks);
  return JSON.stringify({
    turnCount: turns.length,
    assistantCount: assistants.length,
    lastAssistantId: identity || '',
		hasResponseContent: markdown !== '',
    contentVersion: contentVersion(blocks),
    responseMarkdown: markdown,
		responseText: raw,
		hasSemanticMarkdown: markdown !== '' && hasSemanticMarkdown(blocks)
  });
}`

func (c *Client) snapshotState(ctx context.Context) (snapshot, error) {
	state, err := c.evaluateSnapshot(ctx, snapshotStateJS)
	if err != nil {
		return snapshot{}, err
	}
	if err := validateSupportedWorkflow(state); err != nil {
		return snapshot{}, err
	}
	return state, nil
}

var errUnsupportedResearchWorkflow = errors.New("ChatGPT deep-research/research workflows are unsupported by browser automation because the page exposes no durable completion signal; use the official Deep Research API")

func validateSupportedWorkflow(state snapshot) error {
	if state.ResearchWorkflow {
		return errUnsupportedResearchWorkflow
	}
	return nil
}

func (c *Client) snapshot(ctx context.Context) (snapshot, error) {
	return c.evaluateSnapshot(ctx, snapshotJS)
}

func (c *Client) evaluateSnapshot(ctx context.Context, script string) (snapshot, error) {
	if err := c.session.AssertChatGPTOrigin(ctx); err != nil {
		return snapshot{}, err
	}
	guarded := `function() {
    if (location.protocol !== 'https:' || location.hostname !== 'chatgpt.com' || location.port !== '' || location.origin !== 'https://chatgpt.com') {
      throw new Error('untrusted browser origin');
    }
    return (` + script + `).call(this);
  }`
	out, err := c.evaluateSnapshotScript(ctx, guarded)
	if originErr := c.session.AssertChatGPTOrigin(ctx); originErr != nil {
		return snapshot{}, originErr
	}
	if err != nil {
		return snapshot{}, fmt.Errorf("evaluate ChatGPT snapshot on %s: %w", browser.ChatURL, err)
	}
	return out, nil
}

// evaluateSnapshotScript contains only decoding mechanics. Production callers
// use evaluateSnapshot's exact-origin checks and in-script guard; browser DOM
// fixture tests invoke this helper against an isolated about:blank document.
func (c *Client) evaluateSnapshotScript(ctx context.Context, script string) (snapshot, error) {
	var out snapshot
	obj, err := c.session.Page.Context(ctx).Eval(script)
	if err != nil {
		return out, err
	}
	raw, err := obj.Value.MarshalJSON()
	if err != nil {
		return out, err
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return out, err
	}
	err = json.Unmarshal([]byte(encoded), &out)
	return out, err
}

func (s snapshot) marker() turnMarker {
	return turnMarker{AssistantCount: s.AssistantCount, LastAssistantID: s.LastAssistantID}
}

func (m turnMarker) key() string {
	if m.LastAssistantID != "" {
		return m.LastAssistantID
	}
	return "assistant-count:" + strconv.Itoa(m.AssistantCount)
}

func (s snapshot) hasNewAssistant(before turnMarker) bool {
	if before.LastAssistantID != "" {
		return s.LastAssistantID != "" && s.LastAssistantID != before.LastAssistantID
	}
	return s.AssistantCount > before.AssistantCount
}

func (s snapshot) response() string {
	if markdown := strings.TrimSpace(s.ResponseMarkdown); markdown != "" {
		return markdown
	}
	return strings.TrimSpace(s.ResponseText)
}
