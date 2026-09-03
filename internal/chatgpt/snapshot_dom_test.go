package chatgpt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"

	browserpkg "chatgpt-mcp/internal/browser"
	"chatgpt-mcp/internal/config"
)

func TestDOMFixtures(t *testing.T) {
	if os.Getenv("CHATGPT_MCP_BROWSER_TESTS") != "1" {
		t.Skip("set CHATGPT_MCP_BROWSER_TESTS=1 to run browser-backed DOM fixtures")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	launcherInstance := launcher.New().Context(ctx).Headless(true).NoSandbox(true).Leakless(false)
	if chromeBin := os.Getenv("CHATGPT_CHROME_BIN"); chromeBin != "" {
		launcherInstance.Bin(chromeBin)
	}
	controlURL, err := launcherInstance.Launch()
	if err != nil {
		t.Fatalf("launch fixture browser: %v", err)
	}
	t.Cleanup(launcherInstance.Kill)
	connectionCtx, cancelConnection := context.WithCancel(context.Background())
	rodBrowser := rod.New().ControlURL(controlURL).Context(connectionCtx)
	if err := rodBrowser.Connect(); err != nil {
		t.Fatalf("connect fixture browser: %v", err)
	}
	t.Cleanup(func() {
		_ = rodBrowser.Close()
		cancelConnection()
	})
	page, err := rodBrowser.Context(ctx).Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		t.Fatalf("create fixture page: %v", err)
	}
	session := browserpkg.New(&config.Config{})
	session.Page = page
	client := New(&config.Config{}, session)

	t.Run("assistant extraction preserves structure and excludes UI", func(t *testing.T) {
		fixture := `<!doctype html><html><body>
<main>
  <div data-testid="conversation-turn-1"><div data-message-author-role="user">SECRET USER PROMPT</div></div>
  <div data-testid="conversation-turn-2">
    <div data-message-author-role="assistant" data-message-id="assistant-2">
      <div data-testid="reasoning-status" role="status"><p>SECRET RESEARCH PROGRESS</p></div>
      <div data-message-content>
        <h2>Result</h2>
		<p>Paragraph with <strong>bold</strong>, <code>x()<span aria-hidden="true">SECRET INLINE CODE</span></code>, and <a href="https://example.com/source">a source</a>.</p>
				<p>Literal *x*, _y_, and ~~z~~.</p><p># not heading</p><p>1. not a list [brackets]</p>
				<p>---</p><p>Title
===</p>
				<div role="status">SECRET NESTED STATUS</div>
		<ul><li hidden>SECRET HIDDEN LIST</li><li>First</li><li>Second</li></ul>
				<ol start="3"><li>Third</li><li value="7">Seventh</li><li>Eighth</li></ol>
		<div id="hidden-opacity" style="opacity:0"><p>SECRET ZERO OPACITY</p></div>
		<div style="visibility:collapse"><p>SECRET COLLAPSED VISIBILITY</p></div>
		<div style="content-visibility:hidden"><p>SECRET CONTENT VISIBILITY</p></div>
		<div style="position:absolute;width:1px;height:1px;overflow:hidden;clip:rect(0,0,0,0)">SECRET LEGACY CLIP</div>
		<div style="clip-path:inset(50%)">SECRET CLIP PATH</div>
		<div style="width:0;height:0;overflow:hidden">SECRET ZERO BOX</div>
		<details><summary>Visible closed disclosure summary</summary><p>SECRET CLOSED DETAILS BODY</p><summary>SECRET SECOND SUMMARY</summary></details>
		<details open><summary>Visible open disclosure summary</summary><p>Visible open details body.</p></details>
		<div style="display:contents"><span>Visible display-contents child.</span></div>
		<p style="position:absolute;top:5000px">Visible below-viewport answer.</p>
		<p>Visible line one.<br>Visible line two.</p>
        <pre><span style="display:none"><code>SECRET HIDDEN CODE WRAPPER</code></span><code class="language-go">fmt.Println("thinking and reasoning")<span role="status">SECRET FENCED CODE</span>

return</code></pre>
		<table><thead><tr><th>Name</th><th>Value</th></tr></thead><tbody style="display:none"><tr><td>SECRET HIDDEN TBODY</td><td>SECRET HIDDEN TABLE GROUP</td></tr></tbody><tbody><tr style="display:none"><td>SECRET HIDDEN TABLE</td><td>SECRET HIDDEN CELL</td></tr><tr><td>A</td><td>B</td></tr></tbody></table>
        <span aria-hidden="true">SECRET HIDDEN CONTROL</span>
        <button>SECRET BUTTON</button>
      </div>
      <div data-message-content><p>Final paragraph.</p></div>
    </div>
    <button data-testid="copy-turn-action-button">Copy</button>
  </div>
	<div data-testid="conversation-turn-hidden" style="display:none">
	  <div data-message-author-role="assistant" data-message-id="assistant-hidden"><div data-message-content>SECRET HIDDEN ANSWER</div></div>
	</div>
  <button data-testid="stop-button" style="display:none">Stop</button>
	<form>
	  <input id="prompt-textarea" />
	  <input id="hidden-file" type="file" style="display:none" />
	  <div data-testid="attachment-chip" data-file-name="old-report.pdf">old-report.pdf</div>
	  <button data-testid="send-button" type="button">Send</button>
	</form>
</main>
<div id="contenteditable-test" contenteditable="true"><p>stale draft</p></div>
<div id="composer-decoy" contenteditable="true"><p>decoy draft</p></div>
<input id="decoy-file" type="file" style="display:none" />
	<div data-message-author-role="assistant"><div data-message-content>SECRET OUTSIDE CONVERSATION</div></div>
</body></html>`
		if err := page.Context(ctx).SetDocumentContent(fixture); err != nil {
			t.Fatalf("set fixture DOM: %v", err)
		}

		state, err := client.evaluateSnapshotScript(ctx, snapshotStateJS)
		if err != nil {
			t.Fatalf("snapshot state: %v", err)
		}
		if state.AssistantCount != 1 || state.LastAssistantID != "assistant-2" || state.IsGenerating || !state.HasResponseContent || !state.TerminalSignal {
			t.Fatalf("unexpected state snapshot: %+v", state)
		}
		answer, err := client.evaluateSnapshotScript(ctx, snapshotJS)
		if err != nil {
			t.Fatalf("extract response: %v", err)
		}
		text := answer.response()
		for _, want := range []string{
			"## Result",
			"**bold**",
			"`x()`",
			"[a source](https://example.com/source)",
			"Literal \\*x\\*, \\_y\\_, and \\~\\~z\\~\\~.",
			"\\# not heading",
			"1\\. not a list \\[brackets\\]",
			"\\---",
			"Title\n\\===",
			"- First",
			"3. Third\n7. Seventh\n8. Eighth",
			"```go\nfmt.Println(\"thinking and reasoning\")\n\nreturn\n```",
			"| Name | Value |",
			"Visible closed disclosure summary",
			"Visible open disclosure summary",
			"Visible open details body.",
			"Visible display-contents child.",
			"Visible below-viewport answer.",
			"Visible line one.\nVisible line two.",
			"Final paragraph.",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("response missing %q:\n%s", want, text)
			}
		}
		for _, unwanted := range []string{"SECRET USER PROMPT", "SECRET RESEARCH PROGRESS", "SECRET NESTED STATUS", "SECRET HIDDEN CONTROL", "SECRET BUTTON", "SECRET HIDDEN ANSWER", "SECRET OUTSIDE CONVERSATION", "SECRET INLINE CODE", "SECRET FENCED CODE", "SECRET HIDDEN CODE WRAPPER", "SECRET HIDDEN LIST", "SECRET HIDDEN TBODY", "SECRET HIDDEN TABLE GROUP", "SECRET HIDDEN TABLE", "SECRET HIDDEN CELL", "SECRET ZERO OPACITY", "SECRET COLLAPSED VISIBILITY", "SECRET CONTENT VISIBILITY", "SECRET LEGACY CLIP", "SECRET CLIP PATH", "SECRET ZERO BOX", "SECRET CLOSED DETAILS BODY", "SECRET SECOND SUMMARY"} {
			if strings.Contains(text, unwanted) {
				t.Errorf("response leaked %q:\n%s", unwanted, text)
			}
			if strings.Contains(answer.ResponseText, unwanted) {
				t.Errorf("raw response leaked %q:\n%s", unwanted, answer.ResponseText)
			}
		}
		for _, want := range []string{"Result", "Literal *x*, _y_, and ~~z~~.", "Visible closed disclosure summary", "Visible open details body.", "Visible display-contents child.", "Visible below-viewport answer.", "Visible line one.\nVisible line two.", "Final paragraph."} {
			if !strings.Contains(answer.ResponseText, want) {
				t.Errorf("raw response missing %q:\n%s", want, answer.ResponseText)
			}
		}

		initialVersion := state.ContentVersion
		if _, err := page.Context(ctx).Eval(`function() {
		  document.querySelector('#hidden-opacity').textContent = 'SECRET MUTATED ZERO OPACITY';
		}`); err != nil {
			t.Fatalf("mutate hidden response content: %v", err)
		}
		state, err = client.evaluateSnapshotScript(ctx, snapshotStateJS)
		if err != nil {
			t.Fatalf("snapshot after hidden mutation: %v", err)
		}
		if state.ContentVersion != initialVersion {
			t.Fatalf("hidden response mutation changed content version: before=%q after=%q", initialVersion, state.ContentVersion)
		}

		if _, err := page.Context(ctx).Eval(`function() { document.querySelector('[data-testid="stop-button"]').style.display = 'block'; }`); err != nil {
			t.Fatalf("show generation control: %v", err)
		}
		state, err = client.evaluateSnapshotScript(ctx, snapshotStateJS)
		if err != nil {
			t.Fatalf("snapshot generating state: %v", err)
		}
		if !state.IsGenerating {
			t.Fatal("visible stop control was not recognized as generating state")
		}
	})

	t.Run("explicit research workflow is rejected", func(t *testing.T) {
		if _, err := page.Context(ctx).Eval(`function() {
		  const turn = document.querySelector('[data-testid="conversation-turn-2"]');
		  turn.insertAdjacentHTML('beforeend', '<div id="research-fixture" data-testid="deep-research-progress" data-state="running">Researching</div>');
		}`); err != nil {
			t.Fatalf("install research fixture: %v", err)
		}
		defer func() {
			_, _ = page.Context(ctx).Eval(`function() { document.querySelector('#research-fixture').remove(); }`)
		}()

		state, err := client.evaluateSnapshotScript(ctx, snapshotStateJS)
		if err != nil {
			t.Fatalf("snapshot research state: %v", err)
		}
		if !state.ResearchWorkflow {
			t.Fatalf("explicit research marker was not detected: %+v", state)
		}
		if err := validateSupportedWorkflow(state); !errors.Is(err, errUnsupportedResearchWorkflow) {
			t.Fatalf("research workflow validation error = %v", err)
		}

		if _, err := page.Context(ctx).Eval(`function() {
		  document.querySelector('#research-fixture').remove();
		  document.querySelector('main form').insertAdjacentHTML(
		    'beforeend',
		    '<button id="research-fixture" data-testid="deep-research-toggle" aria-pressed="true">Deep research</button>'
		  );
		}`); err != nil {
			t.Fatalf("install selected-research fixture: %v", err)
		}
		state, err = client.evaluateSnapshotScript(ctx, snapshotStateJS)
		if err != nil {
			t.Fatalf("snapshot selected research state: %v", err)
		}
		if !state.ResearchWorkflow {
			t.Fatalf("selected research control was not detected: %+v", state)
		}
	})

	t.Run("contenteditable composer draft is replaced", func(t *testing.T) {
		composer, err := page.Context(ctx).Element("#contenteditable-test")
		if err != nil {
			t.Fatalf("find contenteditable fixture: %v", err)
		}
		if err := replaceComposerText(composer, "new prompt"); err != nil {
			t.Fatalf("replace contenteditable draft: %v", err)
		}
		got, err := composerText(composer)
		if err != nil {
			t.Fatalf("read contenteditable fixture: %v", err)
		}
		if got != "new prompt" {
			t.Fatalf("composer text = %q, want %q", got, "new prompt")
		}
	})

	t.Run("ProseMirror composer preserves multiline prompt structure", func(t *testing.T) {
		composer, err := page.Context(ctx).Element("#contenteditable-test")
		if err != nil {
			t.Fatalf("find contenteditable fixture: %v", err)
		}
		if _, err := composer.Context(ctx).Eval(`function() {
		  this.className = 'ProseMirror';
		  const render = value => {
		    const text = String(value).replace(/\r\n?/g, '\n');
		    const fragment = document.createDocumentFragment();
		    for (const line of text.split('\n')) {
		      const paragraph = document.createElement('p');
		      if (line === '') {
		        const placeholder = document.createElement('br');
		        placeholder.className = 'ProseMirror-trailingBreak';
		        paragraph.append(placeholder);
		      } else {
		        // The live ProseMirror editor represents leading ordinary spaces
		        // as NBSP nodes so indentation survives HTML layout.
		        paragraph.textContent = line.replace(/^( +)/, spaces => '\u00a0'.repeat(spaces.length));
		      }
		      fragment.append(paragraph);
		    }
		    this.replaceChildren(fragment);
		  };
		  this.addEventListener('beforeinput', event => {
		    if (event.inputType !== 'insertText' || typeof event.data !== 'string') return;
		    event.preventDefault();
		    this.dataset.insertWasFocused = String(document.activeElement === this);
		    render(event.data);
		  });
		}`); err != nil {
			t.Fatalf("install ProseMirror fixture behavior: %v", err)
		}

		tests := []struct {
			name string
			text string
			want string
		}{
			{name: "LF", text: "Alpha\nBeta", want: "Alpha\nBeta"},
			{name: "CRLF", text: "Alpha\r\nBeta", want: "Alpha\nBeta"},
			{name: "blank line", text: "Alpha\n\nBeta", want: "Alpha\n\nBeta"},
			{name: "Markdown", text: "## Heading\n\n- first\n- second", want: "## Heading\n\n- first\n- second"},
			{name: "indented JSON", text: "{\n  \"messages\": [\n    {}\n  ]\n}", want: "{\n  \"messages\": [\n    {}\n  ]\n}"},
			{name: "leading and trailing newlines", text: "\nAlpha\nBeta\n", want: "\nAlpha\nBeta\n"},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				if err := replaceComposerText(composer.Context(ctx), test.text); err != nil {
					state, _ := composer.Context(ctx).Eval(`function() {
					  return JSON.stringify({html: this.innerHTML, innerText: this.innerText, textContent: this.textContent});
					}`)
					t.Fatalf("replace composer text: %v; DOM state: %s", err, state.Value.Str())
				}
				focused, err := composer.Attribute("data-insert-was-focused")
				if err != nil || focused == nil || *focused != "true" {
					t.Fatalf("composer was not focused during scoped insertion: value=%v error=%v", focused, err)
				}
				got, err := composerText(composer.Context(ctx))
				if err != nil {
					t.Fatalf("read composer text: %v", err)
				}
				if normalizeComposerText(got) != normalizeComposerText(test.want) {
					t.Fatalf("composer text = %q, want structural text %q", got, test.want)
				}
				decoy, err := page.Context(ctx).Element("#composer-decoy")
				if err != nil {
					t.Fatalf("find composer decoy: %v", err)
				}
				decoyText, err := composerText(decoy)
				if err != nil {
					t.Fatalf("read composer decoy: %v", err)
				}
				if decoyText != "decoy draft" {
					t.Fatalf("scoped composer input changed decoy to %q", decoyText)
				}
			})
		}
	})

	t.Run("file input is scoped to the unique composer", func(t *testing.T) {
		composer, err := resolveComposerOnce(page, ctx, true)
		if err != nil {
			t.Fatalf("resolve fixture composer: %v", err)
		}
		input := composer.FileInput
		if input == nil || input.GetContext().Err() != nil {
			t.Fatalf("hidden file input has an unusable context: element=%v error=%v", input, input.GetContext().Err())
		}
		id, err := input.Attribute("id")
		if err != nil || id == nil || *id != "hidden-file" {
			t.Fatalf("selected file input id = %v, error = %v; wanted composer input", id, err)
		}
		file := filepath.Join(t.TempDir(), "answer.pdf")
		if err := os.WriteFile(file, []byte("fixture"), 0o600); err != nil {
			t.Fatalf("write upload fixture: %v", err)
		}
		if err := input.SetFiles([]string{file}); err != nil {
			t.Fatalf("set hidden file input: %v", err)
		}
	})

	t.Run("attachment readiness uses exact per-chip names", func(t *testing.T) {
		composer, err := resolveComposerOnce(page, ctx, true)
		if err != nil {
			t.Fatalf("resolve fixture composer: %v", err)
		}
		state, err := readComposerAttachmentStateUnchecked(ctx, composer, composerAttachmentStateJS)
		if err != nil {
			t.Fatalf("read attachment state: %v", err)
		}
		if err := requireEmptyComposer(state); err == nil {
			t.Fatal("pre-existing attachment was accepted as a clean baseline")
		}
		if err := matchExpectedAttachments(state, []string{"report.pdf"}); err == nil {
			t.Fatal("substring filename matched old-report.pdf")
		}
		if _, err := page.Context(ctx).Eval(`function() {
		  const chip = document.querySelector('[data-testid="attachment-chip"]');
		  chip.setAttribute('data-file-name', 'answer.pdf');
		  chip.innerText = 'answer.pdf';
		}`); err != nil {
			t.Fatalf("update attachment fixture: %v", err)
		}
		state, err = readComposerAttachmentStateUnchecked(ctx, composer, composerAttachmentStateJS)
		if err != nil {
			t.Fatalf("read updated attachment state: %v", err)
		}
		if err := matchExpectedAttachments(state, []string{"answer.pdf"}); err != nil {
			t.Fatalf("exact attachment was not accepted: %v (state=%+v)", err, state)
		}
	})

	t.Run("no-form composer variant remains snapshot-compatible", func(t *testing.T) {
		if _, err := page.Context(ctx).Eval(`function() {
		  const form = document.querySelector('main form');
		  const textarea = document.createElement('textarea');
		  textarea.setAttribute('placeholder', 'Message ChatGPT');
		  form.querySelector('#prompt-textarea').replaceWith(textarea);
		  const root = document.createElement('div');
		  root.setAttribute('data-testid', 'chat-composer');
		  while (form.firstChild) root.appendChild(form.firstChild);
		  form.replaceWith(root);
		}`); err != nil {
			t.Fatalf("install no-form composer: %v", err)
		}
		if _, err := resolveComposerOnce(page, ctx, true); err != nil {
			t.Fatalf("resolve no-form composer: %v", err)
		}
		if _, err := client.evaluateSnapshotScript(ctx, snapshotStateJS); err != nil {
			t.Fatalf("snapshot with no-form composer: %v", err)
		}
	})

	t.Run("ambiguous composer roots are rejected", func(t *testing.T) {
		if _, err := page.Context(ctx).Eval(`function() {
		  document.body.insertAdjacentHTML('beforeend', '<form id="other-composer"><div contenteditable="true">other</div><button data-testid="send-button">Send</button></form>');
		}`); err != nil {
			t.Fatalf("add second composer: %v", err)
		}
		defer func() {
			_, _ = page.Context(ctx).Eval(`function() { document.querySelector('#other-composer').remove(); }`)
		}()
		if _, err := resolveComposerOnce(page, ctx, false); err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("ambiguous composer error = %v", err)
		}
	})
}
