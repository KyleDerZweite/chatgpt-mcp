package chatgpt

import "time"

var waits = []time.Duration{
	250 * time.Millisecond,
	500 * time.Millisecond,
	time.Second,
	2 * time.Second,
}

type turnMarker struct {
	AssistantCount  int
	LastAssistantID string
}

type poller struct {
	before              turnMarker
	checks              int
	candidate           string
	terminalConsecutive int
	terminalSince       time.Time
	now                 func() time.Time
	idx                 int
}

func newPoll(before turnMarker) *poller {
	return &poller{before: before, now: time.Now}
}

func (p *poller) wait() time.Duration {
	duration := waits[p.idx]
	if p.idx < len(waits)-1 {
		p.idx++
	}
	return duration
}

func (p *poller) complete(snap snapshot) bool {
	p.checks++
	if snap.ResearchWorkflow {
		p.reset("")
		return false
	}
	if !snap.hasNewAssistant(p.before) {
		p.reset("")
		return false
	}
	candidate := snap.marker().key() + "\x00" + snap.ContentVersion
	if candidate != p.candidate {
		p.reset(candidate)
	}
	if snap.IsGenerating || !snap.HasResponseContent || !snap.TerminalSignal {
		p.terminalConsecutive = 0
		p.terminalSince = time.Time{}
		return false
	}

	// Require the same assistant turn to expose an explicit terminal UI signal
	// twice. This filters short gaps between terminal-control appearance and the
	// final ordinary-response render without guessing from answer vocabulary.
	if p.terminalConsecutive == 0 {
		p.terminalSince = p.now()
	}
	p.terminalConsecutive++
	return p.terminalConsecutive >= 2 && p.now().Sub(p.terminalSince) >= time.Second
}

func (p *poller) invalidate() {
	p.terminalConsecutive = 0
	p.terminalSince = time.Time{}
}

func (p *poller) reset(candidate string) {
	p.candidate = candidate
	p.invalidate()
}

func sameCompletedSnapshot(first, second snapshot) bool {
	return first.marker().key() == second.marker().key() &&
		first.ContentVersion != "" && first.ContentVersion == second.ContentVersion &&
		!first.ResearchWorkflow && !second.ResearchWorkflow &&
		!second.IsGenerating && second.HasResponseContent && second.TerminalSignal
}
