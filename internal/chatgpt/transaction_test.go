package chatgpt

import (
	"context"
	"errors"
	"testing"

	"github.com/go-rod/rod/lib/proto"
)

func TestSubmittedMessageIDFromRequest(t *testing.T) {
	event := &proto.NetworkRequestWillBeSent{Request: &proto.NetworkRequest{
		URL:    "https://chatgpt.com/backend-api/conversation",
		Method: "POST",
		PostData: `{"action":"next","messages":[` +
			`{"id":"11111111-2222-3333-4444-555555555555","author":{"role":"user"},` +
			`"content":{"content_type":"text","parts":["  exact prompt\r\nsecond line  "]}}]}`,
	}}

	got, ok := submittedMessageIDFromRequest(event, "exact prompt\nsecond line")
	if !ok || got != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("submittedMessageIDFromRequest() = %q, %v", got, ok)
	}
}

func TestSubmittedMessageIDAcceptsProseMirrorNBSPIndentation(t *testing.T) {
	event := &proto.NetworkRequestWillBeSent{Request: &proto.NetworkRequest{
		URL:    "https://chatgpt.com/backend-api/f/conversation",
		Method: "POST",
		PostData: `{"messages":[` +
			`{"id":"11111111-2222-3333-4444-555555555555","author":{"role":"user"},` +
			`"content":{"content_type":"text","parts":["{\n\u00a0\u00a0\"messages\": []\n}"]}}]}`,
	}}

	got, ok := submittedMessageIDFromRequest(event, "{\n  \"messages\": []\n}")
	if !ok || got != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("submittedMessageIDFromRequest() = %q, %v", got, ok)
	}
}

func TestSubmittedMessageIDRejectsUntrustedOrAmbiguousRequest(t *testing.T) {
	body := `{"messages":[` +
		`{"id":"11111111-2222-3333-4444-555555555555","author":{"role":"user"},"content":{"parts":["same"]}},` +
		`{"id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","author":{"role":"user"},"content":{"parts":["same"]}}]}`
	mismatchedNewest := `{"messages":[` +
		`{"id":"11111111-2222-3333-4444-555555555555","author":{"role":"user"},"content":{"parts":["same"]}},` +
		`{"id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","author":{"role":"user"},"content":{"parts":["different"]}}]}`
	for _, test := range []struct {
		name string
		url  string
		body string
	}{
		{name: "lookalike origin", url: "https://chatgpt.com.evil.invalid/backend-api/conversation", body: body},
		{name: "newest user mismatch", url: "https://chatgpt.com/backend-api/conversation", body: mismatchedNewest},
		{name: "lookalike endpoint", url: "https://chatgpt.com/backend-api/conversation-metadata", body: body},
		{name: "unrelated endpoint", url: "https://chatgpt.com/backend-api/accounts", body: body},
	} {
		t.Run(test.name, func(t *testing.T) {
			event := &proto.NetworkRequestWillBeSent{Request: &proto.NetworkRequest{
				URL: test.url, Method: "POST", PostData: test.body,
			}}
			if got, ok := submittedMessageIDFromRequest(event, "same"); ok || got != "" {
				t.Fatalf("submittedMessageIDFromRequest() = %q, %v; want rejection", got, ok)
			}
		})
	}
}

func TestSubmittedMessageIDUsesNewestOutgoingUserMessage(t *testing.T) {
	event := &proto.NetworkRequestWillBeSent{Request: &proto.NetworkRequest{
		URL:    "https://chatgpt.com/backend-api/conversation",
		Method: "POST",
		PostData: `{"messages":[` +
			`{"id":"11111111-2222-3333-4444-555555555555","author":{"role":"user"},"content":{"parts":["same"]}},` +
			`{"id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","author":{"role":"user"},"content":{"parts":["same"]}}]}`,
	}}

	got, ok := submittedMessageIDFromRequest(event, "same")
	if !ok || got != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Fatalf("submittedMessageIDFromRequest() = %q, %v", got, ok)
	}
}

func TestSubmittedMessageIDOnlyMatchesKnownTextParts(t *testing.T) {
	event := &proto.NetworkRequestWillBeSent{Request: &proto.NetworkRequest{
		URL:    "https://chatgpt.com/backend-api/conversation",
		Method: "POST",
		PostData: `{"messages":[{"id":"11111111-2222-3333-4444-555555555555","author":{"role":"user"},` +
			`"content":{"content_type":"text","parts":["different"]}}]}`,
	}}
	if got, ok := submittedMessageIDFromRequest(event, "text"); ok || got != "" {
		t.Fatalf("metadata string matched submitted prompt: %q, %v", got, ok)
	}

	event.Request.PostData = `{"messages":[{"id":"11111111-2222-3333-4444-555555555555","author":{"role":"user"},` +
		`"content":{"content_type":"multimodal_text","parts":[{"content_type":"file_asset_pointer","asset_pointer":"file-service://text"},""]}}]}`
	if got, ok := submittedMessageIDFromRequest(event, ""); !ok || got != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("explicit empty attachment caption was not matched: %q, %v", got, ok)
	}
}

