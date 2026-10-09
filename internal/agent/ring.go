package agent

import "github.com/himanshubaliyan7/SandboxSchedulerAI/internal/agentapi"

// ring is a bounded replay buffer for one process. It stores the last
// `limit` bytes of output in a circular byte buffer together with a list
// of segments recording which channel each run of bytes came from.
//
// Offsets are one monotonic counter across all channels. Bytes in
// [base, next) are retained; everything below base has been dropped.
//
// Memory is bounded by limit bytes plus maxSegs segment headers: when
// channels alternate so quickly that the segment list would exceed
// maxSegs, the oldest segments (and their bytes) are dropped early.
//
// ring is not safe for concurrent use; the owning Process serialises
// access with its mutex.
type ring struct {
	limit   int
	maxSegs int

	buf   []byte // circular; grows lazily up to limit
	start int    // index in buf of the byte at offset base
	size  int    // bytes retained

	base int64 // offset of the oldest retained byte
	next int64 // offset the next byte will get

	segs []segment // ordered, contiguous, covering [base, next)
}

// segment is a run of bytes from a single channel starting at off. It
// ends where the next segment starts (or at ring.next for the last one).
type segment struct {
	off int64
	ch  agentapi.EventType
}

const (
	minRingBytes     = 16
	initialRingAlloc = 4 << 10
)

func newRing(limit int) *ring {
	if limit < minRingBytes {
		limit = minRingBytes
	}
	maxSegs := limit / 64
	if maxSegs < 64 {
		maxSegs = 64
	}
	return &ring{limit: limit, maxSegs: maxSegs}
}

// write appends p as output on channel ch.
func (r *ring) write(ch agentapi.EventType, p []byte) {
	if len(p) == 0 {
		return
	}
	if len(r.segs) == 0 || r.segs[len(r.segs)-1].ch != ch {
		r.segs = append(r.segs, segment{off: r.next, ch: ch})
	}
	total := int64(len(p))
	if len(p) >= r.limit {
		// Only the tail fits; everything retained so far is dropped.
		p = p[len(p)-r.limit:]
		r.start, r.size = 0, 0
		r.grow(r.limit)
		copy(r.buf, p)
		r.size = len(p)
		r.next += total
		r.base = r.next - int64(len(p))
		r.trimSegs()
		return
	}
	if over := r.size + len(p) - r.limit; over > 0 {
		r.drop(over)
	}
	r.grow(r.size + len(p))
	// Copy into the circular buffer, possibly in two parts.
	w := (r.start + r.size) % len(r.buf)
	n := copy(r.buf[w:], p)
	if n < len(p) {
		copy(r.buf, p[n:])
	}
	r.size += len(p)
	r.next += total
	r.trimSegs()
}

// grow makes the backing array at least need bytes, keeping contents.
func (r *ring) grow(need int) {
	if need <= len(r.buf) {
		return
	}
	n := len(r.buf) * 2
	if n < initialRingAlloc {
		n = initialRingAlloc
	}
	for n < need {
		n *= 2
	}
	if n > r.limit {
		n = r.limit
	}
	nb := make([]byte, n)
	r.copyOut(nb, r.base, r.size)
	r.buf = nb
	r.start = 0
}

// drop discards the n oldest bytes.
func (r *ring) drop(n int) {
	if n <= 0 {
		return
	}
	if n >= r.size {
		n = r.size
	}
	r.size -= n
	r.base += int64(n)
	if r.size == 0 {
		r.start = 0
	} else {
		r.start = (r.start + n) % len(r.buf)
	}
}

// trimSegs drops segments that lie wholly below base, enforces maxSegs,
// and moves the first segment's start up to base.
func (r *ring) trimSegs() {
	if extra := len(r.segs) - r.maxSegs; extra > 0 {
		// Dropping whole old segments drops their bytes too.
		r.drop(int(r.segs[extra].off - r.base))
	}
	i := 0
	for i+1 < len(r.segs) && r.segs[i+1].off <= r.base {
		i++
	}
	// Reslice rather than copy: O(1) per write. The dead prefix is freed
	// when append next reallocates, which copies only the live window, so
	// the backing array stays within a small multiple of maxSegs.
	r.segs = r.segs[i:]
	if len(r.segs) > 0 && r.segs[0].off < r.base {
		r.segs[0].off = r.base
	}
}

// copyOut copies n retained bytes starting at offset off into dst.
func (r *ring) copyOut(dst []byte, off int64, n int) {
	if n == 0 {
		return
	}
	i := (r.start + int(off-r.base)) % len(r.buf)
	c := copy(dst[:n], r.buf[i:])
	if c < n {
		copy(dst[c:n], r.buf)
	}
}

// read copies up to len(dst) bytes of a single channel starting at offset
// from. It returns the channel, the number of bytes copied, the offset of
// the first copied byte, and how many requested bytes were dropped before
// it. n == 0 means there is nothing at or after from yet.
//
// A from beyond next is treated as next.
func (r *ring) read(from int64, dst []byte) (ch agentapi.EventType, n int, off int64, dropped int64) {
	if from > r.next {
		from = r.next
	}
	if from < r.base {
		dropped = r.base - from
		from = r.base
	}
	off = from
	if from == r.next {
		return "", 0, off, dropped
	}
	// Find the segment containing from: the last one with seg.off <= from.
	lo, hi := 0, len(r.segs)
	for lo+1 < hi {
		mid := (lo + hi) / 2
		if r.segs[mid].off <= from {
			lo = mid
		} else {
			hi = mid
		}
	}
	end := r.next
	if lo+1 < len(r.segs) {
		end = r.segs[lo+1].off
	}
	n = len(dst)
	if int64(n) > end-from {
		n = int(end - from)
	}
	r.copyOut(dst, from, n)
	return r.segs[lo].ch, n, off, dropped
}

// release frees the retained bytes. Subsequent reads below next report
// them as dropped.
func (r *ring) release() {
	r.buf = nil
	r.start, r.size = 0, 0
	r.base = r.next
	r.segs = nil
}

// retained is the number of bytes of backing memory held.
func (r *ring) retained() int { return len(r.buf) }
