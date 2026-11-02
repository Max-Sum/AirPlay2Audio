package handlers

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Now playing information is passed to the window as "@media" lines on
// standard output. The window shows them in the Windows media controls and
// does not write them to the log, so media details stay out of log files.
//
//	@media meta <title> <artist> <album> <duration ms>   (text base64, "-" if empty)
//	@media progress <position ms> <duration ms>
//	@media state playing|paused
//	@media art <path base64>|-
var (
	MediaOutput io.Writer // nil disables the lines (tests, console use)
	ArtworkPath = filepath.Join(os.TempDir(), "AirPlay2Audio-artwork")
	mediaMu     sync.Mutex
	lastState   string
)

func emitMedia(fields ...string) {
	mediaMu.Lock()
	defer mediaMu.Unlock()
	if MediaOutput == nil {
		return
	}
	_, _ = fmt.Fprintln(MediaOutput, "@media "+strings.Join(fields, " "))
}

func mediaText(value string) string {
	if value == "" {
		return "-"
	}
	return base64.StdEncoding.EncodeToString([]byte(value))
}

// setPlaybackState reports playing or paused once per change.
func setPlaybackState(playing bool) {
	state := "paused"
	if playing {
		state = "playing"
	}
	mediaMu.Lock()
	changed := lastState != state
	lastState = state
	mediaMu.Unlock()
	if changed {
		emitMedia("state", state)
	}
}

// resetPlaybackState forgets the state, so a new session reports it again.
func resetPlaybackState() {
	mediaMu.Lock()
	lastState = ""
	mediaMu.Unlock()
}

type trackInfo struct {
	Title, Artist, Album string
	DurationMs           uint32
}

// parseTrackInfo reads the DMAP item fields a sender sends for the current
// track: minm title, asar artist, asal album and astm duration.
func parseTrackInfo(body []byte) (trackInfo, bool) {
	var info trackInfo
	found := false
	var walk func([]byte, int)
	walk = func(data []byte, depth int) {
		for len(data) >= 8 {
			tag := string(data[:4])
			size := binary.BigEndian.Uint32(data[4:8])
			if uint64(size) > uint64(len(data)-8) {
				return
			}
			value := data[8 : 8+size]
			switch tag {
			case "mlit":
				if depth < 3 {
					walk(value, depth+1)
				}
			case "minm":
				info.Title, found = cleanText(value), true
			case "asar":
				info.Artist, found = cleanText(value), true
			case "asal":
				info.Album, found = cleanText(value), true
			case "astm":
				if len(value) == 4 {
					info.DurationMs = binary.BigEndian.Uint32(value)
				}
			}
			data = data[8+size:]
		}
	}
	walk(body, 0)
	return info, found
}

func cleanText(value []byte) string {
	text := strings.Map(func(c rune) rune {
		if c < 0x20 || c == 0x7f || c == 0xfffd {
			return -1
		}
		return c
	}, string(value))
	if len(text) > 256 {
		text = text[:256]
	}
	return strings.TrimSpace(text)
}

// parseProgress reads "progress: start/current/end", RTP timestamps at
// 44.1 kHz, as position and duration in milliseconds.
func parseProgress(line string) (uint64, uint64, bool) {
	value := strings.TrimSpace(strings.TrimPrefix(line, "progress:"))
	parts := strings.Split(value, "/")
	if len(parts) != 3 {
		return 0, 0, false
	}
	var stamps [3]uint32
	for i, part := range parts {
		n, err := strconv.ParseUint(strings.TrimSpace(part), 10, 32)
		if err != nil {
			return 0, 0, false
		}
		stamps[i] = uint32(n)
	}
	// Unsigned subtraction handles RTP timestamp wraparound.
	position := uint64(stamps[1]-stamps[0]) * 1000 / 44100
	duration := uint64(stamps[2]-stamps[0]) * 1000 / 44100
	if position > duration {
		position = duration
	}
	return position, duration, true
}

func (r *Rstp) onTrackInfo(body []byte) {
	if info, ok := parseTrackInfo(body); ok {
		emitMedia("meta", mediaText(info.Title), mediaText(info.Artist), mediaText(info.Album), strconv.FormatUint(uint64(info.DurationMs), 10))
	}
}

func (r *Rstp) onProgress(line string) {
	if position, duration, ok := parseProgress(line); ok {
		emitMedia("progress", strconv.FormatUint(position, 10), strconv.FormatUint(duration, 10))
		// Senders send progress when playback starts or resumes.
		setPlaybackState(true)
	}
}

// onArtwork stores the cover in a file and passes its path; an empty body
// or image/none clears it. The file is replaced atomically, so the window
// never reads a partly written image.
func (r *Rstp) onArtwork(contentType string, body []byte) {
	if len(body) == 0 || strings.HasSuffix(contentType, "/none") {
		emitMedia("art", "-")
		return
	}
	temp := ArtworkPath + ".tmp"
	if err := os.WriteFile(temp, body, 0o600); err != nil {
		log.Printf("MEDIA artwork not saved: %v", err)
		return
	}
	if err := os.Rename(temp, ArtworkPath); err != nil {
		_ = os.Remove(temp)
		log.Printf("MEDIA artwork not saved: %v", err)
		return
	}
	emitMedia("art", mediaText(ArtworkPath))
}
