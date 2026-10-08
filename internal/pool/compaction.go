package pool

import (
	"crypto/sha256"
	"time"
)

const (
	compactionGrace = time.Minute
	compactionTTL   = 15 * time.Minute
	compactionCap   = 4096
)

// SummaryProof identifies a complete, successful summary without retaining its
// text. The caller supplies a zero proof for incomplete or failed responses.
// Both a nonzero digest and a positive byte count are required.
type SummaryProof struct {
	Digest [32]byte
	Bytes  int
}

func (p SummaryProof) valid() bool { return p.Digest != [32]byte{} && p.Bytes > 0 }

// CompactionPhase describes request intent, not permission to move an account.
// Every ordinary rebalance safety gate still applies at a compaction boundary.
type CompactionPhase uint8

const (
	CompactionOrdinary CompactionPhase = iota
	CompactionSummary
	CompactionBoundary
)

// CompactionRequest contains only bounded evidence from the request classifier.
// Request hashes the logical model/system/messages; Prefix hashes the first user
// message. Both must be zero for identities derived from request content rather
// than a stable router header or metadata. Summary intent still freezes elective
// execution without a stable identity. Adoption is a candidate summary proof,
// not a confirmed boundary until the pool correlates it with a completed lease.
type CompactionRequest struct {
	Phase    CompactionPhase
	Request  [32]byte
	Prefix   [32]byte
	Adoption SummaryProof
}

type compactionKey struct {
	binding [32]byte
	request [32]byte
}

// A record is one summary generation or dispatched boundary fingerprint. The
// global cache is bounded independently of session/lease/affinity state; eviction
// can only forget an optimization. In-flight summary protection lives on the
// session instead, so eviction must never make a live summary movable.
type compactionRecord struct {
	key          compactionKey
	state        *sessionState
	source       string
	expires      time.Time
	started      time.Time // fixed preference grace, never renewed by completion
	prefix       [32]byte
	proof        SummaryProof
	summary      bool
	consumed     bool
	boundary     bool
	headerPrefix [32]byte
}

type compactionGate struct {
	allowSwitch bool
	allowCheck  bool
	forceCheck  bool
	summary     bool
	generation  *compactionRecord
	boundary    bool
	replay      bool
	adoption    *compactionRecord
	reason      string
}

// compactionFor runs under p.mu after emergency binding. It does not consume a
// boundary: a canceled acquisition must leave that opportunity available. A
// recognized summary never waits for drain or executes a prepared plan.
func (p *Pool) compactionFor(s *sessionState, key compactionKey, source string, req CompactionRequest, now time.Time) compactionGate {
	g := compactionGate{allowSwitch: true, allowCheck: true, reason: "usage-advantage"}
	a := s.compaction
	if a != nil && (a.source != source || !now.Before(a.expires)) {
		s.compaction = nil
		a = nil
	}
	stable := req.Request != [32]byte{}
	r := p.compactions[key]
	if r != nil && !now.Before(r.expires) {
		p.removeCompaction(r)
		r = nil
	}
	if req.Phase == CompactionSummary {
		g.summary, g.allowSwitch = true, false
		if stable {
			if r == nil {
				started := now
				// Several generations can belong to one attempt. Repeated hints
				// must not keep ordinary fallback frozen by renewing its grace.
				if a != nil && !a.consumed {
					started = a.started
				}
				r = p.addCompaction(s, key, source, now)
				r.summary, r.started, r.prefix = true, started, req.Prefix
				s.compaction = r
				g.forceCheck = true
			}
			if r.summary && !r.boundary && r.source == source {
				g.generation = r
			}
		}
		return g
	}
	if stable {
		g.replay = r != nil && r.boundary
		if req.Phase == CompactionBoundary {
			g.boundary, g.reason = true, "compaction-header"
			// A system prompt may change between retries. A repeated explicit
			// hint with the same compacted prefix is not another opportunity.
			if !g.replay && req.Prefix != [32]byte{} {
				for _, prior := range p.compactions {
					if prior.key.binding == key.binding && prior.boundary && prior.headerPrefix == req.Prefix && now.Before(prior.expires) {
						g.replay = true
						break
					}
				}
			}
		} else if a != nil && !a.consumed && a.proof.valid() && req.Adoption == a.proof &&
			req.Prefix != [32]byte{} && a.prefix != [32]byte{} && req.Prefix != a.prefix {
			g.boundary, g.reason = true, "compaction-inferred"
		}
	}
	if g.boundary || g.replay {
		g.boundary = true
		g.forceCheck = true
		g.allowCheck = s.pending != nil // a boundary cannot manufacture its own notice
		g.allowSwitch = !g.replay && s.pending != nil && s.pending.notified
		if !g.replay {
			g.adoption = a
		}
	} else if a != nil && now.Before(a.started.Add(compactionGrace)) {
		g.allowSwitch = false
	}
	if s.summaryInflight > 0 {
		g.allowSwitch = false
	}
	return g
}

// dispatchCompaction atomically consumes the opportunity with lease registration,
// even when revalidation or drain constraints made it stay on the source. Retries
// cannot turn a newly announced notice into a switch on that same boundary.
func (p *Pool) dispatchCompaction(l *Lease, key compactionKey, source string, req CompactionRequest, g compactionGate, now time.Time) {
	s := l.state
	if g.summary {
		l.summary, l.generation = true, g.generation
		s.summaryInflight++
	}
	if g.boundary && req.Request != [32]byte{} {
		if g.adoption != nil {
			g.adoption.consumed = true
		}
		r := p.compactions[key]
		if r == nil {
			r = p.addCompaction(s, key, source, now)
		}
		r.boundary = true
		if req.Phase == CompactionBoundary {
			r.headerPrefix = req.Prefix
		}
	}
	if l.Credential.ID != source {
		s.compaction = nil
	}
}

// completeSummary shares the release critical section with notice acknowledgment
// and the final drain signal. A late response may only prove its own generation
// on the original pin; it cannot revive consumed, expired, or evicted evidence.
func (p *Pool) completeSummary(l *Lease, proof SummaryProof, now time.Time) {
	if !l.summary {
		return
	}
	l.state.summaryInflight--
	r := l.generation
	if r != nil && l.state.compaction == r && p.compactions[r.key] == r &&
		r.source == l.Credential.ID && !r.consumed && now.Before(r.expires) && !r.proof.valid() && proof.valid() {
		r.proof = proof
		r.expires = now.Add(compactionTTL)
	}
}

func (p *Pool) addCompaction(s *sessionState, key compactionKey, source string, now time.Time) *compactionRecord {
	p.pruneCompactions(now)
	if len(p.compactions) >= compactionCap {
		var oldest *compactionRecord
		for _, r := range p.compactions {
			if oldest == nil || r.expires.Before(oldest.expires) {
				oldest = r
			}
		}
		p.removeCompaction(oldest)
	}
	r := &compactionRecord{key: key, state: s, source: source, expires: now.Add(compactionTTL)}
	p.compactions[key] = r
	return r
}

func (p *Pool) removeCompaction(r *compactionRecord) {
	if r.state.compaction == r {
		r.state.compaction = nil
	}
	delete(p.compactions, r.key)
}

func (p *Pool) pruneCompactions(now time.Time) {
	for _, r := range p.compactions {
		if !now.Before(r.expires) {
			p.removeCompaction(r)
		}
	}
}

func compactionRequestKey(binding string, req CompactionRequest) compactionKey {
	return compactionKey{binding: sha256.Sum256([]byte(binding)), request: req.Request}
}
