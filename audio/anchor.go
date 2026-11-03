package audio

import "fmt"

// RealtimeAnchor selects which frame of an AirPlay 2 realtime (type 96)
// anchor packet is played at the packet's network time.
//
// The packet holds frame_1 and frame_2; frame_2 - frame_1 is the sender's
// stream latency, normally 77175 samples (1.75 s).
type RealtimeAnchor int

const (
	// AnchorShairport plays frame_1 - 11025 at the network time, as
	// shairport-sync does. Every receiver in a multi-room group, HomePods
	// included, then plays the same sample at the same moment.
	AnchorShairport RealtimeAnchor = iota
	// AnchorLowLatency plays frame_2 - 11025 at the network time: about
	// 1.75 s earlier. It suits video played on the sender, but this receiver
	// is then out of step with other AirPlay receivers in a group.
	AnchorLowLatency
)

// realtimeAnchorLead is the 250 ms (11025 sample) allowance for network
// jitter which shairport-sync subtracts from the anchor frame.
const realtimeAnchorLead = 11025

func ParseRealtimeAnchor(value string) (RealtimeAnchor, error) {
	switch value {
	case "shairport":
		return AnchorShairport, nil
	case "low-latency":
		return AnchorLowLatency, nil
	}
	return 0, fmt.Errorf("realtime anchor must be shairport or low-latency")
}

// SetRealtimeAnchor selects the realtime anchor mode. Call before Run.
func (p *Player) SetRealtimeAnchor(mode RealtimeAnchor) { p.realtimeAnchor = mode }

// realtimeAnchorStamp returns the RTP frame to play at the anchor packet's
// network time.
func realtimeAnchorStamp(frame1, frame2 uint32, mode RealtimeAnchor) uint32 {
	reference := frame1
	if mode == AnchorLowLatency {
		// Ignore an implausible latency rather than jump by it.
		if latency := int32(frame2 - frame1); latency > 0 && latency < cdRate*4 {
			reference = frame2
		}
	}
	return reference - realtimeAnchorLead
}
