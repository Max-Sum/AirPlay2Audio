# AirPlay 2 audio receiver

English | [中文](README.zh-CN.md)

An AirPlay 2 and AirPlay 1 audio receiver for Windows. An iPhone, iPad, Mac or Apple TV can play to the PC as an AirPlay speaker, and the PC can join multi-room playback with HomePods and other AirPlay devices.

## Usage

1. Run `AirPlay2Audio.exe`.
2. Set the device name and network interface, choose whether to sync to timestamps, and click "启动接收" (Start). The Apple device and the PC must be on the same local network and able to reach each other.
3. Pick the receiver in the Apple device's audio output list.

Audio plays on the Windows default output device. To choose a device or output format, open 高级设置 → 音频输出 (Advanced → Audio output).

AAC audio is decoded by the AAC decoder built into Windows. Windows N and KN editions include it only after the Media Feature Pack is installed.

Settings and the pairing identity live in `%APPDATA%\AirPlay2Audio`. Deleting that folder resets the receiver's identity, and paired devices have to pair again.

### Command line

`AirPlay2Audio.exe` is the window. It runs `AirPlay2Engine.exe` in the background. You can also run the engine directly from a command prompt:

```bat
AirPlay2Engine.exe -n "Living Room" -output-device 2 -statistics
```

`Start-Console.cmd` starts the engine in a console window at realtime priority. `-list-devices` prints the output devices and exits.

## Building

Requirements:

- Windows x64
- Go 1.26 or newer
- The .NET Framework C# compiler (`csc.exe`) that ships with Windows

Build and run the tests:

```powershell
.\build.ps1 -Test
```

The script writes `AirPlay2Engine.exe` and `AirPlay2Audio.exe`. If Go is not on `PATH`, pass `-GoExe 'C:\path\to\go.exe'`.

## Options

These are the command-line options of `AirPlay2Engine.exe`. The window's advanced settings produce the same options.

### General

| Option | Default | Description |
| --- | --- | --- |
| `-n` | `AirPlay 2 Audio` | Receiver name |
| `-i` | empty | Network interface name. Empty advertises on every active interface |
| `-pw` | empty | Password senders must enter to play. Empty lets anyone on the network play |
| `-data` | `%APPDATA%\AirPlay2Audio` | Identity and pairing directory. Each instance needs its own |
| `-trace` | off | Log RTSP request headers and bodies for protocol debugging |

### Synchronization

