package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"chatgpt-mcp/internal/config"
)

const (
	testConversationID      = "11111111-2222-3333-4444-555555555555"
	otherTestConversationID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

func TestModelMatchesVerifiedUILabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		requested string
		actual    string
		want      bool
	}{
		{requested: "gpt-5", actual: "GPT-5", want: true},
		{requested: "gpt-5-thinking", actual: "Current model: GPT-5 Thinking", want: true},
		{requested: "gpt-4-1", actual: "GPT-4.1", want: true},
		{requested: "o3", actual: "Model selector o3", want: true},
		{requested: "gpt-5", actual: "GPT-5 Thinking", want: false},
		{requested: "gpt-5-pro", actual: "GPT-5", want: false},
		{requested: "gpt-5", actual: "", want: false},
		{requested: "-", actual: "Model selector", want: false},
	}
	for _, test := range tests {
		test := test
		t.Run(test.requested+"/"+test.actual, func(t *testing.T) {
			t.Parallel()
			if got := modelMatches(test.requested, test.actual); got != test.want {
				t.Fatalf("modelMatches(%q, %q) = %t, want %t", test.requested, test.actual, got, test.want)
			}
		})
	}
}

func TestMachineReadableModelIDIsAuthoritative(t *testing.T) {
	t.Parallel()

	if modelStateMatches("gpt-5", modelState{ID: "auto", Label: "GPT-5"}) {
		t.Fatal("visible label overrode a mismatched machine-readable model id")
	}
	if !modelStateMatches("gpt-5", modelState{ID: "gpt-5", Label: "Auto"}) {
		t.Fatal("matching machine-readable model id was not accepted")
	}
	if got := (modelState{ID: "gpt-5", Label: "Auto"}).display(); got != "gpt-5" {
		t.Fatalf("verified model display = %q, want authoritative id", got)
	}
}

func TestModelStateMatchesAnyVerifiedUILabelCandidate(t *testing.T) {
	t.Parallel()

	state := modelState{
		Label:  "Current model: GPT-5 Pro",
		Labels: []string{"Current model: GPT-5 Pro", "Pro"},
	}
	if !modelStateMatches("gpt-5-pro", state) {
		t.Fatal("a verified Pro selector label was not recognized")
	}
	if modelStateMatches("gpt-5", state) {
		t.Fatal("a Pro selector label was accepted as plain GPT-5")
	}
	state.ID = "auto"
	if modelStateMatches("gpt-5-pro", state) {
		t.Fatal("visible label overrode a mismatched machine-readable model id")
	}
}

func TestConversationURLPathEscapesIdentifier(t *testing.T) {
	t.Parallel()

	if got, want := conversationURL("abc/def"), "https://chatgpt.com/c/abc%2Fdef"; got != want {
		t.Fatalf("conversationURL() = %q, want %q", got, want)
	}
}

func TestConversationIDRequiresExactValidRoute(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
		want string
	}{
		{name: "conversation", url: "https://chatgpt.com/c/" + testConversationID, want: testConversationID},
		{name: "query and fragment", url: "https://chatgpt.com/c/" + testConversationID + "?model=gpt-5#response", want: testConversationID},
		{name: "canonicalizes id case", url: "https://chatgpt.com/c/AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE", want: otherTestConversationID},
		{name: "query substring", url: "https://chatgpt.com/?next=/c/" + testConversationID},
		{name: "path prefix", url: "https://chatgpt.com/g/custom/c/" + testConversationID},
		{name: "trailing path", url: "https://chatgpt.com/c/" + testConversationID + "/share"},
		{name: "trailing slash", url: "https://chatgpt.com/c/" + testConversationID + "/"},
		{name: "invalid suffix", url: "https://chatgpt.com/c/" + testConversationID + "x"},
		{name: "short id", url: "https://chatgpt.com/c/12345678-abcd"},
		{name: "wrong origin", url: "https://evil.example/c/" + testConversationID},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, ok := conversationIDFromURL(test.url)
			if test.want == "" {
				if ok || got != "" {
					t.Fatalf("conversationIDFromURL(%q) = %q, %t; want no conversation", test.url, got, ok)
				}
				return
			}
			if !ok || got != test.want {
				t.Fatalf("conversationIDFromURL(%q) = %q, %t; want %q, true", test.url, got, ok, test.want)
			}
		})
	}
}

