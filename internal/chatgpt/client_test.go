package chatgpt

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"chatgpt-mcp/internal/config"
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
		{name: "existing conversation", saved: clientState{chatID: "12345678-abcd"}, url: "https://chatgpt.com/c/12345678-abcd", state: snapshot{TurnCount: 2, AssistantCount: 1}, ok: true},
		{name: "active generation", url: "https://chatgpt.com/", state: snapshot{IsGenerating: true}},
		{name: "empty state redirected", url: "https://chatgpt.com/c/12345678-abcd", state: snapshot{TurnCount: 2, AssistantCount: 1}},
		{name: "empty URL with stale turns", url: "https://chatgpt.com/", state: snapshot{TurnCount: 1}},
		{name: "wrong existing conversation", saved: clientState{chatID: "12345678-abcd"}, url: "https://chatgpt.com/c/87654321-dcba", state: snapshot{TurnCount: 2, AssistantCount: 1}},
		{name: "dirty state must restore blank", saved: clientState{dirty: true, chatID: "12345678-abcd"}, url: "https://chatgpt.com/c/12345678-abcd", state: snapshot{TurnCount: 2, AssistantCount: 1}},
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

func TestQuarantineBlocksFurtherMutatingCallsUntilNewChat(t *testing.T) {
	t.Parallel()

	client := &Client{}
	client.quarantineAfterMutation(errors.New("request canceled"))
	if err := client.requireClean(); err == nil || !strings.Contains(err.Error(), "chatgpt_new_chat") {
		t.Fatalf("requireClean error = %v, want new-chat recovery instruction", err)
	}
}