| Option | Default | Description |
| --- | --- | --- |
| `-sync` | `true` | Schedule audio against the sender clock. `-sync=false` plays audio as it arrives, without sync |
| `-realtime-anchor` | `shairport` | AirPlay 2 realtime stream timing, `shairport` or `low-latency`. See [Sync and latency](#sync-and-latency) |
| `-delay` | `0` | Extra output latency compensation in milliseconds, from -500 to 500 |
| `-drift-correction` | `true` | Correct clock drift by adding or removing single samples |
| `-drift-tolerance` | `2` | Timing error in milliseconds allowed before drift correction starts |
| `-resync-threshold` | `50` | Timing error in milliseconds above which playback skips or inserts silence |
| `-statistics` | off | Log a sync and network statistics line every 10 seconds |
| `-monitor` | off | Print an `@stats` line to standard output every second. The window reads it |

### Audio output

| Option | Default | Description |
| --- | --- | --- |
| `-output-device` | empty | Output device, as an index or part of a name from `-list-devices`, case-insensitive. Empty uses the Windows default device |
| `-list-devices` | | List output devices and exit |
| `-output-bits` | `16` | Output sample size, 16, 24 or 32 |
| `-output-rate` | `44100` | Output sample rate, 44100, 48000, 88200, 96000, 176400 or 192000 |
| `-dither` | `auto` | Dither, `auto`, `on` or `off` |
| `-volume-range` | `60` | Attenuation in dB at the sender's lowest volume step. Same as shairport-sync's `volume_range_db` |

### Network

| Option | Default | Description |
| --- | --- | --- |
| `-port` | `7000` | RTSP control port (TCP) |
| `-event-port` | `60003` | AirPlay 2 event port (TCP) |
| `-udp-port-base` | `0` | First port for audio data, control and timing sockets. 0 lets Windows choose |
| `-udp-port-range` | `100` | Number of ports starting at `-udp-port-base` |
| `-ipv4only` | off | Listen and advertise on IPv4 only |
| `-device-id-offset` | `0` | Added to the MAC address to form the device ID. Each instance on one PC needs a different value |

### Sessions

| Option | Default | Description |
| --- | --- | --- |
| `-session-timeout` | `120` | End a session after this many seconds without requests or audio. 0 means never |
| `-active-timeout` | `10` | Seconds after playback stops before the output device is released. 0 keeps it open |
| `-refuse-interruption` | off | Refuse a new sender while another one is connected |
| `-on-play-begin`, `-on-play-end`, `-on-active-begin`, `-on-active-end` | empty | Command to run when the event happens |

## Technical details

The audio renderer follows [UxPlay](https://github.com/FDH2/UxPlay). AirPlay 1, drift correction, statistics and session control follow [shairport-sync](https://github.com/mikebrady/shairport-sync). The transport and decoding paths differ from both projects.

### Sync and latency

By default the receiver plays audio at the sender's PTP/RTP timestamps, so video and other multi-room speakers line up with it. With sync off, the receiver plays audio as soon as it arrives, in order.

AirPlay 2 realtime streams have two timing modes:

- `shairport` plays `frame_1 - 11025` at the anchor's network time, as shairport-sync does. It stays in sync with HomePods and other AirPlay receivers in a multi-room group.
- `low-latency` plays `frame_2 - 11025` at the anchor's network time, about 1.75 seconds earlier than `shairport`.

If audio lags noticeably behind video on iOS, try `low-latency`. On macOS, `low-latency` can make video lag behind audio. These commands lower the macOS audio latency:

```bash
sudo defaults write /Library/Preferences/com.apple.airplay audioLatencySystemMs -int 500
sudo defaults write /Library/Preferences/com.apple.airplay audioLatencyMs -int 500
sudo killall -KILL AirPlayXPCHelper coreaudiod
```

### Drift correction

In sync mode the receiver keeps comparing the sender clock with the sound card's playback position. When the smoothed error exceeds `-drift-tolerance`, it plays one extra or one fewer sample per output block of about 11.6 ms. Linear interpolation spreads that sample across the block, so there is no click. Correction stops once the error falls below a quarter of the tolerance.

This matches shairport-sync's `basic` interpolation. The maximum correction rate is about 1900 ppm, which covers typical crystal offsets. The receiver only skips audio or inserts silence when the error stays above `-resync-threshold`, for example after a sound card underrun. `-drift-correction=false` turns correction off, and the receiver then only jumps when the error exceeds `-resync-threshold`.

In a local test with the AirPlay 1 mock sender, 45 seconds on each of two sound cards, the correction rate was high for the first 10 to 15 seconds while the clocks converged. After that it dropped to 0 ppm and the sync error stayed within ±0.6 ms. Long-term drift and multi-room accuracy with real Apple devices have not been measured.

### Statistics

With `-statistics`, the receiver logs a `STATS` line every 10 seconds. The fields follow shairport-sync's `statistics` output.

| Field | Meaning |
| --- | --- |
| `sync-error`, `smoothed`, `min`, `max` | Sync error over the interval. Positive means playback is behind the sender |
| `correction` | Net drift correction rate in ppm, and the number of samples added and removed. Once stable, it is close to the clock offset between the sender and the sound card |
| `sender-clock` | Rate difference between the sender clock and the local system clock, in ppm |
| `resyncs`, `late-dropped`, `silent` | Number of jumps, samples dropped for arriving late, and samples output as silence |
| `queue` | Minimum and maximum length of the playback queue, in packets |
| `packets`, `missing`, `late`, `resend-requested`, `resend-recovered`, `output-queue-full` | UDP audio packets received, lost, out of order, requested again and recovered. Applies to AirPlay 2 realtime and AirPlay 1 |

In `low-latency` mode the line reports the samples removed to catch up instead.

### Audio output

When `-output-device` is empty, the receiver releases the device after `-active-timeout` seconds of idle time and opens the current Windows default device on the next playback. The Windows waveOut API truncates device names to 31 characters.

Senders always send 44.1 kHz 16-bit audio. A higher output format does not improve the source. It helps when a sound card only accepts certain formats, or to keep precision when lowering the volume. Volume is applied in floating point, so 24-bit and 32-bit output lose no precision. At rates other than 44.1 kHz, the receiver resamples with a windowed sinc filter.

`-dither auto` adds TPDF dither only for 16-bit output after a volume change or resampling.

### Multiple senders

Several senders can connect at once. A newly connected sender waits and does not interrupt the one playing. The sender that started playing last owns the output, and the receiver pauses the one before it. An AirPlay 2 sender gets the pause command over the event channel and stays connected. Pressing play on it again switches the output back. When the playing sender disconnects, the output goes to the most recently connected remaining sender. When a device reconnects, the new connection replaces its old one.

The receiver keeps the PTP clock of every connected sender in sync, and times playback only from the playing sender's clock or the clock its audio anchor names. Switching senders therefore needs no clock resync. The log line `PTP following clock …` shows which clock the receiver follows.

AirPlay 1 has no remote pause channel. An AirPlay 1 sender that loses the output is muted but keeps playing on its side. To get the output back, it has to send RECORD again, which usually means starting playback again.

`-refuse-interruption` makes the receiver answer new senders with 453 while one is connected.

The `-on-*` commands run asynchronously through `cmd.exe`, with the event name in the `AIRPLAY_EVENT` environment variable:

```bat
AirPlay2Engine.exe -on-play-begin "echo %AIRPLAY_EVENT% >> %TEMP%\airplay.log"
```

`-session-timeout` detects senders that have gone away. AirPlay 2 senders send heartbeats while paused, so pausing does not trigger the timeout.

### Ports and multiple instances

Service discovery advertises the actual `-port` and `-event-port` values. `-udp-port-base 6000 -udp-port-range 100` keeps the audio data, control and timing ports within 6000 to 6099 (TCP and UDP), which makes firewall rules easier to write.

To run several instances on one PC, give each a different `-n`, `-data`, `-port`, `-event-port` and `-device-id-offset`. Senders treat instances with the same `-device-id-offset` as one device. For example:

```bat
AirPlay2Engine.exe -n Study -data "%APPDATA%\AirPlay2Audio\study" -port 7100 -event-port 60103 -device-id-offset 1 -output-device 2
```

PTP always uses UDP ports 319 and 320, and only one program can hold them. An instance that starts later logs `WARNING: PTP UDP …`. It can still receive AirPlay 1, and AirPlay 2 with `-sync=false`, but it cannot sync AirPlay 2 to timestamps.

### Development tools

- `tools/protocol_probe.py` connects to a running local receiver and checks its protocol responses.
- `tools/mock_sender` sends a silent mock PTP/AAC stream. Its second argument is the port.
- `tools/generate_aac_fixtures.py` regenerates the AAC test data in `audio/testdata` with ffmpeg.
- `tools/mock_raop` simulates an AirPlay 1 sender. It checks Apple-Response, sends encrypted ALAC and answers NTP. For example, `go run ./tools/mock_raop -port 7000 -seconds 5`.

## Acknowledgements

This project draws on these open-source projects:

- [airplay2-receiver](https://github.com/openairplay/airplay2-receiver)
- [shairport-sync](https://github.com/mikebrady/shairport-sync)
- [goplay2](https://github.com/AlbanSeurat/goplay2)
- [UxPlay](https://github.com/FDH2/UxPlay)
- [uxplay-windows](https://github.com/leapbtw/uxplay-windows)
