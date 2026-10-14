package project

import "github.com/hermes98761234/sgit/internal/gitx"

// queue is the pending first-parent commits of one mounted repo.
type queue struct {
	repo    string
	src     *gitx.Repo
	commits []gitx.FPCommit
	seqs    map[string]int64    // land sequence by commit SHA
	trees   map[string][]string // mount path -> subtree per commit
	pos     int
}

type key struct {
	hasSeq bool
	seq    int64
	when   int64
	repo   string
	pos    int
}

func (q *queue) key() key {
	c := q.commits[q.pos]
	s, ok := q.seqs[c.SHA]
	return key{hasSeq: ok, seq: s, when: c.CommitterUnix, repo: q.repo, pos: q.pos}
}

// less orders two queue heads: by land sequence when both have one,
// otherwise by committer date, then repo name, then first-parent position.
func less(a, b key) bool {
	if a.hasSeq && b.hasSeq {
		return a.seq < b.seq
	}
	if a.when != b.when {
		return a.when < b.when
	}
	if a.repo != b.repo {
		return a.repo < b.repo
	}
	return a.pos < b.pos
}

// next returns the queue whose head comes first, or nil when all are drained.
func next(qs []*queue) *queue {
	var best *queue
	for _, q := range qs {
		if q.pos >= len(q.commits) {
			continue
		}
		if best == nil || less(q.key(), best.key()) {
			best = q
		}
	}
	return best
}
