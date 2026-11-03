package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Max-Sum/AirPlay2Audio/internal/zeroconf"
	"github.com/Max-Sum/AirPlay2Audio/audio"
	"github.com/Max-Sum/AirPlay2Audio/codec"
	"github.com/Max-Sum/AirPlay2Audio/config"
	"github.com/Max-Sum/AirPlay2Audio/event"
	"github.com/Max-Sum/AirPlay2Audio/globals"
	"github.com/Max-Sum/AirPlay2Audio/handlers"
	"github.com/Max-Sum/AirPlay2Audio/homekit"
	"github.com/Max-Sum/AirPlay2Audio/netutil"
	"github.com/Max-Sum/AirPlay2Audio/ptp"
	"github.com/Max-Sum/AirPlay2Audio/rtsp"
)

func main() {
	if err := run(); err != nil {
		log.Printf("ERROR: %v", err)
		os.Exit(1)
	}
}

// usableInterfaces returns the named interface, or with an empty name every
// active one, and their addresses. The first interface is the one the device
// ID is made from, as before. IPv4 link-local addresses are skipped; IPv6
// addresses, including link-local ones, are kept unless ipv4Only is set.
func usableInterfaces(name string, ipv4Only bool) ([]net.Interface, []string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, nil, err
	}
	var found []net.Interface
	var all []string
	for _, candidate := range interfaces {
		if name != "" && candidate.Name != name {
			continue
		}
		if candidate.Flags&net.FlagUp == 0 || candidate.Flags&net.FlagLoopback != 0 || len(candidate.HardwareAddr) != 6 {
			continue
		}
		addrs, err := candidate.Addrs()
		if err != nil {
			continue
		}
		var v4, v6 []string
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err != nil || ip.IsLoopback() {
				continue
			}
			if ip.To4() != nil {
				if !ip.IsLinkLocalUnicast() {
					v4 = append(v4, ip.String())
				}
			} else if !ipv4Only {
				v6 = append(v6, ip.String())
			}
		}
		if len(v4) > 0 || len(v6) > 0 && name != "" {
			found = append(found, candidate)
			all = append(all, append(v4, v6...)...)
		}
	}
	if len(found) == 0 {
		return nil, nil, fmt.Errorf("no active network interface with an IPv4 address (selected: %q)", name)
	}
	return found, all, nil
}

// advertiser republishes the TXT records when the group or session state
// changes. Updates are coalesced and sent from one goroutine, so the last
// state always wins.
type advertiser struct {
	update  chan struct{}
	publish func()
}

func newAdvertiser(publish func()) *advertiser {
	a := &advertiser{update: make(chan struct{}, 1), publish: publish}
	go func() {
		for range a.update {
			a.publish()
		}
	}()
	return a
}

func (a *advertiser) changed() {
	select {
	case a.update <- struct{}{}:
	default:
	}
}

// offsetMAC adds offset to a hardware address, so several receivers on one
// computer have different AirPlay device IDs.
func offsetMAC(mac net.HardwareAddr, offset int) net.HardwareAddr {
	var value uint64
	for _, b := range mac {
		value = value<<8 | uint64(b)
	}
	value = (value + uint64(offset)) & (1<<48 - 1)
	out := make(net.HardwareAddr, 6)
	for i := 5; i >= 0; i-- {
		out[i] = byte(value)
		value >>= 8
	}
	return out
}

type options struct {
	ifName, dataDir    string
	delay              int64
	synchronized       bool
	port, eventPort    int
	deviceIDOffset     int
	listDevices        bool
	driftCorrection    bool
	driftTolerance     float64
	resyncThreshold    float64
	statistics         bool
	monitor            bool
	sessionTimeout     int
	activeTimeout      int
	refuseInterruption bool
	volumeRange        float64
	realtimeAnchor     string
	anchorMode         audio.RealtimeAnchor
	hooks              map[string]*string
}