func TestObservedSubmissionIDFailsClosedUntilCausalEvidence(t *testing.T) {
	transaction := &conversationTransaction{
		requiresNew: true,
		observer: &submissionObserver{
			result: make(chan submissionObservation, 1),
			done:   make(chan struct{}),
		},
	}
	if _, err := transaction.observedSubmissionID(context.Background()); !errors.Is(err, errConversationTransitionPending) {
		t.Fatalf("observedSubmissionID() error = %v, want pending", err)
	}
	transaction.observer.result <- submissionObservation{messageID: "11111111-2222-3333-4444-555555555555"}
	got, err := transaction.observedSubmissionID(context.Background())
	if err != nil || got != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("observedSubmissionID() = %q, %v", got, err)
	}
}

func TestRenderedSubmissionStartRequiresMatchingNewUserTurn(t *testing.T) {
	transaction := &conversationTransaction{
		prompt:              "exact prompt\nsecond line",
		beforeUserCount:     2,
		beforeUserMessageID: "11111111-2222-3333-4444-555555555555",
		observedMessageID:   "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
	}
	for _, test := range []struct {
		name  string
		state submittedTurnState
		want  bool
	}{
		{
			name:  "unchanged user count",
			state: submittedTurnState{UserCount: 2, Text: "exact prompt\nsecond line"},
		},
		{
			name:  "new unrelated user turn",
			state: submittedTurnState{UserCount: 3, Text: "different prompt"},
		},
		{
			name:  "matching user turn",
			state: submittedTurnState{UserCount: 3, MessageID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", Text: " exact prompt\r\nsecond line "},
			want:  true,
		},
		{
			name:  "matching ProseMirror indentation",
			state: submittedTurnState{UserCount: 3, MessageID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", Text: "exact prompt\nsecond\u00a0line"},
			want:  true,
		},
		{
			name: "virtualized count with a new matching message",
			state: submittedTurnState{
				UserCount: 2,
				MessageID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
				Text:      "exact prompt\nsecond line",
			},
			want: true,
		},
		{
			name: "same message id is not new",
			state: submittedTurnState{
				UserCount: 2,
				MessageID: "11111111-2222-3333-4444-555555555555",
				Text:      "exact prompt\nsecond line",
			},
		},
		{
			name: "unexpected attachment",
			state: submittedTurnState{
				UserCount: 3,
				MessageID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
				Text:      "exact prompt\nsecond line",
				Items:     [][]string{{"unexpected.txt"}},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := transaction.renderedSubmissionStarted(test.state); got != test.want {
				t.Fatalf("renderedSubmissionStarted() = %v, want %v", got, test.want)
			}
		})
	}

	withoutBaselineID := *transaction
	withoutBaselineID.beforeUserMessageID = ""
	withoutBaselineID.observedMessageID = ""
	hydratedOldTurn := submittedTurnState{
		UserCount: 3,
		MessageID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Text:      "exact prompt\nsecond line",
	}
	if withoutBaselineID.renderedSubmissionStarted(hydratedOldTurn) {
		t.Fatal("hydrating existing history without a baseline id must not acknowledge a submission")
	}
	newChat := withoutBaselineID
	newChat.requiresNew = true
	newChat.observedMessageID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if !newChat.renderedSubmissionStarted(hydratedOldTurn) {
		t.Fatal("a matching first user turn should acknowledge a verified new-chat submission")
	}

	withAttachment := *transaction
	withAttachment.attachments = []string{"report.pdf"}
	matchingUpload := submittedTurnState{
		UserCount: 3,
		MessageID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Text:      "exact prompt\nsecond line",
		Items:     [][]string{{"report.pdf"}},
	}
	if !withAttachment.renderedSubmissionStarted(matchingUpload) {
		t.Fatal("a new matching user turn with the exact attachment set should acknowledge an upload")
	}
	matchingUpload.Items = nil
	if withAttachment.renderedSubmissionStarted(matchingUpload) {
		t.Fatal("an upload without the expected rendered attachment must not be acknowledged")
	}
}
