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
	for _, test := range []struct {
		name string
		url  string
		body string
	}{
		{name: "lookalike origin", url: "https://chatgpt.com.evil.invalid/backend-api/conversation", body: body},
		{name: "ambiguous ids", url: "https://chatgpt.com/backend-api/conversation", body: body},
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
