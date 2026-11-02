# AirPlay 2 音频接收器

[English](README.md) | 中文

在 Windows 上接收 AirPlay 2 和 AirPlay 1 音频的接收器。iPhone、iPad、Mac 和 Apple TV 可以把电脑当作 AirPlay 音箱，并与 HomePod 等其他 AirPlay 设备组成多房间播放。

## 使用

1. 双击 `AirPlay2Audio.exe`。
2. 设置设备名称和网卡，选择是否按时间戳同步，点击"启动接收"。Apple 设备和电脑需要在同一个局域网内，并且能互相访问。
3. 在 Apple 设备的音频输出列表中选择这台接收器。

音频默认从 Windows 的默认输出设备播放，可以在"高级设置 → 音频输出"中指定设备和格式。

AAC 音频由 Windows 自带的 AAC 解码器解码。Windows N 和 KN 版需要先安装 Media Feature Pack。

设置和配对身份保存在 `%APPDATA%\AirPlay2Audio`。删除这个目录会重置接收器身份，已配对的设备需要重新配对。

### 命令行

`AirPlay2Audio.exe` 是图形界面，它在后台运行 `AirPlay2Engine.exe`。也可以在命令提示符中直接运行引擎：

```bat
AirPlay2Engine.exe -n "客厅" -output-device 2 -statistics
```

`Start-Console.cmd` 以实时优先级在控制台中启动引擎。`-list-devices` 列出可用的输出设备后退出。

## 构建

需要：

- Windows x64
- Go 1.26 或更新版本
- Windows 自带的 .NET Framework C# 编译器 (`csc.exe`)

构建并运行测试：

```powershell
.\build.ps1 -Test
```

脚本生成 `AirPlay2Engine.exe` 和 `AirPlay2Audio.exe`。Go 不在 `PATH` 中时，用 `-GoExe 'C:\path\to\go.exe'` 指定。

## 参数

以下是 `AirPlay2Engine.exe` 的命令行参数。图形界面的高级设置会生成同样的参数。

