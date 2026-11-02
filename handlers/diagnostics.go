package handlers

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Max-Sum/AirPlay2Audio/internal/zeroconf"
	"github.com/Max-Sum/AirPlay2Audio/globals"
	"github.com/Max-Sum/AirPlay2Audio/rtsp"
	"howett.net/plist"
)

// Diagnostics for remote control and metadata, enabled with -trace. They
// show which channel a sender offers for commands sent back to it (DACP or
// MediaRemote) and which metadata it sends. Media values are never logged.

var (
	diagMu     sync.Mutex
	diagLast   = map[string]string{}
	dacpProbed sync.Map // DACP-ID -> true
)

// diagf logs a DIAG line unless the previous line for key was identical.
func diagf(key, format string, args ...interface{}) {
	line := fmt.Sprintf(format, args...)
	diagMu.Lock()
	same := diagLast[key] == line
	diagLast[key] = line
	diagMu.Unlock()
	if !same {
		log.Print("DIAG " + line)
	}
}

func headerValue(req *rtsp.Request, name string) string {
	if values, ok := req.Header[name]; ok && len(values) > 0 {
		return strings.TrimSpace(values[0])
	}
	return ""
}

// diagnoseRemoteControl reports the DACP headers. The DACP-ID names the
// sender's _dacp._tcp service; Active-Remote is the token which authorizes
// commands, so only its presence is logged.
func diagnoseRemoteControl(conn *rtsp.Conn, req *rtsp.Request) {
	if !globals.Trace {
		return
	}
	id := headerValue(req, "Dacp-Id")
	remote := headerValue(req, "Active-Remote") != ""
	if !isHex(id) {
		id = ""
	}
	diagf("dacp-"+string(req.Method), "dacp %s dacp-id=%q active-remote=%t", req.Method, id, remote)
	if id == "" {
		return
	}
	if _, seen := dacpProbed.LoadOrStore(id, true); seen {
		return
	}
	var sender net.IP
	if conn != nil && conn.NetConn() != nil {
		if host, _, err := net.SplitHostPort(conn.NetConn().RemoteAddr().String()); err == nil {
			sender = net.ParseIP(host)
		}
	}
	go browseDacp(id, sender)
}

