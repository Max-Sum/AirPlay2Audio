# AirPlay 2 音频接收器

Windows 平台的 AirPlay2 纯音频接收器。
 [UxPlay](https://github.com/FDH2/UxPlay) 的音频渲染器，AirPlay 1、漂移校正、统计输出和会话控制参考了 [shairport-sync](https://github.com/mikebrady/shairport-sync)；各自的传输协议和编解码路径并不相同。

## 运行

1. 双击 `AirPlay2Audio.exe`。
2. 设置设备名称和网卡，选择是否按时间戳同步，点击“启动接收”。Apple 设备与电脑需要连接到同一个可互通的局域网。
3. 在发送端的音频输出列表中选择接收器。音频默认使用 Windows 的默认输出设备，也可以在“高级设置 → 音频输出”中指定设备和输出格式。

设置和配对身份存储在 `%APPDATA%\AirPlay2Audio`；删除它们会重置接收器身份和配对记录。

## 构建与测试

需要 Windows x64、Go 1.26 或更新版本，以及 Windows 自带的 .NET Framework C# 编译器。运行：

```powershell
.\build.ps1 -Test
```

如果 Go 没有加入 `PATH`，可传入 `-GoExe 'C:\path\to\go.exe'`。`tools/protocol_probe.py` 连接已启动的本地接收器；`tools/mock_sender` 可发送静音的模拟 PTP/AAC 流（第二个参数为端口）；`tools/mock_raop` 模拟 AirPlay 1 发送端（验证 Apple-Response、发送加密 ALAC、应答 NTP），例如 `go run ./tools/mock_raop -port 7000 -seconds 5`。

## 引擎详细设置

### 同步与延迟

默认按照发送端的 PTP/RTP 时间戳播放，以支持音视频及多设备同步。界面取消“按时间戳同步”，或在命令行使用 `-sync=false`，会按已接收音频的顺序尽快播放。

AirPlay 2 实时流的定时有两种模式。用 `-realtime-anchor` 选择：

- `shairport`（默认）：与 shairport-sync 相同，在锚点的网络时间播放 `frame_1 - 11025`。与 HomePod 等其他 AirPlay 设备组成多房间时保持同步。
- `low-latency`：在锚点的网络时间播放 `frame_2 - 11025`，比默认模式早约 1.75 秒。

如果发现在iOS下播放出现明显音频滞后，可以尝试使用low-latency模式。macOS在low-latency模式可能会出现视频比音频滞后的情况，可以运行以下命令缩小延迟：
```
sudo defaults write /Library/Preferences/com.apple.airplay audioLatencySystemMs -int 500
sudo defaults write /Library/Preferences/com.apple.airplay audioLatencyMs -int 500
sudo killall -SIGNAL SIGKILL AirPlayXPCHelper coreaudiod
```

### 漂移校正

同步模式会持续比较发送端时钟和声卡的实际播放进度。平滑后的误差超过容差（默认 2 毫秒，`-drift-tolerance`）时，每个输出块（约 11.6 毫秒）多播或少播一个采样，并用线性插值把这一个采样分摊到整块里，避免咔嗒声；误差回到容差的四分之一以内后停止校正。这相当于 shairport-sync 的 "basic" 插值，最大校正速率约 1900 ppm，足以覆盖常见的晶振偏差。只有误差持续超过 `-resync-threshold`（默认 50 毫秒，例如声卡欠载）时才直接跳过或补静音。`-drift-correction=false` 可关闭校正，恢复旧的“只在大误差时跳变”行为。

本地自测中（AirPlay 1 模拟发送端，两种声卡各 45 秒）：时钟收敛的前 10~15 秒内校正速率较高，之后校正降为 0 ppm，同步误差保持在 ±0.6 毫秒以内。真实 Apple 设备上的长时间漂移和多房间精度仍未测量。

### 同步统计

加 `-statistics`（或在高级设置中勾选）后，每 10 秒输出一行 `STATS`，内容参考 shairport-sync 的 `statistics`：

- `sync-error`、`smoothed`、`min`、`max`：本段时间内的同步误差（正数表示播放落后于发送端）；
- `correction`：漂移校正的净速率（ppm）及多播、少播的采样数，稳定后约等于发送端与声卡之间的时钟偏差；
- `sender-clock`：发送端时钟相对本机系统时钟的速率差（ppm）；
- `resyncs`、`late-dropped`、`silent`：跳变次数、因过时而丢弃的采样、无音频输出的采样；
- `que
ue`：待播放队列的最小值和最大值（包）；
- `packets`、`missing`、`late`、`resend-requested`、`resend-recovered`、`output-queue-full`：UDP 音频（AirPlay 2 实时和 AirPlay 1）的收包、丢包、乱序、重发请求和补回情况。

低延迟模式下改为报告追赶时删除的采样数。

### 音频输出

- 设备：`-output-device` 接受 `-list-devices` 列出的序号或名称（名称的一部分即可，不区分大小写）。留空使用 Windows 默认设备；这时设备在闲置后会被释放（见下文），下次播放会跟随当时的默认设备。Windows 的 waveOut 接口会把设备名截断为 31 个字符。
- 格式：`-output-bits 16|24|32`，`-output-rate 44100|48000|88200|96000|176400|192000`。音量在浮点中计算，输出 24/32 位时不损失精度；采样率不是 44.1 kHz 时使用加窗 sinc 重采样。发送端的音频始终是 44.1 kHz 16 位，提高输出格式不会提升音源本身的音质，主要用于声卡只接受特定格式、或希望音量衰减不丢失精度的情况。
- 抖动：`-dither auto|on|off`，默认 `auto` 只在输出 16 位且做过音量调整或重采样时加入 TPDF 抖动。

### 端口与多实例

- 控制端口 `-port`（默认 TCP 7000）、事件端口 `-event-port`（默认 TCP 60003）可以修改，服务发现会公布实际端口。
- 音频数据、控制和计时端口默认由 Windows 动态分配；`-udp-port-base 6000 -udp-port-range 100` 会限制在 6000~6099（TCP 和 UDP），便于编写防火墙规则。
- 同时运行多个实例时，每个实例需要不同的 `-n`、`-data`、`-port`、`-event-port` 和 `-device-id-offset`（例如 1、2……，否则发送端会把它们当成同一台设备），例如：
  `AirPlay2Engine.exe -n 书房 -data "%APPDATA%\AirPlay2Audio\study" -port 7100 -event-port 60103 -device-id-offset 1 -output-device 2`
- PTP 使用固定的 UDP 319/320，只能由一个程序占用。后启动的实例会提示 `WARNING: PTP UDP …`，此时它仍可接收 AirPlay 1 和低延迟模式（`-sync=false`）的 AirPlay 2，但无法进行 AirPlay 2 的按时间戳同步。

### 会话控制

参考 shairport-sync 的 `sessioncontrol`：

- `-session-timeout`（默认 120 秒，0 表示不限）：会话在这么长时间内没有任何 RTSP 请求和音频、控制包时视为发送端已经离开，接收器断开它。AirPlay 2 发送端暂停时也会定期发送心跳，不受影响。
- `-active-timeout`（默认 10 秒，0 表示一直占用）：停止播放后保持输出设备打开的时间，过后释放设备。
- 多个发送端可以同时连接。后连接的设备先等待，不影响正在播放的设备；哪个设备**后开始播放**，就由它占用输出，接收器同时让之前播放的设备暂停（AirPlay 2 通过事件通道发送暂停命令），它的连接保持不变，在它上面再次点播放就会切换回来。正在播放的设备断开后，输出交给最近连接的另一台设备。接收器与每台已连接设备的 PTP 时钟同时保持同步，只用正在播放的设备（或其音频锚点指定的）时钟定时，所以切换时不需要重新对时；日志中的 `PTP following clock …` 显示当前跟随的时钟。同一台设备重新连接会替换它自己的旧连接。
- AirPlay 1 发送端没有可用的远程暂停通道：被切走时只是静音，它自己不会暂停；它重新获得输出需要再次发起 RECORD（通常是重新开始播放）。
- `-refuse-interruption`：有发送端连接时拒绝新的发送端（返回 453），不允许同时连接。
- `-on-play-begin`、`-on-play-end`、`-on-active-begin`、`-on-active-end`：在相应时刻通过 `cmd.exe` 异步运行命令，环境变量 `AIRPLAY_EVENT` 为事件名。例如 `-on-play-begin "echo %AIRPLAY_EVENT% >> %TEMP%\airplay.log"`。


## 感谢

本项目参考了以下开源项目：

- [airplay2-receiver](https://github.com/openairplay/airplay2-receiver)
- [shairport-sync](https://github.com/mikebrady/shairport-sync)
- [goplay2](https://github.com/AlbanSeurat/goplay2)
- [uxplay](https://github.com/fdh2/uxplay)
- [uxplay-windows](https://github.com/leapbtw/uxplay-windows)

特此感谢。