func TestValidateRestoredConversation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		saved clientState
		url   string
		state snapshot
		ok    bool
	}{
		{name: "empty state", url: "https://chatgpt.com/", state: snapshot{}, ok: true},
		{name: "existing conversation", saved: clientState{chatID: testConversationID}, url: "https://chatgpt.com/c/" + testConversationID, state: snapshot{TurnCount: 2, AssistantCount: 1}, ok: true},
		{name: "active generation", url: "https://chatgpt.com/", state: snapshot{IsGenerating: true}},
		{name: "empty state redirected", url: "https://chatgpt.com/c/" + testConversationID, state: snapshot{TurnCount: 2, AssistantCount: 1}},
		{name: "query substring is not a conversation", url: "https://chatgpt.com/?next=/c/" + testConversationID, state: snapshot{}, ok: true},
		{name: "empty URL with stale turns", url: "https://chatgpt.com/", state: snapshot{TurnCount: 1}},
		{name: "wrong existing conversation", saved: clientState{chatID: testConversationID}, url: "https://chatgpt.com/c/" + otherTestConversationID, state: snapshot{TurnCount: 2, AssistantCount: 1}},
		{name: "dirty state must restore blank", saved: clientState{dirty: true, chatID: testConversationID}, url: "https://chatgpt.com/c/" + testConversationID, state: snapshot{TurnCount: 2, AssistantCount: 1}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateRestoredConversation(test.saved, test.url, test.state)
			if test.ok && err != nil {
				t.Fatalf("validateRestoredConversation() error = %v", err)
			}
			if !test.ok && err == nil {
				t.Fatal("validateRestoredConversation() unexpectedly succeeded")
			}
		})
	}
}

func TestModelSlugRequiresAlphanumericBoundaries(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"-", ".", "-gpt-5", "gpt-5-", strings.Repeat("a", 65)} {
		if modelRe.MatchString(value) {
			t.Errorf("model slug %q was accepted", value)
		}
	}
	for _, value := range []string{"o3", "gpt-5", "gpt-4.1"} {
		if !modelRe.MatchString(value) {
			t.Errorf("model slug %q was rejected", value)
		}
	}
}

func TestZeroTimeoutUsesDefault(t *testing.T) {
	t.Parallel()

	client := &Client{cfg: &config.Config{DefaultTimeoutMinutes: 30, MaxTimeoutMinutes: 120}}
	if got, want := client.timeout(0), 30*time.Minute; got != want {
		t.Fatalf("timeout(0) = %s, want %s", got, want)
	}
	if got, want := client.timeout(999), 120*time.Minute; got != want {
		t.Fatalf("timeout(999) = %s, want %s", got, want)
	}
}

func TestInvalidTimeoutConfigurationStillProducesPositiveBoundedDuration(t *testing.T) {
	t.Parallel()

	client := &Client{cfg: &config.Config{DefaultTimeoutMinutes: -1, MaxTimeoutMinutes: -1}}
	if got, want := client.timeout(0), 30*time.Minute; got != want {
		t.Fatalf("timeout with invalid config = %s, want %s", got, want)
	}
	if got, want := client.timeout(int(^uint(0)>>1)), 120*time.Minute; got != want {
		t.Fatalf("huge timeout = %s, want %s", got, want)
	}
}

func TestAskRejectsEmptyPromptBeforeUsingBrowser(t *testing.T) {
	t.Parallel()

	client := &Client{}
	_, err := client.Ask(context.Background(), " \n\t", "", 0)
	if err == nil {
		t.Fatal("Ask accepted an empty prompt")
	}
}

func TestSleepContextHonorsCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepContext(ctx, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("sleepContext error = %v, want context.Canceled", err)
	}
}

func TestConfiguredSubmissionDelayHonorsCancellation(t *testing.T) {
	t.Parallel()

	client := &Client{cfg: &config.Config{DelayMs: int(time.Minute / time.Millisecond)}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.waitBeforeSubmission(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitBeforeSubmission error = %v, want context.Canceled", err)
	}
}

func TestAskResultFormattingMetadataIsInternal(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(AskResult{
		Response:          "**formatted**",
		RawResponse:       "formatted",
		ResponseFormatted: true,
	})
	if err != nil {
		t.Fatalf("marshal AskResult: %v", err)
	}
	if strings.Contains(string(encoded), "RawResponse") || strings.Contains(string(encoded), "ResponseFormatted") ||
		strings.Contains(string(encoded), "raw_response") || strings.Contains(string(encoded), "response_formatted") {
		t.Fatalf("internal response metadata leaked into JSON: %s", encoded)
	}
}

func TestQuarantineBlocksFurtherMutatingCallsUntilNewChat(t *testing.T) {
	t.Parallel()

	client := &Client{}
	client.quarantineAfterMutation(errors.New("request canceled"))
	if err := client.requireClean(); err == nil || !strings.Contains(err.Error(), "chatgpt_new_chat") {
		t.Fatalf("requireClean error = %v, want new-chat recovery instruction", err)
	}
}
