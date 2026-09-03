package chatgpt

import "testing"

func TestNormalizeComposerText(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "LF", input: "Alpha\nBeta", want: "Alpha\nBeta"},
		{name: "CRLF", input: "Alpha\r\nBeta", want: "Alpha\nBeta"},
		{name: "blank line", input: "Alpha\n\nBeta", want: "Alpha\n\nBeta"},
		{name: "Markdown", input: "## Heading\r\n\r\n- first\r\n- second", want: "## Heading\n\n- first\n- second"},
		{name: "ProseMirror NBSP indentation", input: "{\n\u00a0\u00a0\"messages\": [\n\u00a0\u00a0\u00a0\u00a0{}\n]", want: "{\n  \"messages\": [\n    {}\n]"},
		{name: "leading and trailing newlines", input: "\r\nAlpha\nBeta\r\n", want: "Alpha\nBeta"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := normalizeComposerText(test.input); got != test.want {
				t.Fatalf("normalizeComposerText(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestExpectedAttachmentsRejectUnexpectedFileInputEntries(t *testing.T) {
	state := composerAttachmentState{
		Items:      [][]string{{"answer.pdf"}},
		InputNames: []string{"answer.pdf", "secret.txt"},
	}
	if err := matchExpectedAttachments(state, []string{"answer.pdf"}); err == nil {
		t.Fatal("attachment check accepted an extra native file-input entry")
	}
}

func TestExpectedAttachmentsAllowClearedNativeFileInput(t *testing.T) {
	state := composerAttachmentState{Items: [][]string{{"answer.pdf"}}}
	if err := matchExpectedAttachments(state, []string{"answer.pdf"}); err != nil {
		t.Fatalf("attachment check rejected a valid rendered chip after input reset: %v", err)
	}
}
