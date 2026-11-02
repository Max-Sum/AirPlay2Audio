package ptp

import (
	"testing"
	"time"
)

func TestOffsetAndPathDelay(t *testing.T) {
	m := newMeasurements()
	base := time.Unix(1000, 0)
	m.addSync(7, base.Add(100*time.Millisecond))
	m.addFollowUp(7, base)
	m.addDelayReq(7, base.Add(200*time.Millisecond))
	m.addDelayResp(7, base.Add(210*time.Millisecond))
	r, err := m.latest()
	if err != nil {
		t.Fatal(err)
	}
	if r.Offset != 45*time.Millisecond || r.Delay != 55*time.Millisecond {
		t.Fatalf("offset=%s delay=%s", r.Offset, r.Delay)
	}
}

func TestVirtualClockAppliesOffset(t *testing.T) {
	c := NewVirtualClock(0)
	if c.Synchronized() {
		t.Fatal("new clock must not be synchronized")
	}
	before := c.Now()
	c.Offset(3 * time.Second)
	after := c.Now()
	if !c.Synchronized() || after.Sub(before) < -3100*time.Millisecond || after.Sub(before) > -2900*time.Millisecond {
		t.Fatalf("unexpected clock correction: %s", after.Sub(before))
	}
}

func TestMeasurementsKeepIncompleteAndMatchSequence(t *testing.T) {
	m := newMeasurements()
	base := time.Unix(1000, 0)
	m.addSync(1, base.Add(100*time.Millisecond))
	m.addFollowUp(1, base)
	m.addDelayReq(1, base.Add(200*time.Millisecond))
	// A newer sync must not be mixed with the older response.
	m.addSync(2, base.Add(2*time.Second))
	m.addFollowUp(2, base.Add(time.Second))
	m.addDelayReq(2, base.Add(2100*time.Millisecond))
	m.addDelayResp(1, base.Add(210*time.Millisecond))
	r, err := m.latest()
	if err != nil || r.Offset != 45*time.Millisecond {
		t.Fatalf("wrong matched offset: %+v, %v", r, err)
	}
	m.addDelayResp(2, base.Add(1110*time.Millisecond))
	r, err = m.latest()
	if err != nil || r.Offset != 995*time.Millisecond {
		t.Fatalf("incomplete exchange was lost or mismatched: %+v, %v", r, err)
	}
}

func TestPruneAcrossSequenceWrap(t *testing.T) {
	m := newMeasurements()
	m.addSync(65000, time.Now())
	m.addSync(65535, time.Now())
	m.addSync(0, time.Now())
	m.prune(2)
	if _, ok := m.serverToClient[65000]; ok {
		t.Fatal("old exchange was not removed")
	}
	if len(m.serverToClient) != 2 {
		t.Fatal("recent exchange removed across wrap")
	}
}

func TestLocalClockUnaffectedByCorrections(t *testing.T) {
	c := NewVirtualClock(0)
	before := c.LocalNow()
	c.SetLocalOffset(time.Second)
	after := c.LocalNow()
	if after.Sub(before) < 0 || after.Sub(before) > 100*time.Millisecond {
		t.Fatalf("local clock stepped: %s", after.Sub(before))
	}
	if delta := c.Now().Sub(c.LocalNow()); delta > -900*time.Millisecond || delta < -1100*time.Millisecond {
		t.Fatalf("absolute correction not applied: %s", delta)
	}
}

func TestRejectIsolatedLargeOffsetAndFollowRealClockChange(t *testing.T) {
	c := NewVirtualClock(0)
	c.SetLocalOffset(time.Second)
	c.SetLocalOffset(2 * time.Second)
	if delta := c.Now().Sub(c.LocalNow()); delta > -900*time.Millisecond || delta < -1100*time.Millisecond {
		t.Fatalf("isolated outlier stepped clock: %s", delta)
	}
	c.SetLocalOffset(2 * time.Second)
	c.SetLocalOffset(2 * time.Second)
	if delta := c.Now().Sub(c.LocalNow()); delta > -1900*time.Millisecond || delta < -2100*time.Millisecond {
		t.Fatalf("consistent new clock ignored: %s", delta)
	}
}