func parseFlags() (*options, error) {
	defaultDir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	o := &options{hooks: map[string]*string{}}
	flag.StringVar(&config.Config.DeviceName, "n", "AirPlay 2 Audio", "AirPlay receiver name")
	flag.StringVar(&o.ifName, "i", "", "network interface name; empty advertises on every active interface")
	flag.Int64Var(&o.delay, "delay", 0, "additional physical output latency compensation in milliseconds")
	flag.BoolVar(&o.synchronized, "sync", true, "schedule audio against the sender clock; -sync=false plays received audio immediately without synchronization")
	flag.StringVar(&o.dataDir, "data", filepath.Join(defaultDir, "AirPlay2Audio"), "private receiver identity directory; use a different one for each instance")
	flag.BoolVar(&globals.Trace, "trace", false, "log RTSP request headers and bodies for protocol debugging")
	flag.StringVar(&config.Config.Password, "pw", "", "password senders must enter to play; empty allows anyone on the network")

	// Network.
	flag.IntVar(&o.port, "port", 7000, "AirPlay RTSP TCP port")
	flag.IntVar(&o.eventPort, "event-port", 60003, "AirPlay 2 event TCP port")
	flag.IntVar(&netutil.PortBase, "udp-port-base", 0, "first port for audio data, control and timing sockets; 0 lets Windows choose")
	flag.IntVar(&netutil.PortRange, "udp-port-range", 100, "number of ports from -udp-port-base")
	flag.BoolVar(&netutil.IPv4Only, "ipv4only", false, "listen and advertise on IPv4 only")
	flag.IntVar(&o.deviceIDOffset, "device-id-offset", 0, "added to the hardware address that forms the AirPlay device ID; give each instance on one computer a different value")

	// Output.
	flag.StringVar(&codec.Output.Device, "output-device", "", "output device: index or part of the name from -list-devices; empty uses the Windows default device")
	flag.BoolVar(&o.listDevices, "list-devices", false, "list output devices and exit")
	flag.IntVar(&codec.Output.Bits, "output-bits", 16, "output sample size: 16, 24 or 32")
	flag.IntVar(&codec.Output.Rate, "output-rate", 44100, "output sample rate: 44100, 48000, 88200, 96000, 176400 or 192000")
	flag.StringVar(&codec.Output.Dither, "dither", "auto", "dither: auto (16-bit output with volume or rate conversion), on or off")
	flag.Float64Var(&o.volumeRange, "volume-range", audio.DefaultVolumeRange, "attenuation in dB at the sender's lowest volume step (shairport-sync's volume_range_db)")

	// Synchronization.
	flag.StringVar(&o.realtimeAnchor, "realtime-anchor", "shairport", "AirPlay 2 realtime stream timing: shairport (in step with other AirPlay receivers, as shairport-sync) or low-latency (about 1.75 s earlier; suits video on the sender, not multi-room)")
	flag.IntVar(&handlers.OutputLatencyMs, "output-latency", 0, "link latency in milliseconds reported to the sender (outputLatencyMicros in /info)")
	flag.BoolVar(&o.driftCorrection, "drift-correction", true, "correct clock drift by adding or removing single samples")
	flag.Float64Var(&o.driftTolerance, "drift-tolerance", 2, "timing error in milliseconds allowed before drift correction")
	flag.Float64Var(&o.resyncThreshold, "resync-threshold", 50, "timing error in milliseconds that makes playback jump instead of correcting")
	flag.BoolVar(&o.statistics, "statistics", false, "log synchronization and network statistics every 10 seconds")
	flag.BoolVar(&o.monitor, "monitor", false, "print a machine-readable \"@stats\" line every second on standard output (used by the window)")

	// Sessions.
	flag.IntVar(&o.sessionTimeout, "session-timeout", 120, "end a session after this many seconds without requests or audio; 0 never")
	flag.IntVar(&o.activeTimeout, "active-timeout", 10, "seconds after playback stops before the output device is released; 0 keeps it open")
	flag.BoolVar(&o.refuseInterruption, "refuse-interruption", false, "refuse a new sender while another is connected instead of switching to it")
	for _, name := range []string{"active-begin", "play-begin", "play-end", "active-end"} {
		o.hooks[name] = flag.String("on-"+name, "", "command to run on "+name+" (through cmd.exe; AIRPLAY_EVENT is set)")
	}
	flag.Parse()

	if o.delay < -500 || o.delay > 500 {
		return nil, fmt.Errorf("delay must be between -500 and 500 ms")
	}
	if handlers.OutputLatencyMs < 0 || handlers.OutputLatencyMs > 10000 {
		return nil, fmt.Errorf("output-latency must be between 0 and 10000 ms")
	}
	for _, p := range []struct {
		name  string
		value int
	}{{"port", o.port}, {"event-port", o.eventPort}} {
		if p.value <= 0 || p.value > 65535 {
			return nil, fmt.Errorf("-%s must be between 1 and 65535", p.name)
		}
	}
	if o.port == o.eventPort {
		return nil, fmt.Errorf("-port and -event-port must differ")
	}
	if netutil.PortBase < 0 || netutil.PortBase > 65535 || netutil.PortBase > 0 && (netutil.PortRange < 3 || netutil.PortBase+netutil.PortRange-1 > 65535) {
		return nil, fmt.Errorf("-udp-port-base and -udp-port-range must describe at least 3 valid ports")
	}
	if err := codec.Output.Validate(); err != nil {
		return nil, err
	}
	if o.driftTolerance <= 0 || o.resyncThreshold <= o.driftTolerance {
		return nil, fmt.Errorf("-drift-tolerance must be positive and below -resync-threshold")
	}
	if o.anchorMode, err = audio.ParseRealtimeAnchor(o.realtimeAnchor); err != nil {
		return nil, err
	}
	if o.volumeRange < 10 || o.volumeRange > 96 {
		return nil, fmt.Errorf("-volume-range must be between 10 and 96 dB")
	}
	if o.sessionTimeout < 0 || o.activeTimeout < 0 {
		return nil, fmt.Errorf("timeouts cannot be negative")
	}
	return o, nil
}