### 基本

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-n` | `AirPlay 2 Audio` | 接收器名称 |
| `-i` | 空 | 网卡名称。留空时在所有活动网卡上公布 |
| `-pw` | 空 | 播放密码。留空时局域网内任何设备都可以播放 |
| `-data` | `%APPDATA%\AirPlay2Audio` | 身份和配对数据目录。每个实例需要单独的目录 |
| `-trace` | 关 | 记录 RTSP 请求头和正文，用于协议调试 |

### 同步

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-sync` | `true` | 按发送端时钟播放。`-sync=false` 收到音频就播放，不做同步 |
| `-realtime-anchor` | `shairport` | AirPlay 2 实时流的定时方式，`shairport` 或 `low-latency`。见[同步与延迟](#同步与延迟) |
| `-delay` | `0` | 额外的输出延迟补偿，单位毫秒，范围 -500 到 500 |
| `-drift-correction` | `true` | 通过增减单个采样校正时钟漂移 |
| `-drift-tolerance` | `2` | 开始漂移校正前允许的误差，单位毫秒 |
| `-resync-threshold` | `50` | 误差超过这个值时直接跳过或补静音，单位毫秒 |
| `-statistics` | 关 | 每 10 秒输出一行同步和网络统计 |
| `-monitor` | 关 | 每秒在标准输出打印一行 `@stats`，供图形界面读取 |

### 音频输出

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-output-device` | 空 | 输出设备，填 `-list-devices` 中的序号或名称的一部分，不区分大小写。留空使用 Windows 默认设备 |
| `-list-devices` | | 列出输出设备后退出 |
| `-output-bits` | `16` | 输出位深，16、24 或 32 |
| `-output-rate` | `44100` | 输出采样率，44100、48000、88200、96000、176400 或 192000 |
| `-dither` | `auto` | 抖动，`auto`、`on` 或 `off` |
| `-volume-range` | `60` | 发送端最低音量对应的衰减，单位 dB。对应 shairport-sync 的 `volume_range_db` |

### 网络

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-port` | `7000` | RTSP 控制端口 (TCP) |
| `-event-port` | `60003` | AirPlay 2 事件端口 (TCP) |
| `-udp-port-base` | `0` | 音频数据、控制和计时端口的起始值。0 表示由 Windows 分配 |
| `-udp-port-range` | `100` | 从 `-udp-port-base` 开始可用的端口数量 |
| `-ipv4only` | 关 | 只在 IPv4 上监听和公布 |
| `-device-id-offset` | `0` | 加到 MAC 地址上生成设备 ID。同一台电脑上的多个实例需要不同的值 |

### 会话

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-session-timeout` | `120` | 会话在这么多秒内没有请求和音频时断开。0 表示不限 |
| `-active-timeout` | `10` | 停止播放多少秒后释放输出设备。0 表示一直占用 |
| `-refuse-interruption` | 关 | 已有发送端连接时拒绝新的发送端 |
| `-on-play-begin`、`-on-play-end`、`-on-active-begin`、`-on-active-end` | 空 | 在对应事件发生时运行的命令 |

## 技术细节

音频渲染参考了 [UxPlay](https://github.com/FDH2/UxPlay)。AirPlay 1、漂移校正、统计输出和会话控制参考了 [shairport-sync](https://github.com/mikebrady/shairport-sync)。这些项目的传输协议和解码路径与本项目不同。

以下选项都可以在窗口的“高级设置”中修改；括号中的命令行参数用于直接运行 `AirPlay2Engine.exe`。

### 同步与延迟

默认按发送端的 PTP/RTP 时间戳播放，这样视频画面和多房间设备能与音频对齐。关闭同步后，接收器按收到的顺序尽快播放音频。

AirPlay 2 实时流有两种定时方式：

- `shairport`：在锚点的网络时间播放 `frame_1 - 11025`，与 shairport-sync 一致。和 HomePod 等设备组成多房间时保持同步。
- `low-latency`：在锚点的网络时间播放 `frame_2 - 11025`，比 `shairport` 早约 1.75 秒。

如果 iOS 上播放视频时声音明显滞后，可以试试 `low-latency`。在 macOS 上使用 `low-latency` 时，画面可能比声音慢，可以用以下命令缩短 macOS 的音频延迟：

```bash
sudo defaults write /Library/Preferences/com.apple.airplay audioLatencySystemMs -int 500
sudo defaults write /Library/Preferences/com.apple.airplay audioLatencyMs -int 500
sudo killall -KILL AirPlayXPCHelper coreaudiod
```

### 漂移校正

同步模式下，接收器持续比较发送端时钟和声卡的实际播放进度。平滑后的误差超过 `-drift-tolerance` 时，每个约 11.6 毫秒的输出块多播或少播一个采样。这个采样通过线性插值分摊到整块中，所以不会产生咔嗒声。误差降到容差的四分之一以内时停止校正。

这种做法与 shairport-sync 的 `basic` 插值相同，最大校正速率约 1900 ppm，能覆盖常见的晶振偏差。只有误差持续超过 `-resync-threshold` 时，接收器才直接跳过音频或补静音，比如声卡欠载之后。`-drift-correction=false` 关闭校正，此时只在误差超过 `-resync-threshold` 时跳变。

本地测试用 AirPlay 1 模拟发送端，在两块声卡上各播放 45 秒。前 10 到 15 秒时钟收敛，校正速率较高。之后校正速率降到 0 ppm，同步误差保持在 ±0.6 毫秒以内。真实 Apple 设备上的长时间漂移和多房间精度还没有测量。

### 同步统计

开启 `-statistics` 后，每 10 秒输出一行 `STATS`，字段参考 shairport-sync 的 `statistics`。

| 字段 | 含义 |
| --- | --- |
| `sync-error`、`smoothed`、`min`、`max` | 这段时间内的同步误差。正数表示播放落后于发送端 |
| `correction` | 漂移校正的净速率 (ppm)，以及多播和少播的采样数。稳定后约等于发送端与声卡的时钟偏差 |
| `sender-clock` | 发送端时钟相对本机系统时钟的速率差 (ppm) |
| `resyncs`、`late-dropped`、`silent` | 跳变次数、因过时丢弃的采样数、没有音频输出的采样数 |
| `queue` | 待播放队列的最小值和最大值，单位为包 |
| `packets`、`missing`、`late`、`resend-requested`、`resend-recovered`、`output-queue-full` | UDP 音频的收包、丢包、乱序、重发请求和补回数量。适用于 AirPlay 2 实时流和 AirPlay 1 |

`low-latency` 模式下改为报告追赶进度时删除的采样数。

### 音频输出

`-output-device` 留空时，接收器在闲置 `-active-timeout` 秒后释放设备，下次播放时使用当时的 Windows 默认设备。Windows 的 waveOut 接口把设备名截断为 31 个字符。

发送端的音频总是 44.1 kHz、16 位。提高输出格式不会改善音源本身，适用于声卡只接受特定格式，或者希望音量衰减不损失精度的情况。音量在浮点中计算，输出 24 或 32 位时不损失精度。采样率不是 44.1 kHz 时，接收器使用加窗 sinc 重采样。

`-dither auto` 只在输出 16 位并且调整过音量或做过重采样时加入 TPDF 抖动。

### 多个发送端

多个发送端可以同时连接。后连接的设备先等待，不影响正在播放的设备。最后开始播放的设备占用输出，之前播放的设备会被暂停。AirPlay 2 设备通过事件通道收到暂停命令，连接保持不变，在它上面再次点播放就会切回来。正在播放的设备断开后，输出交给最近连接的另一台设备。同一台设备重新连接时，新连接替换它自己的旧连接。

接收器同时与每台已连接设备的 PTP 时钟保持同步，定时只使用正在播放的设备的时钟，或其音频锚点指定的时钟，所以切换时不需要重新对时。日志中的 `PTP following clock …` 显示当前跟随的时钟。

AirPlay 1 没有远程暂停通道。AirPlay 1 发送端被切走时只是静音，它自己不会暂停。要重新获得输出，它需要再次发送 RECORD，通常就是重新开始播放。

`-refuse-interruption` 让接收器在已有连接时对新发送端返回 453，不允许同时连接。

`-on-*` 命令通过 `cmd.exe` 异步运行，环境变量 `AIRPLAY_EVENT` 为事件名，例如：

```bat
AirPlay2Engine.exe -on-play-begin "echo %AIRPLAY_EVENT% >> %TEMP%\airplay.log"
```

`-session-timeout` 判断发送端是否已经离开。AirPlay 2 发送端暂停时也会定期发送心跳，所以暂停不会触发超时。

### 端口与多实例

服务发现公布 `-port` 和 `-event-port` 的实际值。`-udp-port-base 6000 -udp-port-range 100` 把音频数据、控制和计时端口限制在 6000 到 6099 (TCP 和 UDP)，方便编写防火墙规则。

同时运行多个实例时，每个实例需要不同的 `-n`、`-data`、`-port`、`-event-port` 和 `-device-id-offset`。`-device-id-offset` 相同的实例会被发送端当成同一台设备。例如：

```bat
AirPlay2Engine.exe -n 书房 -data "%APPDATA%\AirPlay2Audio\study" -port 7100 -event-port 60103 -device-id-offset 1 -output-device 2
```

PTP 固定使用 UDP 319 和 320，同一时间只能有一个程序占用。后启动的实例会提示 `WARNING: PTP UDP …`。这个实例仍能接收 AirPlay 1，以及 `-sync=false` 下的 AirPlay 2，但不能按时间戳同步 AirPlay 2。

### 开发工具

- `tools/protocol_probe.py` 连接正在运行的本地接收器，检查协议响应。
- `tools/mock_sender` 发送静音的模拟 PTP/AAC 流，第二个参数是端口。
- `tools/generate_aac_fixtures.py` 用 ffmpeg 重新生成 `audio/testdata` 中的 AAC 测试数据。
- `tools/mock_raop` 模拟 AirPlay 1 发送端。它验证 Apple-Response，发送加密的 ALAC，并应答 NTP。例如 `go run ./tools/mock_raop -port 7000 -seconds 5`。

## 致谢

本项目参考了以下开源项目：

- [airplay2-receiver](https://github.com/openairplay/airplay2-receiver)
- [shairport-sync](https://github.com/mikebrady/shairport-sync)
- [goplay2](https://github.com/AlbanSeurat/goplay2)
- [UxPlay](https://github.com/FDH2/UxPlay)
- [uxplay-windows](https://github.com/leapbtw/uxplay-windows)
