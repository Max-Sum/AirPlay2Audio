package ptp

import (
	"fmt"
	"time"
)

type mData struct {
	sendTS    time.Time
	receiveTS time.Time
}

type MeasurementResult struct {
	Delay              time.Duration
	Offset             time.Duration
	ServerToClientDiff time.Duration
	ClientToServerDiff time.Duration
	Timestamp          time.Time
}

type measures struct {
	currentUTCoffset time.Duration
	serverToClient   map[uint16]*mData
	clientToServer   map[uint16]*mData
}

// addSync stores ts and seq of SYNC packet
func (m *measures) addSync(seq uint16, ts time.Time) {
	v, found := m.serverToClient[seq]
	if found {
		v.receiveTS = ts
	} else {
		m.serverToClient[seq] = &mData{receiveTS: ts}
	}
}

// addFollowUp stores ts and seq of FOLLOW_UP packet
func (m *measures) addFollowUp(seq uint16, ts time.Time) {
	v, found := m.serverToClient[seq]
	if found {
		v.sendTS = ts
	} else {
		m.serverToClient[seq] = &mData{sendTS: ts}
	}
}

// addDelayReq stores ts and seq of DELAY_REQ packet
func (m *measures) addDelayReq(seq uint16, ts time.Time) {
	v, found := m.clientToServer[seq]
	if found {
		v.sendTS = ts
	} else {
		m.clientToServer[seq] = &mData{sendTS: ts}
	}
}

// addDelayResp stores ts and seq of DELAY_RESP packet and updates history with latest measures
func (m *measures) addDelayResp(seq uint16, ts time.Time) {

	v, found := m.clientToServer[seq]
	if found {
		v.receiveTS = ts
	} else {
		m.clientToServer[seq] = &mData{receiveTS: ts}
	}
}

// Match both directions by sequence. Retain incomplete exchanges because UDP
// packets may arrive out of order.
func (m *measures) latest() (*MeasurementResult, error) {
	var lastServerToClient *mData
	var lastClientToServer *mData
	var selected uint16
	for seq, v := range m.serverToClient {
		reverse := m.clientToServer[seq]
		if reverse == nil || v.receiveTS.IsZero() || v.sendTS.IsZero() || reverse.receiveTS.IsZero() || reverse.sendTS.IsZero() {
			continue
		}
		if lastServerToClient == nil || reverse.sendTS.After(lastClientToServer.sendTS) {
			selected = seq
			lastServerToClient = v
			lastClientToServer = reverse
		}
	}
	if lastServerToClient == nil {
		return nil, fmt.Errorf("no complete PTP exchange yet")
	}

	delete(m.serverToClient, selected)
	delete(m.clientToServer, selected)

	clientToServerDiff := lastClientToServer.receiveTS.Sub(lastClientToServer.sendTS)
	serverToClientDiff := lastServerToClient.receiveTS.Sub(lastServerToClient.sendTS)
	delay := (clientToServerDiff + serverToClientDiff) / 2
	offset := serverToClientDiff - delay
	// or this expression of same formula
	// offset := (serverToClientDiff - clientToServerDiff)/2

	return &MeasurementResult{
		Delay:              delay,
		Offset:             offset,
		ServerToClientDiff: serverToClientDiff,
		ClientToServerDiff: clientToServerDiff,
		Timestamp:          lastClientToServer.receiveTS,
	}, nil
}

// Bound memory if packets are lost, allowing 256 recent sequence numbers.
// The signed difference preserves ordering across 16-bit wraparound.
func (m *measures) prune(seq uint16) {
	for old := range m.serverToClient {
		if int16(seq-old) > 256 {
			delete(m.serverToClient, old)
		}
	}
	for old := range m.clientToServer {
		if int16(seq-old) > 256 {
			delete(m.clientToServer, old)
		}
	}
}

func newMeasurements() *measures {
	return &measures{
		serverToClient: map[uint16]*mData{},
		clientToServer: map[uint16]*mData{},
	}
}
