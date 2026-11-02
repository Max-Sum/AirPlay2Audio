package audio

import "time"

// realtimeReorder holds at most 32 packets while a sequence gap is open.
// It does not change timestamps or decrypt data. While a gap is open it can
// ask the sender to resend the missing packets (see request), and it moves
// past the gap once wait has elapsed.
type realtimeReorder struct {
	next     uint16
	started  bool
	wait     time.Duration
	gapSince time.Time
	pending  map[uint16]*PCMFrame
	missing  uint64
	late     uint64

	// request, when set, asks the sender to resend count packets starting
	// at first. Each missing packet is requested at most maxRequests times,
	// at least retry apart.
	request     func(first, count uint16)
	retry       time.Duration
	maxRequests int
	requested   map[uint16]resendRecord
	requests    uint64 // packets requested, including retries
	recovered   uint64 // requested packets that arrived in time
}

type resendRecord struct {
	last  time.Time
	count int
}

func newRealtimeReorder(wait time.Duration) *realtimeReorder {
	return &realtimeReorder{wait: wait, pending: make(map[uint16]*PCMFrame), requested: make(map[uint16]resendRecord)}
}

// reset forgets the stream position, for example after a FLUSH.
func (q *realtimeReorder) reset() {
	clear(q.pending)
	clear(q.requested)
	q.started = false
	q.gapSince = time.Time{}
}

// drain emits contiguous packets, or moves past a missing range after its
// deadline. Signed subtraction keeps the sequence order across uint16 wrap.
func (q *realtimeReorder) drain(now time.Time, force bool, emit func(*PCMFrame)) {
	for {
		if f := q.pending[q.next]; f != nil {
			delete(q.pending, q.next)
			q.next++
			q.gapSince = time.Time{}
			emit(f)
			continue
		}
		if len(q.pending) == 0 {
			q.gapSince = time.Time{}
			q.forgetPassed()
			return
		}
		if q.gapSince.IsZero() {
			q.gapSince = now
		}
		if !force && now.Sub(q.gapSince) < q.wait {
			q.requestMissing(now)
			return
		}
		nearest := uint16(32767)
		for seq := range q.pending {
			if distance := uint16(seq - q.next); distance < nearest {
				nearest = distance
			}
		}
		q.missing += uint64(nearest)
		q.next += nearest
		q.gapSince = time.Time{}
	}
}

// requestMissing asks for every packet between next and the farthest
// pending packet that has not arrived, coalescing neighbours into ranges.
func (q *realtimeReorder) requestMissing(now time.Time) {
	q.forgetPassed()
	if q.request == nil {
		return
	}
	var farthest uint16
	for seq := range q.pending {
		if distance := uint16(seq - q.next); distance > farthest {
			farthest = distance
		}
	}
	var first, count uint16
	flush := func() {
		if count > 0 {
			q.request(first, count)
			q.requests += uint64(count)
			count = 0
		}
	}
	for distance := uint16(0); distance < farthest; distance++ {
		seq := q.next + distance
		if q.pending[seq] != nil {
			flush()
			continue
		}
		record, seen := q.requested[seq]
		if seen && (record.count >= q.maxRequests || now.Sub(record.last) < q.retry) {
			flush()
			continue
		}
		q.requested[seq] = resendRecord{last: now, count: record.count + 1}
		if count == 0 {
			first = seq
		}
		count++
	}
	flush()
}

// forgetPassed drops resend records for packets the stream has moved past.
func (q *realtimeReorder) forgetPassed() {
	for seq := range q.requested {
		if int16(seq-q.next) < 0 {
			delete(q.requested, seq)
		}
	}
}

// deadline reports when drain must run next without a new packet: the end
// of the current gap's wait, or an earlier resend retry.
func (q *realtimeReorder) deadline() (time.Time, bool) {
	if q.gapSince.IsZero() {
		return time.Time{}, false
	}
	at := q.gapSince.Add(q.wait)
	if q.request != nil {
		for seq, record := range q.requested {
			if record.count < q.maxRequests && q.pending[seq] == nil && int16(seq-q.next) >= 0 {
				if retry := record.last.Add(q.retry); retry.Before(at) {
					at = retry
				}
			}
		}
	}
	return at, true
}

func (q *realtimeReorder) push(f *PCMFrame, now time.Time, emit func(*PCMFrame)) {
	seq := uint16(f.SequenceNumber)
	if !q.started {
		q.next, q.started = seq, true
	}
	distance := int16(seq - q.next)
	if distance < 0 {
		q.late++
		return
	}
	if distance > 32 {
		q.drain(now, true, emit)
		distance = int16(seq - q.next)
		if distance < 0 {
			q.late++
			return
		}
		if distance > 32 {
			q.missing += uint64(distance)
			q.next = seq
			q.forgetPassed()
		}
	}
	if _, exists := q.pending[seq]; exists {
		q.late++
		return
	}
	if _, asked := q.requested[seq]; asked {
		delete(q.requested, seq)
		q.recovered++
	}
	q.pending[seq] = f
	q.drain(now, false, emit)
}
