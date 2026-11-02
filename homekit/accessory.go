package homekit

import (
	"encoding/hex"
	"fmt"
	"goplay2/globals"
	"reflect"
	"strings"
	"sync"

	"github.com/google/uuid"
)

type UUID struct {
	uuid.UUID
}

type Accessory struct {
	Srcvers   string
	Deviceid  string
	Features  globals.Features
	flags     string
	model     string
	protovers string
	acl       string
	rsf       string
	Pi        UUID
	Gid       UUID
	Psi       UUID
	gcgl      string
	igl       string
	pk        string
	vv        string
	pw        string
}

// recordsMu guards the fields that change while the receiver runs (group
// and status flags); the mDNS responder reads them from its own goroutine.
var recordsMu sync.RWMutex

// Status flag bits (the "flags" TXT record and statusFlags in /info).
const (
	StatusAudioLink     = 1 << 2  // always set by shairport-sync
	StatusSessionActive = 1 << 17 // ReceiverSessionIsActive
)

// SetPasswordRequired sets the pw TXT record, which makes senders ask the
// user for a password before connecting.
func (t *Accessory) SetPasswordRequired(required bool) {
	recordsMu.Lock()
	defer recordsMu.Unlock()
	t.pw = "false"
	if required {
		t.pw = "true"
	}
}

func (t *Accessory) PasswordRecord() string {
	recordsMu.RLock()
	defer recordsMu.RUnlock()
	return t.pw
}

// SetGroup publishes the multi-room group of the sender that owns the
// output, as shairport-sync does after SETUP: gid is the sender's groupUUID
// and gcgl tells whether that group contains a group leader. An empty gid
// returns to the receiver's own identity (no group).
func (t *Accessory) SetGroup(gid string, leader bool) {
	recordsMu.Lock()
	defer recordsMu.Unlock()
	t.Gid = t.Pi
	if parsed, err := uuid.Parse(gid); err == nil {
		t.Gid = UUID{parsed}
	}
	t.gcgl = "0"
	if leader && gid != "" {
		t.gcgl = "1"
	}
}

// SetSessionActive sets or clears ReceiverSessionIsActive in the status flags.
func (t *Accessory) SetSessionActive(active bool) {
	recordsMu.Lock()
	defer recordsMu.Unlock()
	flags := uint64(StatusAudioLink)
	if active {
		flags |= StatusSessionActive
	}
	t.flags = fmt.Sprintf("0x%x", flags)
}

// StatusFlags returns the advertised status flags.
func (t *Accessory) StatusFlags() uint64 {
	recordsMu.RLock()
	defer recordsMu.RUnlock()
	var flags uint64
	fmt.Sscanf(t.flags, "0x%x", &flags)
	return flags
}

// GroupID returns the advertised group UUID.
func (t *Accessory) GroupID() string {
	recordsMu.RLock()
	defer recordsMu.RUnlock()
	return t.Gid.String()
}

func NewAccessory(deviceId string, currentUuid string, features globals.Features) *Accessory {
	var deviceUuid = uuid.MustParse(currentUuid)
	return &Accessory{
		Srcvers:   "366.0",
		Deviceid:  deviceId,
		Features:  features,
		flags:     "0x4",
		model:     "GoPlay2",
		protovers: "1.1",
		acl:       "0",
		rsf:       "0x0",
		Pi:        UUID{deviceUuid},
		Gid:       UUID{deviceUuid},
		Psi:       UUID{deviceUuid},
		gcgl:      "0",
		igl:       "0",
		pk:        "",
		vv:        "2",
		pw:        "false",
	}
}

// The advertised key must match the persisted HomeKit accessory identity.
func (t *Accessory) SetPublicKey(key []byte) { t.pk = hex.EncodeToString(key) }

func (t *Accessory) Model() string { return t.model }

func (t *Accessory) PublicKey() []byte {
	key, _ := hex.DecodeString(t.pk)
	return key
}

// TXTData is the wire encoding used for the txtAirPlay /info qualifier.
// Its records are exactly those published via mDNS.
func (t *Accessory) TXTData() []byte {
	var txt []byte
	for _, record := range t.ToRecords() {
		if len(record) <= 255 {
			txt = append(txt, byte(len(record)))
			txt = append(txt, record...)
		}
	}
	return txt
}

func (t *Accessory) String() string {
	return fmt.Sprintf("Pi: %s, guid: %s, Psi: %s", t.Pi, t.Gid, t.Psi)
}

func (uid UUID) ToRecord() string {
	return uid.String()
}

func (t *Accessory) ToRecords() []string {
	recordsMu.RLock()
	defer recordsMu.RUnlock()

	fields := reflect.TypeOf(*t)
	values := reflect.ValueOf(*t)

	numField := values.NumField()
	results := make([]string, numField)

	for i := 0; i < numField; i++ {
		results[i] = strings.ToLower(fields.Field(i).Name) + "="
		value := values.Field(i)
		switch fields.Field(i).Type.Name() {
		case "string":
			results[i] += value.String()
		case "UUID":
			results[i] += value.Interface().(UUID).ToRecord()
		case "Features":
			results[i] += value.Interface().(globals.Features).ToRecord()
		default:
			panic(fields.Field(i).Type.Name())
		}
	}
	return results

}

var Device *Accessory
