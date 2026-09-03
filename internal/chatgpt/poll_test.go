package chatgpt

import (
	"errors"
	"testing"
	"time"
)

func controlledPoll(before turnMarker) (*poller, *time.Time) {
	poll := newPoll(before)
	now := time.Unix(1, 0)
	poll.now = func() time.Time { return now }
	return poll, &now
}

func TestPollerRequiresNewAssistantTurn(t *testing.T) {
	t.Parallel()

	poll, _ := controlledPoll(turnMarker{AssistantCount: 2, LastAssistantID: "assistant-2"})
	old := snapshot{
		AssistantCount:     2,
		LastAssistantID:    "assistant-2",
		HasResponseContent: true,
		TerminalSignal:     true,
		ContentVersion:     "old-v1",
		ResponseMarkdown:   "previous answer",
	}
	if poll.complete(old) {
		t.Fatal("poller accepted the pre-existing assistant answer")
	}
	if poll.complete(old) {
		t.Fatal("poller accepted a stable pre-existing answer")
	}
}

func TestPollerUsesAssistantIdentityWhenCountIsUnchanged(t *testing.T) {
	t.Parallel()

	poll, now := controlledPoll(turnMarker{AssistantCount: 2, LastAssistantID: "assistant-old"})
	snap := snapshot{
		AssistantCount:     2,
		LastAssistantID:    "assistant-new",
		HasResponseContent: true,
		TerminalSignal:     true,
		ContentVersion:     "new-v1",
		ResponseMarkdown:   "new answer",
	}
	if poll.complete(snap) {
		t.Fatal("poller accepted only one terminal observation")
	}
	*now = now.Add(time.Second)
	if !poll.complete(snap) {
		t.Fatal("poller did not accept a stable new assistant identity")
	}
}

func TestPollerWaitsForExplicitGenerationStateToClear(t *testing.T) {
	t.Parallel()

	poll, now := controlledPoll(turnMarker{AssistantCount: 1, LastAssistantID: "assistant-1"})
	generating := snapshot{
		AssistantCount:     2,
		LastAssistantID:    "assistant-2",
		IsGenerating:       true,
		HasResponseContent: true,
		TerminalSignal:     true,
		ContentVersion:     "partial-v1",
		ResponseMarkdown:   "partial",
	}
	if poll.complete(generating) {
		t.Fatal("poller accepted a response while the DOM reported generation")
	}

	complete := generating
	complete.IsGenerating = false
	complete.ContentVersion = "complete-v1"
	complete.ResponseMarkdown = "complete"
	if poll.complete(complete) {
		t.Fatal("poller accepted only one terminal observation after generation")
	}
	*now = now.Add(time.Second)
	if !poll.complete(complete) {
		t.Fatal("poller did not finish after stable explicit generation state")
	}
}

func TestPollerAcceptsThinkingAndReasoningInCompletedAnswer(t *testing.T) {
	t.Parallel()

	poll, now := controlledPoll(turnMarker{AssistantCount: 0})
	snap := snapshot{
		AssistantCount:     1,
		LastAssistantID:    "assistant-1",
		HasResponseContent: true,
		TerminalSignal:     true,
		ContentVersion:     "answer-v1",
		ResponseMarkdown:   "Thinking and reasoning are ordinary words in this answer.",
	}
	if poll.complete(snap) {
		t.Fatal("poller accepted only one terminal observation")
	}
	*now = now.Add(time.Second)
	if !poll.complete(snap) {
		t.Fatal("answer vocabulary was incorrectly treated as generation state")
	}
}

func TestPollerDoesNotUseContentStabilityAsCompletion(t *testing.T) {
	t.Parallel()

	poll, _ := controlledPoll(turnMarker{AssistantCount: 0})
	snap := snapshot{
		AssistantCount:     1,
		LastAssistantID:    "assistant-1",
		HasResponseContent: true,
		ContentVersion:     "content-v1",
		ResponseMarkdown:   "same-length text",
	}
	for range 20 {
		if poll.complete(snap) {
			t.Fatal("poller used repeated response content as a completion signal")
		}
	}
}

func TestPollerRejectsResearchWorkflow(t *testing.T) {
	t.Parallel()

	poll, now := controlledPoll(turnMarker{AssistantCount: 0})
	snap := snapshot{
		AssistantCount:     1,
		LastAssistantID:    "assistant-research",
		HasResponseContent: true,
		TerminalSignal:     true,
		ResearchWorkflow:   true,
		ContentVersion:     "interim-v1",
	}
	for range 3 {
		*now = now.Add(10 * time.Second)
		if poll.complete(snap) {
			t.Fatal("poller accepted an explicitly detected research workflow")
		}
	}
	if sameCompletedSnapshot(snap, snap) {
		t.Fatal("research snapshots passed final completion verification")
	}
	if err := validateSupportedWorkflow(snap); !errors.Is(err, errUnsupportedResearchWorkflow) {
		t.Fatalf("research workflow validation error = %v", err)
	}
}

func TestPollerResetsAfterFailedFinalVerification(t *testing.T) {
	poll, now := controlledPoll(turnMarker{AssistantCount: 0})
	finished := snapshot{
		AssistantCount:     1,
		LastAssistantID:    "assistant-1",
		HasResponseContent: true,
		TerminalSignal:     true,
		ContentVersion:     "v1",
	}
	if poll.complete(finished) {
		t.Fatal("first terminal observation completed")
	}
	*now = now.Add(time.Second)
	if !poll.complete(finished) {
		t.Fatal("second settled terminal observation did not complete")
	}
	poll.invalidate()
	if poll.complete(finished) {
		t.Fatal("one terminal observation completed after invalidation")
	}
}

func TestCompletedSnapshotsRequireSameContentVersion(t *testing.T) {
	first := snapshot{AssistantCount: 1, LastAssistantID: "assistant-1", ContentVersion: "v1"}
	second := snapshot{
		AssistantCount:     1,
		LastAssistantID:    "assistant-1",
		HasResponseContent: true,
		TerminalSignal:     true,
		ContentVersion:     "v2",
	}
	if sameCompletedSnapshot(first, second) {
		t.Fatal("content mutation passed final snapshot verification")
	}
}

func TestResponsePreservesMarkdownLayout(t *testing.T) {
	t.Parallel()

	want := "# Heading\n\nParagraph one.\n\n```go\nfmt.Println(\"thinking\")\n```"
	snap := snapshot{ResponseMarkdown: "\n" + want + "\n"}
	if got := snap.response(); got != want {
		t.Fatalf("response() = %q, want %q", got, want)
	}
}
