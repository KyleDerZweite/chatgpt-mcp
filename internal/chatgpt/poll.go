package chatgpt

import "time"

var waits = []time.Duration{
	2 * time.Second, 3 * time.Second, 5 * time.Second, 8 * time.Second,
	13 * time.Second, 21 * time.Second, 30 * time.Second,
}

type poller struct {
	deadline time.Time
	checks   int
	lastLen  int
	lastText string
	stable   int
	idx      int
}

func newPoll(timeout time.Duration) *poller {
	return &poller{deadline: time.Now().Add(timeout)}
}

func (p *poller) expired() bool {
	return time.Now().After(p.deadline)
}

func (p *poller) wait() time.Duration {
	d := waits[p.idx]
	if p.idx < len(waits)-1 {
		p.idx++
	}
	return d
}

func (p *poller) complete(snap snapshot) bool {
	p.checks++
	text := snap.response()
	if text == "" {
		p.stable = 0
		return false
	}

	if len(text) == p.lastLen || text == p.lastText {
		p.stable++
	} else {
		p.stable = 0
	}
	p.lastLen = len(text)
	p.lastText = text

	threshold := 1
	if !snap.LastTurnCopy {
		threshold = 8
	}
	if snap.isThinking() {
		threshold = 30
	}
	return !snap.isThinking() && p.stable >= threshold
}
