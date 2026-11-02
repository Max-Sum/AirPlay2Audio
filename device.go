package main

import (
	"encoding/hex"
	"fmt"

	"github.com/Max-Sum/AirPlay2Audio/globals"
	"github.com/Max-Sum/AirPlay2Audio/homekit"
)

// airplayDevice returns the advertised feature bits. They follow
// shairport-sync, whose volume control works with current iOS senders.
func airplayDevice() globals.Features {
	var features = globals.NewFeatures().Set(globals.SupportsAirPlayAudio).Set(globals.AudioRedundant)
	features = features.Set(globals.HasUnifiedAdvertiserInfo).Set(globals.SupportsBufferedAudio)
	features = features.Set(globals.SupportsUnifiedMediaControl)
	features = features.Set(globals.SupportsHKPairingAndAccessControl).Set(globals.SupportsHKPeerManagement)
	features = features.Set(globals.Authentication_4)
	features = features.Set(globals.SupportsPTP)
	// Transient pairing (bit 48): senders do a PIN-less SRP pair-setup per
	// session instead of a full HomeKit pairing.
	features = features.Set(globals.SupportsCoreUtilsPairingAndEncryption)
	features = features.Set(globals.AudioFormats_0).Set(globals.AudioFormats_1).Set(globals.AudioFormats_2)
	// Artwork, progress and text metadata, which the window shows in the
	// Windows media controls. With these, iOS also uses buffered AAC for
	// Apple Music. The metadata arrives as SET_PARAMETER.
	features = features.Set(globals.MetadataFeatures_0).Set(globals.MetadataFeatures_1).Set(globals.MetadataFeatures_2)
	return features
}

// raopRecords is the _raop._tcp TXT record published next to _airplay._tcp.
// iOS only sends volume changes to the receiver when this record exists.
// AirPlay 1 senders connect through it: et=1 offers RSA key exchange (as
// shairport-sync's classic mode), and ch/ss/sr describe the accepted audio.
func raopRecords() []string {
	return []string{
		"txtvers=1",
		"ch=2",
		"ss=16",
		"sr=44100",
		"cn=0,1",
		"da=true",
		"et=0,1,4",
		"ek=1",
		"sv=false",
		"ft=" + homekit.Device.Features.ToRecord(),
		"md=0,1,2",
		"am=" + homekit.Device.Model(),
		fmt.Sprintf("sf=0x%x", homekit.Device.StatusFlags()),
		"tp=UDP",
		"vn=65537",
		"vs=" + homekit.Device.Srcvers,
		"pk=" + hex.EncodeToString(homekit.Device.PublicKey()),
		"pw=" + homekit.Device.PasswordRecord(),
	}
}