func runHook(line, event string) {
	if line == "" {
		return
	}
	cmd := hookCommand(line)
	cmd.Env = append(os.Environ(), "AIRPLAY_EVENT="+event)
	if err := cmd.Start(); err != nil {
		log.Printf("HOOK %s: %v", event, err)
		return
	}
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("HOOK %s: %v", event, err)
		}
	}()
}

func run() error {
	o, err := parseFlags()
	if err != nil {
		return err
	}
	if o.listDevices {
		devices, err := codec.Devices()
		if err != nil {
			return err
		}
		for _, d := range devices {
			fmt.Printf("DEVICE %d %s\n", d.Index, d.Name)
		}
		return nil
	}
	if strings.TrimSpace(config.Config.DeviceName) == "" {
		return fmt.Errorf("receiver name is empty")
	}
	if config.Config.Password == "" {
		config.Config.Password = os.Getenv("AIRPLAY_PASSWORD")
	}
	globals.ErrLog = log.New(os.Stderr, "", log.LstdFlags|log.Lmsgprefix)
	if err := config.Config.Load(o.dataDir); err != nil {
		return err
	}
	ifaces, ips, err := usableInterfaces(o.ifName, netutil.IPv4Only)
	if err != nil {
		return err
	}
	iFace := &ifaces[0]
	hardware := offsetMAC(iFace.HardwareAddr, o.deviceIDOffset)
	mac := strings.ToUpper(hardware.String())
	homekit.Device = homekit.NewAccessory(mac, config.Config.DeviceUUID, airplayDevice())
	homekit.Device.SetPasswordRequired(config.Config.Password != "")
	homekit.Server, err = homekit.NewServer(mac, config.Config.DeviceName, o.dataDir, ips)
	if err != nil {
		return fmt.Errorf("initialize receiver identity: %w", err)
	}
	homekit.Device.SetPublicKey(homekit.Server.Device.PublicKey())

	decoder := codec.NewAacDecoder()
	if err := decoder.InitRaw([]byte{0x12, 0x10}); err != nil {
		return fmt.Errorf("initialize AAC decoder: %w", err)
	}
	decoder.Close()
	clock := ptp.NewVirtualClock(o.delay)
	queueFrames := 4096
	if !o.synchronized {
		// Apply TCP backpressure rather than buffering tens of seconds of
		// future audio. Never discard queued frames to catch up.
		queueFrames = 16
	}
	player := audio.NewPlayer(clock, audio.NewRing(queueFrames))
	player.SetSynchronized(o.synchronized)
	player.SetSyncOptions(audio.SyncOptions{
		DriftCorrection: o.driftCorrection,
		Tolerance:       time.Duration(o.driftTolerance * float64(time.Millisecond)),
		Resync:          time.Duration(o.resyncThreshold * float64(time.Millisecond)),
	})
	player.SetActiveTimeout(time.Duration(o.activeTimeout) * time.Second)
	player.SetVolumeRange(o.volumeRange)
	player.SetRealtimeAnchor(o.anchorMode)
	player.OnEvent = func(event string) { runHook(*o.hooks[event], event) }
	if err := player.Init(); err != nil {
		return fmt.Errorf("initialize audio output: %w", err)
	}

	// PTP uses the fixed ports 319 and 320, which only one program can own.
	// Without them AirPlay 2 synchronized playback is unavailable, but
	// AirPlay 1 and unsynchronized playback still work.
	ptpServer := ptp.NewServer(clock)
	ptpServer.SetIdentity(ptp.ClockIdentityFromMAC(hardware))
	if err := ptpServer.Listen(); err != nil {
		log.Printf("WARNING: %v; AirPlay 2 timed playback needs these ports (is another receiver running?)", err)
	} else {
		defer ptpServer.Close()
		go ptpServer.Serve()
	}
	rtspListener, err := netutil.ListenFixed(o.port)
	if err != nil {
		return fmt.Errorf("AirPlay TCP %d: %w", o.port, err)
	}
	defer rtspListener.Close()
	eventListener, err := netutil.ListenFixed(o.eventPort)
	if err != nil {
		return fmt.Errorf("AirPlay event TCP %d: %w", o.eventPort, err)
	}
	defer eventListener.Close()
	handler, err := handlers.NewRstpHandler(o.dataDir, player, config.Config.Password)
	if err != nil {
		return fmt.Errorf("initialize pairing: %w", err)
	}
	handler.ClockSources = ptpServer.SetSources
	handler.TimingPeers = ptpServer.SetPeers
	handler.AnchorClock = ptpServer.PreferClock
	player.OnAnchorClock = ptpServer.PreferClock
	handler.HardwareAddr = hardware
	handler.EventPort = o.eventPort
	handler.RefuseInterruption = o.refuseInterruption
	handlers.MediaOutput = os.Stdout
	event.InfoProvider = handlers.InfoValue
	defer os.Remove(handlers.ArtworkPath)
	go player.Run()
	go event.Serve(eventListener)
	go func() {
		if err := rtsp.Serve(rtspListener, handler); err != nil {
			globals.ErrLog.Printf("AirPlay server: %v", err)
		}
	}()
	if o.sessionTimeout > 0 {
		go func() {
			timeout := time.Duration(o.sessionTimeout) * time.Second
			for range time.Tick(5 * time.Second) {
				handler.ExpireIdle(timeout)
			}
		}()
	}
	if o.statistics {
		go func() {
			last := time.Now()
			for now := range time.Tick(10 * time.Second) {
				if line, ok := player.Statistics(now.Sub(last)); ok {
					log.Print(line)
				}
				last = now
			}
		}()
	}
	if o.monitor {
		go func() {
			for range time.Tick(time.Second) {
				fmt.Fprintln(os.Stdout, player.Monitor())
			}
		}()
	}

	host := "airplay2-" + strings.ReplaceAll(config.Config.DeviceUUID, "-", "")[:12]
	// Without -i the receiver is advertised on every active interface, as
	// shairport-sync does through Avahi. Each interface then answers with its
	// own addresses (zeroconf does that when no address list is given); with
	// -ipv4only the IPv4 list is given so no IPv6 address is published.
	advertisedIPs := ips
	if o.ifName == "" && !netutil.IPv4Only {
		advertisedIPs = nil
	}
	service, err := zeroconf.RegisterProxy(config.Config.DeviceName, "_airplay._tcp", "local.", o.port, host, advertisedIPs, homekit.Device.ToRecords(), ifaces)
	if err != nil {
		return fmt.Errorf("advertise AirPlay device: %w", err)
	}
	defer service.Shutdown()
	// RAOP record, as published by shairport-sync. AirPlay 1 senders find
	// the receiver through it, and iOS reads the audio capabilities from it.
	raopName := strings.ReplaceAll(mac, ":", "") + "@" + config.Config.DeviceName
	raop, err := zeroconf.RegisterProxy(raopName, "_raop._tcp", "local.", o.port, host, advertisedIPs, raopRecords(), ifaces)
	if err != nil {
		return fmt.Errorf("advertise RAOP service: %w", err)
	}
	defer raop.Shutdown()
	// Group and session changes (SETUP, sender switch, end of session) are
	// published again, as shairport-sync's mdns_update.
	adv := newAdvertiser(func() {
		service.SetText(homekit.Device.ToRecords())
		raop.SetText(raopRecords())
	})
	handler.SetAdvertise(adv.changed)
	names := make([]string, len(ifaces))
	for i := range ifaces {
		names[i] = ifaces[i].Name
	}
	log.Printf("READY name=%q interface=%q addresses=%s port=%d password=%t", config.Config.DeviceName, strings.Join(names, ","), strings.Join(ips, ","), o.port, config.Config.Password != "")
	stop := make(chan struct{}, 1)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "stop" {
				stop <- struct{}{}
				return
			}
			if gain, ok := parseLocalVolume(line); ok {
				player.ControlChannel <- globals.ControlMessage{MType: globals.LOCAL_VOLUME, Paramf: gain}
				log.Printf("AUDIO local volume %.0f%%", gain*100)
			}
			if name, command, ok := parseMediaCommand(line); ok {
				event.LogCommand(name, command)
			}
		}
	}()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	select {
	case <-stop:
	case <-signals:
	}
	log.Print("Stopping receiver")
	// Remember the sender's last volume as the next session's initial volume.
	if err := config.Config.Store(); err != nil {
		log.Printf("save configuration: %v", err)
	}
	return nil
}

// parseMediaCommand reads "media next" and similar console commands.
func parseMediaCommand(line string) (string, uint64, bool) {
	fields := strings.Fields(line)
	if len(fields) != 2 || fields[0] != "media" {
		return "", 0, false
	}
	commands := map[string]uint64{
		"play":     event.CommandPlay,
		"pause":    event.CommandPause,
		"toggle":   event.CommandToggle,
		"stop":     event.CommandStop,
		"next":     event.CommandNext,
		"previous": event.CommandPrevious,
	}
	command, ok := commands[fields[1]]
	return fields[1], command, ok
}

func parseLocalVolume(line string) (float64, bool) {
	fields := strings.Fields(line)
	if len(fields) != 2 || fields[0] != "volume" {
		return 0, false
	}
	percent, err := strconv.Atoi(fields[1])
	if err != nil || percent < 0 || percent > 100 {
		return 0, false
	}
	return float64(percent) / 100, true
}
