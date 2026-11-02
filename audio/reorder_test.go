package audio

import (
	"reflect"
	"testing"
	"time"
)

func TestRealtimeReorderOutOfOrderAndWrap(t *testing.T) {
	q := newRealtimeReorder(40 * time.Millisecond)
	now := time.Now()
	var got []uint32
	emit := func(f *PCMFrame) { got = append(got, f.SequenceNumber) }
	q.push(testPCM(0, 65534, 0, 0), now, emit)
	q.push(testPCM(0, 0, 0, 0), now, emit)
	q.push(testPCM(0, 65535, 0, 0), now.Add(time.Millisecond), emit)
	if !reflect.DeepEqual(got, []uint32{65534, 65535, 0}) || q.missing != 0 {
		t.Fatalf("reordered=%v missing=%d", got, q.missing)
	}
}

func TestRealtimeReorderDeadlineAndLatePacket(t *testing.T) {
	q := newRealtimeReorder(8 * time.Millisecond)
	now := time.Now()
	var got []uint32
	emit := func(f *PCMFrame) { got = append(got, f.SequenceNumber) }
	q.push(testPCM(0, 1, 0, 0), now, emit)
	q.push(testPCM(0, 3, 0, 0), now, emit)
	q.drain(now.Add(7*time.Millisecond), false, emit)
	if len(got) != 1 {
		t.Fatal("packet emitted before deadline")
	}
	q.drain(now.Add(8*time.Millisecond), false, emit)
	q.push(testPCM(0, 2, 0, 0), now.Add(9*time.Millisecond), emit)
	if !reflect.DeepEqual(got, []uint32{1, 3}) || q.missing != 1 || q.late != 1 {
		t.Fatalf("got=%v missing=%d late=%d", got, q.missing, q.late)
	}
}

type resendCall struct{ first, count uint16 }

func newResendReorder(calls *[]resendCall) *realtimeReorder {
	q := newRealtimeReorder(120 * time.Millisecond)
	q.retry, q.maxRequests = 40*time.Millisecond, 3
	q.request = func(first, count uint16) { *calls = append(*calls, resendCall{first, count}) }
	return q
}

func TestRealtimeReorderRequestsMissingRangesOnce(t *testing.T) {
	var calls []resendCall
	q := newResendReorder(&calls)
	now := time.Now()
	emit := func(*PCMFrame) {}
	q.push(testPCM(0, 65533, 0, 0), now, emit)
	// 65534, 65535 and 1 are missing; 0 arrived. Ranges cross the wrap.
	q.push(testPCM(0, 0, 0, 0), now, emit)
	q.push(testPCM(0, 2, 0, 0), now, emit)
	want := []resendCall{{65534, 2}, {1, 1}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("requests=%v want %v", calls, want)
	}
	q.drain(now.Add(10*time.Millisecond), false, emit)
	if len(calls) != 2 {
		t.Fatalf("re-requested before retry interval: %v", calls)
	}
	if at, ok := q.deadline(); !ok || !at.Equal(now.Add(40*time.Millisecond)) {
		t.Fatalf("deadline=%v ok=%v, want retry time", at, ok)
	}
}

func TestRealtimeReorderRetriesThenGivesUp(t *testing.T) {
	var calls []resendCall
	q := newResendReorder(&calls)
	now := time.Now()
	var got []uint32
	emit := func(f *PCMFrame) { got = append(got, f.SequenceNumber) }
	q.push(testPCM(0, 1, 0, 0), now, emit)
	q.push(testPCM(0, 3, 0, 0), now, emit)
	q.drain(now.Add(40*time.Millisecond), false, emit)
	q.drain(now.Add(80*time.Millisecond), false, emit)
	q.drain(now.Add(119*time.Millisecond), false, emit)
	if len(calls) != 3 {
		t.Fatalf("requests=%v, want three attempts", calls)
	}
	if at, _ := q.deadline(); !at.Equal(now.Add(120 * time.Millisecond)) {
		t.Fatalf("deadline=%v, want gap expiry", at)
	}
	q.drain(now.Add(120*time.Millisecond), false, emit)
	if !reflect.DeepEqual(got, []uint32{1, 3}) || q.missing != 1 || len(q.requested) != 0 {
		t.Fatalf("got=%v missing=%d records=%d", got, q.missing, len(q.requested))
	}
}

func TestRealtimeReorderRecoversResentPacket(t *testing.T) {
	var calls []resendCall
	q := newResendReorder(&calls)
	now := time.Now()
	var got []uint32
	emit := func(f *PCMFrame) { got = append(got, f.SequenceNumber) }
	q.push(testPCM(0, 1, 0, 0), now, emit)
	q.push(testPCM(0, 3, 0, 0), now, emit)
	q.push(testPCM(0, 2, 0, 0), now.Add(30*time.Millisecond), emit)
	if !reflect.DeepEqual(got, []uint32{1, 2, 3}) || q.missing != 0 || q.recovered != 1 || q.requests != 1 {
		t.Fatalf("got=%v missing=%d recovered=%d requests=%d", got, q.missing, q.recovered, q.requests)
	}
	if _, ok := q.deadline(); ok {
		t.Fatal("deadline set with no gap open")
	}
}

func TestRealtimeReorderLargeGapBounded(t *testing.T) {
	q := newRealtimeReorder(time.Second)
	now := time.Now()
	var got []uint32
	emit := func(f *PCMFrame) { got = append(got, f.SequenceNumber) }
	q.push(testPCM(0, 1, 0, 0), now, emit)
	q.push(testPCM(0, 3, 0, 0), now, emit)
	q.push(testPCM(0, 100, 0, 0), now, emit)
	if !reflect.DeepEqual(got, []uint32{1, 3, 100}) || q.missing != 97 || len(q.pending) != 0 {
		t.Fatalf("got=%v missing=%d pending=%d", got, q.missing, len(q.pending))
	}
}