// browseDacp looks for the sender's DACP service, iTunes_Ctrl_<DACP-ID>.
func browseDacp(id string, sender net.IP) {
	resolver, err := zeroconf.NewResolver(zeroconf.SelectIPTraffic(zeroconf.IPv4))
	if err != nil {
		log.Printf("DIAG dacp browse error: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	entries := make(chan *zeroconf.ServiceEntry, 16)
	if err := resolver.Browse(ctx, "_dacp._tcp", "local.", entries); err != nil {
		log.Printf("DIAG dacp browse error: %v", err)
		return
	}
	want := "itunes_ctrl_" + strings.ToLower(id)
	found, others := false, 0
	for {
		select {
		case entry, ok := <-entries:
			if !ok {
				entries = nil
				continue
			}
			if entry == nil {
				continue
			}
			if strings.ToLower(entry.Instance) != want {
				others++
				continue
			}
			found = true
			fromSender := false
			for _, ip := range entry.AddrIPv4 {
				if sender != nil && ip.Equal(sender) {
					fromSender = true
				}
			}
			log.Printf("DIAG dacp service found port=%d addresses=%d matches-sender=%t", entry.Port, len(entry.AddrIPv4), fromSender)
		case <-ctx.Done():
			if !found {
				log.Printf("DIAG dacp service not found for this dacp-id (other _dacp._tcp services: %d)", others)
			}
			return
		}
	}
}

func isHex(value string) bool {
	if len(value) == 0 || len(value) > 32 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// diagnoseSupportedCommands lists the MediaRemote command numbers a sender
// offers in updateMRSupportedCommands. Each entry is a binary plist; the
// field names are logged once so the numbering can be identified.
func diagnoseSupportedCommands(entries [][]byte) {
	if !globals.Trace || len(entries) == 0 {
		return
	}
	var enabled, disabled []int
	keys := map[string]bool{}
	for _, raw := range entries {
		var entry map[string]interface{}
		if _, err := plist.Unmarshal(raw, &entry); err != nil {
			continue
		}
		number, hasNumber := -1, false
		on := true
		for key, value := range entry {
			keys[key] = true
			lower := strings.ToLower(key)
			switch {
			case strings.Contains(lower, "enabled"):
				if b, ok := value.(bool); ok {
					on = b
				}
			case strings.HasSuffix(lower, "commandkey") || lower == "command":
				if n, ok := toInt(value); ok {
					number, hasNumber = n, true
				}
			}
		}
		if !hasNumber {
			continue
		}
		if on {
			enabled = append(enabled, number)
		} else {
			disabled = append(disabled, number)
		}
	}
	sort.Ints(enabled)
	sort.Ints(disabled)
	diagf("mr-keys", "mr command fields %s", sortedKeys(keys))
	diagf("mr-commands", "mr commands count=%d enabled=%v disabled=%v", len(entries), enabled, disabled)
}

// diagnoseCommandFields shows the top-level and params field names of a
// POST /command, which identifies messages other than the command list.
func diagnoseCommandFields(body []byte) {
	if !globals.Trace {
		return
	}
	var content map[string]interface{}
	if _, err := plist.Unmarshal(body, &content); err != nil {
		return
	}
	top := map[string]bool{}
	for key := range content {
		top[key] = true
	}
	nested := map[string]bool{}
	if params, ok := content["params"].(map[string]interface{}); ok {
		for key := range params {
			nested[key] = true
		}
	}
	diagf("command-fields", "command fields %s params %s", sortedKeys(top), sortedKeys(nested))
}

// diagnoseParameter reports the kind of SET_PARAMETER body: its content
// type and, for text and DMAP bodies, the field names without values.
func diagnoseParameter(req *rtsp.Request) {
	if !globals.Trace {
		return
	}
	contentType := strings.ToLower(headerValue(req, "Content-Type"))
	switch {
	case strings.HasPrefix(contentType, "text/parameters"):
		names := map[string]bool{}
		for _, line := range strings.Split(string(req.Body), "\n") {
			if i := strings.Index(line, ":"); i > 0 && isProtocolIdentifier(strings.TrimSpace(line[:i])) {
				names[strings.TrimSpace(line[:i])] = true
			}
		}
		delete(names, "volume") // already logged as AUDIO volume
		if len(names) > 0 {
			diagf("param-text", "parameter text fields %s", sortedKeys(names))
		}
	case strings.Contains(contentType, "dmap"):
		diagf("param-dmap", "parameter dmap tags %s", strings.Join(dmapTags(req.Body), " "))
	case strings.HasPrefix(contentType, "image/"):
		diagf("param-image", "parameter artwork type=%s", sanitizeType(contentType))
	default:
		diagf("param-other", "parameter content-type=%s", sanitizeType(contentType))
	}
}

// dmapTags returns the distinct tags of a DMAP body, descending into the
// mlit container which wraps the item fields.
func dmapTags(body []byte) []string {
	seen := map[string]bool{}
	var walk func([]byte, int)
	walk = func(data []byte, depth int) {
		for len(data) >= 8 {
			tag := string(data[:4])
			size := binary.BigEndian.Uint32(data[4:8])
			if uint64(size) > uint64(len(data)-8) || !isProtocolIdentifier(tag) {
				return
			}
			seen[tag] = true
			if tag == "mlit" && depth < 3 {
				walk(data[8:8+size], depth+1)
			}
			data = data[8+size:]
		}
	}
	walk(body, 0)
	tags := make([]string, 0, len(seen))
	for tag := range seen {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return tags
}

func sanitizeType(value string) string {
	value = strings.Map(func(c rune) rune {
		if c < 0x20 || c == 0x7f {
			return -1
		}
		return c
	}, value)
	if len(value) > 64 {
		value = value[:64]
	}
	if value == "" {
		return "(none)"
	}
	return value
}

func sortedKeys(keys map[string]bool) string {
	list := make([]string, 0, len(keys))
	for key := range keys {
		if isProtocolIdentifier(key) {
			list = append(list, key)
		}
	}
	sort.Strings(list)
	return "[" + strings.Join(list, " ") + "]"
}

func toInt(value interface{}) (int, bool) {
	switch n := value.(type) {
	case uint64:
		return int(n), true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}
