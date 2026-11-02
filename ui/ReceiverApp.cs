// AirPlay 2 audio receiver - Windows front end.
// Compiled with the .NET Framework C# compiler (C# 5), see build.ps1.
using System;
using System.Collections.Generic;
using System.Diagnostics;
using System.Drawing;
using System.Drawing.Drawing2D;
using System.Globalization;
using System.IO;
using System.Net;
using System.Net.NetworkInformation;
using System.Net.Sockets;
using System.Runtime.InteropServices;
using System.Security.Cryptography;
using System.Text;
using System.Text.RegularExpressions;
using System.Threading;
using System.Windows.Forms;
using Microsoft.Win32;

internal static class Program
{
    private const string MutexName = "Local\\AirPlay2Audio.UI";
    private const string ShowEventName = "Local\\AirPlay2Audio.UI.Show";

    [DllImport("user32.dll")] private static extern bool SetProcessDPIAware();

    [STAThread]
    private static void Main(string[] args)
    {
        // Build helper: writes ui\app.ico from the drawn logo.
        if (args.Length == 2 && args[0] == "--export-icon") { Theme.ExportIcon(args[1]); return; }
        bool background = false;
        foreach (string arg in args) if (String.Equals(arg, "--background", StringComparison.OrdinalIgnoreCase)) background = true;

        bool created;
        using (Mutex mutex = new Mutex(true, MutexName, out created)) {
            if (!created) {
                // Already running: bring the existing window to the front.
                try { using (EventWaitHandle show = EventWaitHandle.OpenExisting(ShowEventName)) show.Set(); } catch (Exception) { }
                return;
            }
            try { SetProcessDPIAware(); } catch (Exception) { }
            Application.EnableVisualStyles();
            Application.SetCompatibleTextRenderingDefault(false);
            using (EventWaitHandle show = new EventWaitHandle(false, EventResetMode.AutoReset, ShowEventName)) {
                MainForm form = new MainForm(background);
                Thread listener = new Thread(delegate() {
                    while (true) {
                        show.WaitOne();
                        try { form.BeginInvoke((MethodInvoker)delegate { form.ShowFromTray(); }); } catch (Exception) { return; }
                    }
                });
                listener.IsBackground = true;
                listener.Start();
                Application.Run(form);
            }
            GC.KeepAlive(mutex);
        }
        // Started again only after the mutex is released, so the new copy
        // does not hand over to this one.
        if (MainForm.RestartRequested) {
            try { Process.Start(Application.ExecutablePath); } catch (Exception) { }
        }
    }
}

// ---------------------------------------------------------------- language

// Interface language: Simplified Chinese or English. Strings are written
// in pairs at the point of use: L.T("中文", "English").
internal static class L
{
    public const string Auto = "", Chinese = "zh", English = "en";

    public static bool Zh { get; private set; }

    static L() { Init(Auto); }

    // setting: Auto (follow the Windows display language), Chinese or English.
    public static void Init(string setting)
    {
        if (setting == Chinese) Zh = true;
        else if (setting == English) Zh = false;
        else Zh = CultureInfo.CurrentUICulture.Name.StartsWith("zh", StringComparison.OrdinalIgnoreCase);
    }

    public static string T(string zh, string en) { return Zh ? zh : en; }
}

// ---------------------------------------------------------------- media keys

// Registers the window with Windows' system media controls, so the media
// keys of keyboards and USB or Bluetooth speakers, and the media flyout,
// control the sender. Pressed buttons are passed on as "next", "previous"
// or "toggle". All WinRT use stays in this class: if it is unavailable,
// Create returns null and the rest of the program works without it.
internal sealed class MediaSession
{
    [ComImport, Guid("ddb0472d-c911-4a1f-86d9-dc3d71a95f5a"), InterfaceType(ComInterfaceType.InterfaceIsIInspectable)]
    private interface ISystemMediaTransportControlsInterop
    {
        [return: MarshalAs(UnmanagedType.IInspectable)]
        object GetForWindow(IntPtr appWindow, [In] ref Guid riid);
    }

    // IID of ISystemMediaTransportControls.
    private static readonly Guid ControlsIid = new Guid("99fa3ff4-1742-42a6-902e-087d41f965ec");

    private readonly Windows.Media.SystemMediaTransportControls controls;
    private readonly Action<string> command;
    private readonly System.Windows.Forms.Timer clock = new System.Windows.Forms.Timer { Interval = 1000 };
    private bool active, paused;
    private string sender = "", title = "", artist = "", album = "";
    private Windows.Storage.Streams.RandomAccessStreamReference artwork;
    private long positionMs, durationMs;
    private DateTime positionAt = DateTime.UtcNow;

    private MediaSession(Windows.Media.SystemMediaTransportControls controls, Action<string> command)
    {
        this.controls = controls;
        this.command = command;
        clock.Tick += delegate { if (active && !paused) UpdateTimeline(); };
        clock.Start();
        controls.IsPlayEnabled = true;
        controls.IsPauseEnabled = true;
        controls.IsNextEnabled = true;
        controls.IsPreviousEnabled = true;
        controls.IsStopEnabled = false;
        controls.ButtonPressed += OnButtonPressed;
        controls.PlaybackStatus = Windows.Media.MediaPlaybackStatus.Closed;
        controls.IsEnabled = false;
    }

    // command is called on a background thread.
    public static MediaSession Create(IntPtr window, Action<string> command)
    {
        try { return CreateControls(window, command); }
        catch (Exception ex) { LogStore.Add(L.T("系统媒体控制不可用，媒体键将无法使用：", "System media controls are unavailable; media keys will not work: ") + ex.Message); return null; }
    }

    private static MediaSession CreateControls(IntPtr window, Action<string> command)
    {
        object factory = System.Runtime.InteropServices.WindowsRuntime.WindowsRuntimeMarshal.GetActivationFactory(typeof(Windows.Media.SystemMediaTransportControls));
        ISystemMediaTransportControlsInterop interop = (ISystemMediaTransportControlsInterop)factory;
        Guid iid = ControlsIid;
        var controls = (Windows.Media.SystemMediaTransportControls)interop.GetForWindow(window, ref iid);
        return new MediaSession(controls, command);
    }

    private void OnButtonPressed(Windows.Media.SystemMediaTransportControls sender, Windows.Media.SystemMediaTransportControlsButtonPressedEventArgs args)
    {
        switch (args.Button) {
            case Windows.Media.SystemMediaTransportControlsButton.Next: command("next"); break;
            case Windows.Media.SystemMediaTransportControlsButton.Previous: command("previous"); break;
            case Windows.Media.SystemMediaTransportControlsButton.Play:
            case Windows.Media.SystemMediaTransportControlsButton.Pause:
                // The receiver does not know yet whether the sender is paused,
                // so both buttons toggle; the shown state follows the presses.
                command("toggle");
                SetPaused(!paused);
                break;
        }
    }

    // Shows the session while a sender is connected and hides it otherwise.
    public void SetActive(bool connected, string senderName)
    {
        try {
            if (connected && !active) {
                // A new session: forget the previous sender's track.
                paused = false;
                title = artist = album = "";
                artwork = null;
                positionMs = durationMs = 0;
            }
            active = connected;
            sender = senderName;
            if (!connected) {
                controls.PlaybackStatus = Windows.Media.MediaPlaybackStatus.Closed;
                controls.DisplayUpdater.ClearAll();
                controls.DisplayUpdater.Update();
                controls.IsEnabled = false;
                return;
            }
            controls.IsEnabled = true;
            UpdateDisplay();
            SetPaused(paused);
        } catch (Exception ex) {
            LogStore.Add(L.T("更新系统媒体控制失败：", "Failed to update the system media controls: ") + ex.Message);
        }
    }

    // Handles an "@media" line from the engine (without the prefix). Values
    // are never logged.
    public void Handle(string message)
    {
        string[] f = message.Split(' ');
        try {
            switch (f[0]) {
                case "meta":
                    if (f.Length < 5) return;
                    title = Text(f[1]); artist = Text(f[2]); album = Text(f[3]);
                    long duration;
                    if (Int64.TryParse(f[4], NumberStyles.None, CultureInfo.InvariantCulture, out duration) && duration > 0) durationMs = duration;
                    if (active) UpdateDisplay();
                    break;
                case "progress":
                    long position, total;
                    if (f.Length < 3 || !Int64.TryParse(f[1], NumberStyles.None, CultureInfo.InvariantCulture, out position)
                        || !Int64.TryParse(f[2], NumberStyles.None, CultureInfo.InvariantCulture, out total)) return;
                    positionMs = position; positionAt = DateTime.UtcNow;
                    if (total > 0) durationMs = total;
                    if (active) UpdateTimeline();
                    break;
                case "state":
                    if (f.Length < 2) return;
                    SetPaused(f[1] == "paused");
                    break;
                case "art":
                    if (f.Length < 2) return;
                    artwork = LoadArtwork(Text(f[1]));
                    if (active) UpdateDisplay();
                    break;
            }
        } catch (Exception ex) {
            LogStore.Add(L.T("更新系统媒体控制失败：", "Failed to update the system media controls: ") + ex.Message);
        }
    }

    private static string Text(string field)
    {
        if (field == "-") return "";
        try { return Encoding.UTF8.GetString(Convert.FromBase64String(field)); }
        catch (FormatException) { return ""; }
    }

    // The image is copied into memory, so the engine can replace the file.
    private static Windows.Storage.Streams.RandomAccessStreamReference LoadArtwork(string path)
    {
        if (path.Length == 0) return null;
        byte[] bytes;
        try { bytes = File.ReadAllBytes(path); } catch (IOException) { return null; } catch (UnauthorizedAccessException) { return null; }
        if (bytes.Length == 0) return null;
        // Built from WinRT types only: the .NET stream adapters need the
        // SDK's merged Windows.winmd, which build.ps1 does not use.
        var stream = new Windows.Storage.Streams.InMemoryRandomAccessStream();
        var writer = new Windows.Storage.Streams.DataWriter(stream);
        writer.WriteBytes(bytes);
        Windows.Foundation.IAsyncOperation<uint> store = writer.StoreAsync();
        Windows.Foundation.IAsyncInfo info = (Windows.Foundation.IAsyncInfo)store;
        DateTime deadline = DateTime.UtcNow.AddSeconds(2);
        // An in-memory store finishes almost immediately.
        while (info.Status == Windows.Foundation.AsyncStatus.Started && DateTime.UtcNow < deadline) Thread.Sleep(1);
        if (info.Status != Windows.Foundation.AsyncStatus.Completed) return null;
        store.GetResults();
        writer.DetachStream();
        stream.Seek(0);
        return Windows.Storage.Streams.RandomAccessStreamReference.CreateFromStream(stream);
    }

    private void UpdateDisplay()
    {
        Windows.Media.SystemMediaTransportControlsDisplayUpdater display = controls.DisplayUpdater;
        display.Type = Windows.Media.MediaPlaybackType.Music;
        if (title.Length > 0) {
            display.MusicProperties.Title = title;
            display.MusicProperties.Artist = artist;
            display.MusicProperties.AlbumTitle = album;
        } else {
            // No track information yet: show who is playing.
            display.MusicProperties.Title = sender.Length > 0 ? sender : "AirPlay";
            display.MusicProperties.Artist = "AirPlay";
            display.MusicProperties.AlbumTitle = "";
        }
        display.Thumbnail = artwork;
        display.Update();
        UpdateTimeline();
    }

    private void UpdateTimeline()
    {
        if (durationMs <= 0) return;
        long position = positionMs;
        if (!paused) position += (long)(DateTime.UtcNow - positionAt).TotalMilliseconds;
        position = Math.Max(0, Math.Min(durationMs, position));
        try {
            Windows.Media.SystemMediaTransportControlsTimelineProperties timeline = new Windows.Media.SystemMediaTransportControlsTimelineProperties();
            timeline.StartTime = TimeSpan.Zero;
            timeline.MinSeekTime = TimeSpan.Zero;
            timeline.EndTime = TimeSpan.FromMilliseconds(durationMs);
            timeline.MaxSeekTime = timeline.EndTime;
            timeline.Position = TimeSpan.FromMilliseconds(position);
            controls.UpdateTimelineProperties(timeline);
        } catch (Exception) { }
    }

    private void SetPaused(bool value)
    {
        if (paused != value) {
            // Keep the position where playback stopped or resumed.
            if (!paused) positionMs = Math.Min(durationMs > 0 ? durationMs : Int64.MaxValue, positionMs + (long)(DateTime.UtcNow - positionAt).TotalMilliseconds);
            positionAt = DateTime.UtcNow;
        }
        paused = value;
        if (!active) return;
        try {
            controls.PlaybackStatus = paused ? Windows.Media.MediaPlaybackStatus.Paused : Windows.Media.MediaPlaybackStatus.Playing;
            UpdateTimeline();
        } catch (Exception) { }
    }
}

// ---------------------------------------------------------------- settings

internal sealed class Settings
{
    public string DeviceName = DefaultName();
    public string NetworkInterface = "";
    public int DelayMs = 0;
    public bool Synchronize = true;
    public int LocalVolume = 100;
    public bool TraceLog = false;
    public bool TrayTipShown = false;
    public string Password = ""; // empty: anyone on the network may play
    public string OutputDevice = ""; // empty: Windows default device
    public int OutputRate = 44100;
    public int OutputBits = 16;
    public bool Statistics = false;
    public string Language = L.Auto;
    public string Dither = "auto"; // auto, on or off
    public string RealtimeAnchor = "shairport"; // shairport or low-latency
    public bool DriftCorrection = true;
    public double DriftToleranceMs = 2;
    public double ResyncThresholdMs = 50;
    public int SessionTimeout = 120; // seconds, 0: never
    public int ActiveTimeout = 0;    // seconds, 0: keep the output open
    public bool RefuseInterruption = false;
    public string OnPlayBegin = "", OnPlayEnd = "", OnActiveBegin = "", OnActiveEnd = "";
    public int Port = 7000;
    public int EventPort = 60003;
    public int UdpPortBase = 0; // 0: Windows chooses
    public int UdpPortRange = 100;
    public bool IPv4Only = false;
    public int DeviceIdOffset = 0;

    public static readonly string Directory = Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.ApplicationData), "AirPlay2Audio");
    private static string FilePath { get { return Path.Combine(Directory, "settings.ini"); } }

    private static string DefaultName()
    {
        string machine = Environment.MachineName;
        if (String.IsNullOrEmpty(machine)) return "AirPlay 2 Audio";
        return machine.Length > 40 ? machine.Substring(0, 40) : machine;
    }

    public static Settings Load()
    {
        Settings s = new Settings();
        try {
            if (File.Exists(FilePath)) {
                foreach (string line in File.ReadAllLines(FilePath, Encoding.UTF8)) {
                    int eq = line.IndexOf('=');
                    if (eq <= 0) continue;
                    s.Set(line.Substring(0, eq).Trim(), line.Substring(eq + 1).Trim());
                }
            } else {
                s.MigrateOld();
            }
        } catch (IOException) { }
        return s;
    }

    // Settings written by the previous version: one value per line.
    private void MigrateOld()
    {
        string old = Path.Combine(Directory, "ui-settings.txt");
        if (!File.Exists(old)) return;
        string[] v = File.ReadAllLines(old, Encoding.UTF8);
        if (v.Length > 0 && v[0].Trim().Length > 0) DeviceName = v[0].Trim();
        if (v.Length > 1) NetworkInterface = v[1];
        int n; bool b;
        if (v.Length > 2 && Int32.TryParse(v[2], out n)) DelayMs = n;
        if (v.Length > 3 && Boolean.TryParse(v[3], out b)) Synchronize = b;
        if (v.Length > 4 && Int32.TryParse(v[4], out n)) LocalVolume = n;
        Save();
    }

    private void Set(string key, string value)
    {
        int n; bool b; double d;
        switch (key) {
            case "DeviceName": if (value.Length > 0) DeviceName = value; break;
            case "NetworkInterface": NetworkInterface = value; break;
            case "DelayMs": if (Int32.TryParse(value, out n)) DelayMs = Math.Max(-500, Math.Min(500, n)); break;
            case "Synchronize": if (Boolean.TryParse(value, out b)) Synchronize = b; break;
            case "LocalVolume": if (Int32.TryParse(value, out n)) LocalVolume = Math.Max(0, Math.Min(100, n)); break;
            case "TraceLog": if (Boolean.TryParse(value, out b)) TraceLog = b; break;
            case "TrayTipShown": if (Boolean.TryParse(value, out b)) TrayTipShown = b; break;
            case "Password": Password = Unprotect(value); break;
            case "OutputDevice": OutputDevice = value; break;
            case "OutputRate": if (Int32.TryParse(value, out n) && OutputFormat.ValidRate(n)) OutputRate = n; break;
            case "OutputBits": if (Int32.TryParse(value, out n) && (n == 16 || n == 24 || n == 32)) OutputBits = n; break;
            case "Statistics": if (Boolean.TryParse(value, out b)) Statistics = b; break;
            case "Language": if (value == L.Chinese || value == L.English) Language = value; break;
            case "Dither": if (value == "auto" || value == "on" || value == "off") Dither = value; break;
            case "RealtimeAnchor": if (value == "shairport" || value == "low-latency") RealtimeAnchor = value; break;
            case "DriftCorrection": if (Boolean.TryParse(value, out b)) DriftCorrection = b; break;
            case "DriftToleranceMs": if (Double.TryParse(value, NumberStyles.Float, CultureInfo.InvariantCulture, out d) && d > 0 && d <= 1000) DriftToleranceMs = d; break;
            case "ResyncThresholdMs": if (Double.TryParse(value, NumberStyles.Float, CultureInfo.InvariantCulture, out d) && d > 0 && d <= 10000) ResyncThresholdMs = d; break;
            case "SessionTimeout": if (Int32.TryParse(value, out n) && n >= 0) SessionTimeout = n; break;
            case "ActiveTimeout": if (Int32.TryParse(value, out n) && n >= 0) ActiveTimeout = n; break;
            case "RefuseInterruption": if (Boolean.TryParse(value, out b)) RefuseInterruption = b; break;
            case "OnPlayBegin": OnPlayBegin = value; break;
            case "OnPlayEnd": OnPlayEnd = value; break;
            case "OnActiveBegin": OnActiveBegin = value; break;
            case "OnActiveEnd": OnActiveEnd = value; break;
            case "Port": if (Int32.TryParse(value, out n) && n > 0 && n <= 65535) Port = n; break;
            case "EventPort": if (Int32.TryParse(value, out n) && n > 0 && n <= 65535) EventPort = n; break;
            case "UdpPortBase": if (Int32.TryParse(value, out n) && n >= 0 && n <= 65535) UdpPortBase = n; break;
            case "UdpPortRange": if (Int32.TryParse(value, out n) && n >= 3 && n <= 65535) UdpPortRange = n; break;
            case "IPv4Only": if (Boolean.TryParse(value, out b)) IPv4Only = b; break;
            case "DeviceIdOffset": if (Int32.TryParse(value, out n) && n >= 0 && n <= 999) DeviceIdOffset = n; break;
        }
    }

    // The password is stored encrypted for the current Windows user (DPAPI).
    private static readonly byte[] PasswordEntropy = Encoding.UTF8.GetBytes("AirPlay2Audio.Password");

    private static string Protect(string value)
    {
        if (value.Length == 0) return "";
        try {
            byte[] data = ProtectedData.Protect(Encoding.UTF8.GetBytes(value), PasswordEntropy, DataProtectionScope.CurrentUser);
            return "dpapi:" + Convert.ToBase64String(data);
        } catch (CryptographicException) { return ""; }
    }

    private static string Unprotect(string value)
    {
        if (!value.StartsWith("dpapi:", StringComparison.Ordinal)) return "";
        try {
            byte[] data = ProtectedData.Unprotect(Convert.FromBase64String(value.Substring(6)), PasswordEntropy, DataProtectionScope.CurrentUser);
            return Encoding.UTF8.GetString(data);
        } catch (CryptographicException) { return ""; } catch (FormatException) { return ""; }
    }

    public void Save()
    {
        try {
            System.IO.Directory.CreateDirectory(Directory);
            File.WriteAllLines(FilePath, new string[] {
                "DeviceName=" + DeviceName.Replace("\r", "").Replace("\n", ""),
                "NetworkInterface=" + NetworkInterface,
                "DelayMs=" + DelayMs.ToString(CultureInfo.InvariantCulture),
                "Synchronize=" + Synchronize,
                "LocalVolume=" + LocalVolume.ToString(CultureInfo.InvariantCulture),
                "TraceLog=" + TraceLog,
                "TrayTipShown=" + TrayTipShown,
                "Password=" + Protect(Password),
                "OutputDevice=" + OutputDevice.Replace("\r", "").Replace("\n", ""),
                "OutputRate=" + OutputRate.ToString(CultureInfo.InvariantCulture),
                "OutputBits=" + OutputBits.ToString(CultureInfo.InvariantCulture),
                "Statistics=" + Statistics,
                "Language=" + Language,
                "Dither=" + Dither,
                "RealtimeAnchor=" + RealtimeAnchor,
                "DriftCorrection=" + DriftCorrection,
                "DriftToleranceMs=" + DriftToleranceMs.ToString("R", CultureInfo.InvariantCulture),
                "ResyncThresholdMs=" + ResyncThresholdMs.ToString("R", CultureInfo.InvariantCulture),
                "SessionTimeout=" + SessionTimeout.ToString(CultureInfo.InvariantCulture),
                "ActiveTimeout=" + ActiveTimeout.ToString(CultureInfo.InvariantCulture),
                "RefuseInterruption=" + RefuseInterruption,
                "OnPlayBegin=" + OneLine(OnPlayBegin),
                "OnPlayEnd=" + OneLine(OnPlayEnd),
                "OnActiveBegin=" + OneLine(OnActiveBegin),
                "OnActiveEnd=" + OneLine(OnActiveEnd),
                "Port=" + Port.ToString(CultureInfo.InvariantCulture),
                "EventPort=" + EventPort.ToString(CultureInfo.InvariantCulture),
                "UdpPortBase=" + UdpPortBase.ToString(CultureInfo.InvariantCulture),
                "UdpPortRange=" + UdpPortRange.ToString(CultureInfo.InvariantCulture),
                "IPv4Only=" + IPv4Only,
                "DeviceIdOffset=" + DeviceIdOffset.ToString(CultureInfo.InvariantCulture),
            }, Encoding.UTF8);
        } catch (IOException) { } catch (UnauthorizedAccessException) { }
    }

    private static string OneLine(string value) { return value.Replace("\r", "").Replace("\n", " ").Trim(); }
}

// Start with Windows, via the per-user Run key.
internal static class AutoStart
{
    private const string RunKey = "Software\\Microsoft\\Windows\\CurrentVersion\\Run";
    private const string ValueName = "AirPlay2Audio";

    private static string Command { get { return "\"" + Application.ExecutablePath + "\" --background"; } }

    public static bool IsEnabled()
    {
        try {
            using (RegistryKey key = Registry.CurrentUser.OpenSubKey(RunKey)) {
                return key != null && key.GetValue(ValueName) != null;
            }
        } catch (Exception) { return false; }
    }

    public static void Set(bool enabled)
    {
        using (RegistryKey key = Registry.CurrentUser.CreateSubKey(RunKey)) {
            if (enabled) key.SetValue(ValueName, Command);
            else if (key.GetValue(ValueName) != null) key.DeleteValue(ValueName);
        }
    }

    // Keep the entry pointing at this copy if the folder was moved.
    public static void Refresh()
    {
        try {
            using (RegistryKey key = Registry.CurrentUser.OpenSubKey(RunKey, true)) {
                if (key == null) return;
                string current = key.GetValue(ValueName) as string;
                if (current != null && current != Command) key.SetValue(ValueName, Command);
            }
        } catch (Exception) { }
    }
}

// ---------------------------------------------------------------- engine process

internal static class Job
{
    // A job object that kills the engine if this window process ends abruptly.
    [StructLayout(LayoutKind.Sequential)]
    private struct BasicLimits {
        public long PerProcessUserTimeLimit, PerJobUserTimeLimit;
        public uint LimitFlags;
        public UIntPtr MinimumWorkingSetSize, MaximumWorkingSetSize;
        public uint ActiveProcessLimit;
        public UIntPtr Affinity;
        public uint PriorityClass, SchedulingClass;
    }
    [StructLayout(LayoutKind.Sequential)]
    private struct IoCounters { public ulong a, b, c, d, e, f; }
    [StructLayout(LayoutKind.Sequential)]
    private struct ExtendedLimits {
        public BasicLimits Basic;
        public IoCounters Io;
        public UIntPtr ProcessMemoryLimit, JobMemoryLimit, PeakProcessMemoryUsed, PeakJobMemoryUsed;
    }
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode)] private static extern IntPtr CreateJobObject(IntPtr attributes, string name);
    [DllImport("kernel32.dll")] private static extern bool SetInformationJobObject(IntPtr job, int infoClass, ref ExtendedLimits info, int length);
    [DllImport("kernel32.dll")] private static extern bool AssignProcessToJobObject(IntPtr job, IntPtr process);

    private static IntPtr handle = IntPtr.Zero;

    public static void Add(Process process)
    {
        try {
            if (handle == IntPtr.Zero) {
                handle = CreateJobObject(IntPtr.Zero, null);
                ExtendedLimits limits = new ExtendedLimits();
                limits.Basic.LimitFlags = 0x2000; // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
                SetInformationJobObject(handle, 9, ref limits, Marshal.SizeOf(typeof(ExtendedLimits)));
            }
            AssignProcessToJobObject(handle, process.Handle);
        } catch (Exception) { }
    }
}

internal enum ReceiverState { Stopped, Starting, Waiting, Connected, Playing, Failed }

internal sealed class Engine
{
    private Process process;
    private bool stopRequested;
    public bool Ready { get; private set; }
    public event Action<string> Line;
    public event Action<string> Media; // "@media" lines, without the prefix
    public event Action<string> Stats; // "@stats" lines, without the prefix
    public event Action<int, bool> Exited; // exit code, stop was requested

    public bool Running { get { return process != null; } }

    public void Start(Settings s)
    {
        if (process != null) return;
        string exe = Path.Combine(AppDomain.CurrentDomain.BaseDirectory, "AirPlay2Engine.exe");
        if (!File.Exists(exe)) throw new FileNotFoundException(L.T("找不到 AirPlay2Engine.exe，请保留程序目录中的全部文件。", "AirPlay2Engine.exe was not found. Keep all files in the program folder together."));
        StringBuilder args = new StringBuilder();
        args.Append("-n ").Append(Quote(s.DeviceName));
        args.Append(" -delay ").Append(s.DelayMs.ToString(CultureInfo.InvariantCulture));
        args.Append(" -data ").Append(Quote(Path.Combine(Settings.Directory, "receiver")));
        if (!s.Synchronize) args.Append(" -sync=false");
        if (s.NetworkInterface.Length > 0) args.Append(" -i ").Append(Quote(s.NetworkInterface));
        if (s.TraceLog) args.Append(" -trace");
        if (s.OutputDevice.Length > 0) args.Append(" -output-device ").Append(Quote(s.OutputDevice));
        if (s.OutputRate != 44100) args.Append(" -output-rate ").Append(s.OutputRate.ToString(CultureInfo.InvariantCulture));
        if (s.OutputBits != 16) args.Append(" -output-bits ").Append(s.OutputBits.ToString(CultureInfo.InvariantCulture));
        if (s.Statistics) args.Append(" -statistics");
        if (s.Dither != "auto") args.Append(" -dither ").Append(s.Dither);
        if (s.RealtimeAnchor != "shairport") args.Append(" -realtime-anchor ").Append(s.RealtimeAnchor);
        if (!s.DriftCorrection) args.Append(" -drift-correction=false");
        if (s.DriftToleranceMs != 2) args.Append(" -drift-tolerance ").Append(s.DriftToleranceMs.ToString("R", CultureInfo.InvariantCulture));
        if (s.ResyncThresholdMs != 50) args.Append(" -resync-threshold ").Append(s.ResyncThresholdMs.ToString("R", CultureInfo.InvariantCulture));
        if (s.SessionTimeout != 120) args.Append(" -session-timeout ").Append(s.SessionTimeout.ToString(CultureInfo.InvariantCulture));
        if (s.ActiveTimeout != 10) args.Append(" -active-timeout ").Append(s.ActiveTimeout.ToString(CultureInfo.InvariantCulture));
        if (s.RefuseInterruption) args.Append(" -refuse-interruption");
        if (s.OnPlayBegin.Length > 0) args.Append(" -on-play-begin ").Append(Quote(s.OnPlayBegin));
        if (s.OnPlayEnd.Length > 0) args.Append(" -on-play-end ").Append(Quote(s.OnPlayEnd));
        if (s.OnActiveBegin.Length > 0) args.Append(" -on-active-begin ").Append(Quote(s.OnActiveBegin));
        if (s.OnActiveEnd.Length > 0) args.Append(" -on-active-end ").Append(Quote(s.OnActiveEnd));
        if (s.Port != 7000) args.Append(" -port ").Append(s.Port.ToString(CultureInfo.InvariantCulture));
        if (s.EventPort != 60003) args.Append(" -event-port ").Append(s.EventPort.ToString(CultureInfo.InvariantCulture));
        if (s.UdpPortBase > 0) {
            args.Append(" -udp-port-base ").Append(s.UdpPortBase.ToString(CultureInfo.InvariantCulture));
            args.Append(" -udp-port-range ").Append(s.UdpPortRange.ToString(CultureInfo.InvariantCulture));
        }
        if (s.IPv4Only) args.Append(" -ipv4only");
        if (s.DeviceIdOffset != 0) args.Append(" -device-id-offset ").Append(s.DeviceIdOffset.ToString(CultureInfo.InvariantCulture));
        args.Append(" -monitor");

        Process p = new Process();
        p.StartInfo = new ProcessStartInfo {
            FileName = exe, Arguments = args.ToString(), WorkingDirectory = AppDomain.CurrentDomain.BaseDirectory,
            UseShellExecute = false, CreateNoWindow = true,
            RedirectStandardInput = true, RedirectStandardOutput = true, RedirectStandardError = true,
            StandardOutputEncoding = Encoding.UTF8, StandardErrorEncoding = Encoding.UTF8,
        };
        // Passed in the environment, so it does not show in the process command line.
        p.StartInfo.EnvironmentVariables.Remove("AIRPLAY_PASSWORD");
        if (s.Password.Length > 0) p.StartInfo.EnvironmentVariables["AIRPLAY_PASSWORD"] = s.Password;
        p.EnableRaisingEvents = true;
        DataReceivedEventHandler onData = delegate(object sender, DataReceivedEventArgs e) {
            if (e.Data == null) return;
            if (e.Data.StartsWith("@media ", StringComparison.Ordinal)) {
                // Now playing details: passed on, never logged.
                Action<string> media = Media;
                if (media != null) media(e.Data.Substring(7));
                return;
            }
            if (e.Data.StartsWith("@stats ", StringComparison.Ordinal)) {
                // Once a second for the monitor panel: never logged.
                Action<string> stats = Stats;
                if (stats != null) stats(e.Data.Substring(7));
                return;
            }
            if (e.Data.Contains("READY ")) Ready = true;
            Action<string> handler = Line;
            if (handler != null) handler(e.Data);
        };
        p.OutputDataReceived += onData;
        p.ErrorDataReceived += onData;
        p.Exited += delegate {
            int code = -1;
            try { code = p.ExitCode; } catch (InvalidOperationException) { }
            bool requested = stopRequested;
            if (Object.ReferenceEquals(process, p)) process = null;
            Action<int, bool> handler = Exited;
            if (handler != null) handler(code, requested);
        };
        stopRequested = false;
        Ready = false;
        process = p;
        try {
            p.Start();
            Job.Add(p);
            RaisePriority(p);
            p.BeginOutputReadLine();
            p.BeginErrorReadLine();
        } catch (Exception) {
            process = null;
            p.Dispose();
            throw;
        }
        SetLocalVolume(s.LocalVolume);
    }

    // Runs the engine at the highest CPU priority. Without administrator rights
    // Windows quietly grants High instead of Realtime.
    private static void RaisePriority(Process p)
    {
        try { p.PriorityClass = ProcessPriorityClass.RealTime; }
        catch (Exception) {
            try { p.PriorityClass = ProcessPriorityClass.High; } catch (Exception) { }
        }
    }

    public void SetLocalVolume(int percent)
    {
        Send("volume " + percent.ToString(CultureInfo.InvariantCulture));
    }

    // Asks the sender to change track: "next", "previous", "toggle" and so on.
    public void SendMedia(string command)
    {
        Send("media " + command);
    }

    private void Send(string command)
    {
        Process p = process;
        if (p == null) return;
        try { if (!p.HasExited) { p.StandardInput.WriteLine(command); p.StandardInput.Flush(); } }
        catch (InvalidOperationException) { } catch (IOException) { }
    }

    // Asks the engine to stop (it unregisters from the network), then kills it
    // if it does not exit in time.
    public void Stop(bool wait)
    {
        Process p = process;
        if (p == null) return;
        stopRequested = true;
        Send("stop");
        WaitCallback finish = delegate {
            try { if (!p.WaitForExit(2500)) { p.Kill(); p.WaitForExit(1000); } } catch (Exception) { }
        };
        if (wait) finish(null); else ThreadPool.QueueUserWorkItem(finish);
    }

    // Windows command-line quoting, including backslashes before quotes.
    private static string Quote(string value)
    {
        StringBuilder result = new StringBuilder("\""); int slashes = 0;
        foreach (char c in value) {
            if (c == '\\') { slashes++; continue; }
            if (c == '"') { result.Append('\\', slashes * 2 + 1); result.Append(c); slashes = 0; continue; }
            result.Append('\\', slashes); slashes = 0; result.Append(c);
        }
        result.Append('\\', slashes * 2); result.Append('"'); return result.ToString();
    }
}

// Keeps recent engine output in memory and in %APPDATA%\AirPlay2Audio\logs.
internal static class LogStore
{
    private static readonly List<string> lines = new List<string>();
    private static readonly object gate = new object();
    private static StreamWriter file;
    public static event Action<string> Added;
    public static event Action Cleared;

    public static string LogDirectory { get { return Path.Combine(Settings.Directory, "logs"); } }

    public static void Open()
    {
        try {
            System.IO.Directory.CreateDirectory(LogDirectory);
            string current = Path.Combine(LogDirectory, "receiver.log");
            string previous = Path.Combine(LogDirectory, "receiver.previous.log");
            if (File.Exists(current)) { if (File.Exists(previous)) File.Delete(previous); File.Move(current, previous); }
            file = new StreamWriter(current, false, new UTF8Encoding(false));
            file.AutoFlush = true;
        } catch (Exception) { file = null; }
    }

    public static void Add(string line)
    {
        string stamped = line.Length > 20 && line[4] == '/' ? line : DateTime.Now.ToString("yyyy/MM/dd HH:mm:ss ") + line;
        lock (gate) {
            lines.Add(stamped);
            if (lines.Count > 5000) lines.RemoveRange(0, 1000);
            if (file != null) { try { file.WriteLine(stamped); } catch (Exception) { } }
        }
        Action<string> handler = Added;
        if (handler != null) handler(stamped);
    }

    public static string[] Snapshot() { lock (gate) return lines.ToArray(); }

    public static void Clear()
    {
        lock (gate) lines.Clear();
        Action handler = Cleared;
        if (handler != null) handler();
    }
}

// ---------------------------------------------------------------- look

internal static class Theme
{
    public static readonly Color Window = Color.FromArgb(243, 245, 249);
    public static readonly Color Card = Color.White;
    public static readonly Color Text = Color.FromArgb(28, 33, 41);
    public static readonly Color SubText = Color.FromArgb(84, 92, 104);
    public static readonly Color Border = Color.FromArgb(222, 226, 232);
    public static readonly Color Accent = Color.FromArgb(10, 102, 224);
    public static readonly Color Green = Color.FromArgb(22, 145, 70);
    public static readonly Color Orange = Color.FromArgb(200, 120, 0);
    public static readonly Color Red = Color.FromArgb(200, 40, 40);
    public static readonly Color Grey = Color.FromArgb(120, 128, 140);

    public static Font Regular(float size) { return new Font("Microsoft YaHei UI", size, FontStyle.Regular); }
    public static Font Bold(float size) { return new Font("Microsoft YaHei UI", size, FontStyle.Bold); }

    // The app logo: speaker cone with sound waves on a rounded square.
    public static Bitmap Logo(int size, bool active)
    {
        Bitmap bmp = new Bitmap(size, size);
        using (Graphics g = Graphics.FromImage(bmp)) {
            g.SmoothingMode = SmoothingMode.AntiAlias;
            g.Clear(Color.Transparent);
            float r = size * 0.22f;
            using (GraphicsPath path = RoundRect(new RectangleF(0, 0, size - 1, size - 1), r))
            using (LinearGradientBrush fill = new LinearGradientBrush(new PointF(0, 0), new PointF(0, size),
                active ? Color.FromArgb(52, 140, 255) : Color.FromArgb(150, 158, 170),
                active ? Color.FromArgb(10, 92, 214) : Color.FromArgb(105, 113, 125))) {
                g.FillPath(fill, path);
            }
            float s = size / 32f;
            using (SolidBrush white = new SolidBrush(Color.White))
            using (Pen wave = new Pen(Color.White, Math.Max(1.2f, 2.2f * s))) {
                wave.StartCap = LineCap.Round; wave.EndCap = LineCap.Round;
                g.FillPolygon(white, new PointF[] {
                    new PointF(6 * s, 12.5f * s), new PointF(10 * s, 12.5f * s), new PointF(15.5f * s, 8 * s),
                    new PointF(15.5f * s, 24 * s), new PointF(10 * s, 19.5f * s), new PointF(6 * s, 19.5f * s) });
                g.DrawArc(wave, 13.5f * s, 11 * s, 7 * s, 10 * s, -50, 100);
                g.DrawArc(wave, 13 * s, 7.5f * s, 12 * s, 17 * s, -50, 100);
            }
        }
        return bmp;
    }

    public static Icon LogoIcon(int size, bool active)
    {
        using (Bitmap bmp = Logo(size, active)) return Icon.FromHandle(bmp.GetHicon());
    }

    public static void ExportIcon(string path)
    {
        int[] sizes = { 16, 20, 24, 32, 40, 48, 64, 256 };
        List<byte[]> images = new List<byte[]>();
        foreach (int size in sizes) {
            using (Bitmap bmp = Logo(size, true))
            using (MemoryStream png = new MemoryStream()) {
                bmp.Save(png, System.Drawing.Imaging.ImageFormat.Png);
                images.Add(png.ToArray());
            }
        }
        using (BinaryWriter w = new BinaryWriter(File.Create(path))) {
            w.Write((ushort)0); w.Write((ushort)1); w.Write((ushort)sizes.Length);
            int offset = 6 + 16 * sizes.Length;
            for (int i = 0; i < sizes.Length; i++) {
                w.Write((byte)(sizes[i] >= 256 ? 0 : sizes[i]));
                w.Write((byte)(sizes[i] >= 256 ? 0 : sizes[i]));
                w.Write((byte)0); w.Write((byte)0);
                w.Write((ushort)1); w.Write((ushort)32);
                w.Write(images[i].Length); w.Write(offset);
                offset += images[i].Length;
            }
            foreach (byte[] image in images) w.Write(image);
        }
    }

    public static GraphicsPath RoundRect(RectangleF rect, float radius)
    {
        GraphicsPath path = new GraphicsPath();
        float d = radius * 2;
        path.AddArc(rect.X, rect.Y, d, d, 180, 90);
        path.AddArc(rect.Right - d, rect.Y, d, d, 270, 90);
        path.AddArc(rect.Right - d, rect.Bottom - d, d, d, 0, 90);
        path.AddArc(rect.X, rect.Bottom - d, d, d, 90, 90);
        path.CloseFigure();
        return path;
    }

    public static Button MakeButton(string text, bool primary)
    {
        Button b = new Button();
        b.Text = text;
        b.AutoSize = true;
        b.AutoSizeMode = AutoSizeMode.GrowAndShrink;
        b.Padding = new Padding(10, 3, 10, 3);
        b.MinimumSize = new Size(0, 34);
        b.Font = Regular(10f);
        b.FlatStyle = FlatStyle.Flat;
        b.FlatAppearance.BorderColor = primary ? Accent : Border;
        b.FlatAppearance.BorderSize = 1;
        b.BackColor = primary ? Accent : Color.White;
        b.ForeColor = primary ? Color.White : Text;
        b.UseVisualStyleBackColor = false;
        b.Cursor = Cursors.Hand;
        return b;
    }
}

// White rounded panel used to group settings.
internal sealed class Card : Panel
{
    public Card()
    {
        DoubleBuffered = true;
        BackColor = Theme.Window;
        Padding = new Padding(16, 12, 16, 12);
        AutoSize = true;
        AutoSizeMode = AutoSizeMode.GrowAndShrink;
        Dock = DockStyle.Top;
    }

    protected override void OnPaint(PaintEventArgs e)
    {
        e.Graphics.SmoothingMode = SmoothingMode.AntiAlias;
        using (GraphicsPath path = Theme.RoundRect(new RectangleF(0.5f, 0.5f, Width - 2, Height - 2), 8))
        using (SolidBrush fill = new SolidBrush(Theme.Card))
        using (Pen border = new Pen(Theme.Border)) {
            e.Graphics.FillPath(fill, path);
            e.Graphics.DrawPath(border, path);
        }
    }

    protected override void OnResize(EventArgs e) { base.OnResize(e); Invalidate(); }
}

// ---------------------------------------------------------------- main window

internal sealed class MainForm : Form
{
    private readonly Settings settings = Settings.Load();
    private readonly Engine engine = new Engine();
    private MediaSession media;
    private readonly bool startInBackground;
    private bool exiting;
    private bool shownOnce;

    private ReceiverState state = ReceiverState.Stopped;
    private string senderName = "";
    private string failure = "";
    private int restartCount;
    private DateTime restartWindow = DateTime.MinValue;
    private readonly System.Windows.Forms.Timer restartTimer = new System.Windows.Forms.Timer();

    private readonly PictureBox logo = new PictureBox();
    private readonly Label statusLabel = new Label();
    private readonly Label errorLabel = new Label();
    private readonly Button toggleButton;
    private readonly TextBox nameBox = new TextBox();
    private readonly TextBox passwordBox = new TextBox();
    private readonly CheckBox showPasswordBox = new CheckBox();
    private readonly TrackBar volumeBar = new TrackBar();
    private readonly Label volumeValue = new Label();
    private readonly Label senderVolume = new Label();
    private readonly CheckBox autoStartBox = new CheckBox();
    private readonly NotifyIcon tray = new NotifyIcon();
    private readonly ToolStripMenuItem trayStatus = new ToolStripMenuItem();
    private readonly ToolStripMenuItem trayToggle = new ToolStripMenuItem();
    private Icon activeIcon, idleIcon;
    private LogForm logForm;
    private readonly PlaybackMonitor monitor = new PlaybackMonitor();
    private MonitorForm monitorForm;

    public static bool RestartRequested { get; private set; }

    public MainForm(bool background)
    {
        L.Init(settings.Language);
        startInBackground = background;
        SuspendLayout();
        Text = L.T("AirPlay 2 音频接收器", "AirPlay 2 Audio Receiver");
        Font = Theme.Regular(10f);
        BackColor = Theme.Window;
        ForeColor = Theme.Text;
        FormBorderStyle = FormBorderStyle.FixedSingle;
        MaximizeBox = false;
        MinimizeBox = true;
        StartPosition = FormStartPosition.CenterScreen;
        AutoSize = true;
        AutoSizeMode = AutoSizeMode.GrowAndShrink;
        Padding = new Padding(0, 0, 18, 16);
        activeIcon = Theme.LogoIcon(32, true);
        idleIcon = Theme.LogoIcon(32, false);
        Icon = activeIcon;

        TableLayoutPanel root = new TableLayoutPanel();
        root.ColumnCount = 1;
        root.AutoSize = true;
        root.AutoSizeMode = AutoSizeMode.GrowAndShrink;
        root.Location = new Point(18, 16);
        root.ColumnStyles.Add(new ColumnStyle(SizeType.Absolute, 440));
        Controls.Add(root);

        // Header: logo, title and live status.
        TableLayoutPanel header = new TableLayoutPanel { ColumnCount = 2, AutoSize = true, Dock = DockStyle.Fill, Margin = new Padding(0, 0, 0, 14) };
        header.ColumnStyles.Add(new ColumnStyle(SizeType.AutoSize));
        header.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100));
        logo.Size = new Size(52, 52);
        logo.SizeMode = PictureBoxSizeMode.Zoom;
        logo.Image = Theme.Logo(104, true);
        logo.Margin = new Padding(0, 2, 14, 0);
        header.Controls.Add(logo, 0, 0);
        header.SetRowSpan(logo, 2);
        Label title = new Label { Text = L.T("AirPlay 2 音频接收器", "AirPlay 2 Audio Receiver"), Font = Theme.Bold(15f), AutoSize = true, Margin = new Padding(0, 0, 0, 2) };
        header.Controls.Add(title, 1, 0);
        statusLabel.AutoSize = true;
        statusLabel.Font = Theme.Bold(11f);
        statusLabel.Margin = new Padding(1, 0, 0, 0);
        header.Controls.Add(statusLabel, 1, 1);
        root.Controls.Add(header);

        // Shown only when something went wrong.
        errorLabel.AutoSize = true;
        errorLabel.MaximumSize = new Size(440, 0);
        errorLabel.ForeColor = Theme.Red;
        errorLabel.Margin = new Padding(0, 0, 0, 12);
        errorLabel.Visible = false;
        root.Controls.Add(errorLabel);

        toggleButton = Theme.MakeButton(L.T("开始接收", "Start receiving"), true);
        toggleButton.AutoSize = false;
        toggleButton.Dock = DockStyle.Fill;
        toggleButton.MinimumSize = new Size(0, 44);
        toggleButton.Font = Theme.Bold(11f);
        toggleButton.Margin = new Padding(0, 0, 0, 16);
        toggleButton.Click += delegate { if (engine.Running) StopReceiver(); else StartReceiver(); };
        root.Controls.Add(toggleButton);

        // Receiver card.
        root.Controls.Add(SectionTitle(L.T("接收器", "Receiver")));
        Card receiverCard = new Card { Margin = new Padding(0, 0, 0, 14) };
        TableLayoutPanel grid = FieldGrid();
        nameBox.Text = settings.DeviceName;
        nameBox.MaxLength = 63;
        nameBox.Dock = DockStyle.Fill;
        nameBox.Margin = new Padding(0, 4, 0, 8);
        nameBox.Leave += delegate { ApplyName(); };
        nameBox.KeyDown += delegate(object s, KeyEventArgs e) { if (e.KeyCode == Keys.Enter) { e.SuppressKeyPress = true; ApplyName(); } };
        AddField(grid, L.T("设备名称", "Device name"), nameBox);

        FlowLayoutPanel passwordRow = new FlowLayoutPanel { AutoSize = true, WrapContents = false, Margin = new Padding(0), Anchor = AnchorStyles.Left, BackColor = Theme.Card };
        passwordBox.Text = settings.Password;
        passwordBox.MaxLength = 64;
        passwordBox.UseSystemPasswordChar = true;
        passwordBox.Width = 230;
        passwordBox.Margin = new Padding(0, 4, 8, 4);
        passwordBox.Leave += delegate { ApplyPassword(); };
        passwordBox.KeyDown += delegate(object s, KeyEventArgs e) { if (e.KeyCode == Keys.Enter) { e.SuppressKeyPress = true; ApplyPassword(); } };
        ConfigureCheck(showPasswordBox, L.T("显示", "Show"));
        showPasswordBox.Margin = new Padding(0, 6, 0, 0);
        showPasswordBox.CheckedChanged += delegate { passwordBox.UseSystemPasswordChar = !showPasswordBox.Checked; };
        passwordRow.Controls.Add(passwordBox);
        passwordRow.Controls.Add(showPasswordBox);
        AddField(grid, L.T("访问密码", "Password"), passwordRow);

        FlowLayoutPanel volumeRow = new FlowLayoutPanel { AutoSize = true, WrapContents = false, Margin = new Padding(0), Anchor = AnchorStyles.Left };
        volumeBar.Minimum = 0; volumeBar.Maximum = 100; volumeBar.TickStyle = TickStyle.None;
        volumeBar.SmallChange = 5; volumeBar.LargeChange = 10;
        volumeBar.AutoSize = false;
        volumeBar.Size = new Size(230, 30); volumeBar.Margin = new Padding(0, 4, 6, 0);
        volumeBar.BackColor = Theme.Card;
        volumeBar.Value = settings.LocalVolume;
        volumeBar.ValueChanged += delegate {
            volumeValue.Text = volumeBar.Value + "%";
            settings.LocalVolume = volumeBar.Value;
            engine.SetLocalVolume(volumeBar.Value);
        };
        volumeBar.MouseUp += delegate { settings.Save(); };
        volumeBar.KeyUp += delegate { settings.Save(); };
        volumeValue.Text = settings.LocalVolume + "%";
        volumeValue.AutoSize = true;
        volumeValue.Margin = new Padding(0, 7, 0, 0);
        volumeRow.Controls.Add(volumeBar);
        volumeRow.Controls.Add(volumeValue);
        AddField(grid, L.T("本机音量", "Local volume"), volumeRow);

        senderVolume.AutoSize = true;
        senderVolume.Text = "—";
        senderVolume.Margin = new Padding(0, 6, 0, 4);
        AddField(grid, L.T("发送端音量", "Sender volume"), senderVolume);

        FlowLayoutPanel mediaRow = new FlowLayoutPanel { AutoSize = true, WrapContents = false, Margin = new Padding(0, 2, 0, 2), Anchor = AnchorStyles.Left, BackColor = Theme.Card };
        Button previous = Theme.MakeButton(L.T("上一首", "Previous"), false);
        previous.Click += delegate { engine.SendMedia("previous"); };
        Button toggle = Theme.MakeButton(L.T("播放/暂停", "Play/Pause"), false);
        toggle.Click += delegate { engine.SendMedia("toggle"); };
        Button next = Theme.MakeButton(L.T("下一首", "Next"), false);
        next.Click += delegate { engine.SendMedia("next"); };
        foreach (Button b in new Button[] { previous, toggle, next }) { b.Margin = new Padding(0, 0, 8, 0); mediaRow.Controls.Add(b); }
        AddField(grid, L.T("播放控制", "Playback"), mediaRow);
        receiverCard.Controls.Add(grid);
        root.Controls.Add(receiverCard);

        // General options card.
        root.Controls.Add(SectionTitle(L.T("常规", "General")));
        Card generalCard = new Card { Margin = new Padding(0, 0, 0, 14) };
        FlowLayoutPanel options = new FlowLayoutPanel { FlowDirection = FlowDirection.TopDown, AutoSize = true, WrapContents = false, Dock = DockStyle.Top, BackColor = Theme.Card };
        ConfigureCheck(autoStartBox, L.T("开机时自启动", "Start with Windows"));
        autoStartBox.Checked = AutoStart.IsEnabled();
        autoStartBox.CheckedChanged += delegate {
            try { AutoStart.Set(autoStartBox.Checked); }
            catch (Exception ex) { MessageBox.Show(this, L.T("无法修改开机启动设置：", "Could not change the startup setting: ") + ex.Message, Text, MessageBoxButtons.OK, MessageBoxIcon.Warning); }
        };
        options.Controls.Add(autoStartBox);
        options.Controls.Add(LanguageRow());
        generalCard.Controls.Add(options);
        root.Controls.Add(generalCard);

        // Bottom buttons.
        FlowLayoutPanel buttons = new FlowLayoutPanel { AutoSize = true, Dock = DockStyle.Fill, WrapContents = true, Margin = new Padding(0) };
        Button advanced = Theme.MakeButton(L.T("高级设置…", "Advanced settings…"), false);
        advanced.Click += delegate { ShowAdvanced(); };
        Button monitorButton = Theme.MakeButton(L.T("播放监控", "Playback monitor"), false);
        monitorButton.Click += delegate { ShowMonitor(); };
        Button logs = Theme.MakeButton(L.T("运行日志", "Log"), false);
        logs.Click += delegate { ShowLog(); };
        Button sound = Theme.MakeButton(L.T("Windows 声音设置", "Windows sound settings"), false);
        sound.Click += delegate { try { Process.Start("ms-settings:sound"); } catch (Exception) { } };
        foreach (Button b in new Button[] { advanced, monitorButton, logs, sound }) { b.Margin = new Padding(0, 0, 8, 8); buttons.Controls.Add(b); }
        root.Controls.Add(buttons);

        BuildTray();
        AutoScaleDimensions = new SizeF(96F, 96F);
        AutoScaleMode = AutoScaleMode.Dpi;
        ResumeLayout(true);

        engine.Line += delegate(string line) { LogStore.Add(line); Post(delegate { OnEngineLine(line); }); };
        engine.Exited += delegate(int code, bool requested) { Post(delegate { OnEngineExited(code, requested); }); };
        engine.Media += delegate(string message) { Post(delegate { if (media != null) media.Handle(message); }); };
        engine.Stats += delegate(string message) { Post(delegate { OnStats(message); }); };
        restartTimer.Tick += delegate { restartTimer.Stop(); if (!engine.Running && !exiting) StartReceiver(); };
        LogStore.Open();
        AutoStart.Refresh();
        OutputDevices.Refresh(null);
        // Reading Handle creates the window even when starting in the tray;
        // the media controls are bound to it.
        media = MediaSession.Create(Handle, delegate(string command) { Post(delegate { engine.SendMedia(command); }); });
        SetState(ReceiverState.Stopped);
    }

    // Captioned in both languages, so it can be found in either.
    private Control LanguageRow()
    {
        FlowLayoutPanel row = new FlowLayoutPanel { AutoSize = true, WrapContents = false, Margin = new Padding(0, 6, 0, 2), BackColor = Theme.Card };
        Label caption = new Label { Text = "语言 / Language", AutoSize = true, Margin = new Padding(0, 6, 10, 0) };
        ComboBox language = new ComboBox { DropDownStyle = ComboBoxStyle.DropDownList, Width = 180, Margin = new Padding(0, 2, 0, 2) };
        string[] values = { L.Auto, L.Chinese, L.English };
        language.Items.AddRange(new object[] { L.T("跟随系统", "System default"), "简体中文", "English" });
        language.SelectedIndex = Math.Max(0, Array.IndexOf(values, settings.Language));
        language.SelectedIndexChanged += delegate {
            string value = values[language.SelectedIndex];
            if (value == settings.Language) return;
            settings.Language = value;
            settings.Save();
            DialogResult answer = MessageBox.Show(this,
                L.T("重新启动程序后将使用新的语言。现在重新启动吗？", "The new language is used after the program restarts. Restart now?"),
                Text, MessageBoxButtons.YesNo, MessageBoxIcon.Question);
            if (answer == DialogResult.Yes) { RestartRequested = true; ExitApplication(); }
        };
        row.Controls.Add(caption);
        row.Controls.Add(language);
        return row;
    }

    private static Label SectionTitle(string text)
    {
        return new Label { Text = text, Font = Theme.Bold(10.5f), AutoSize = true, Margin = new Padding(2, 0, 0, 6) };
    }

    private static TableLayoutPanel FieldGrid()
    {
        TableLayoutPanel grid = new TableLayoutPanel { ColumnCount = 2, AutoSize = true, Dock = DockStyle.Top, BackColor = Theme.Card };
        grid.ColumnStyles.Add(new ColumnStyle(SizeType.Absolute, L.Zh ? 100 : 120));
        grid.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100));
        return grid;
    }

    private static void AddField(TableLayoutPanel grid, string caption, Control field)
    {
        Label label = new Label { Text = caption, AutoSize = true, Anchor = AnchorStyles.Left, Margin = new Padding(0, 6, 8, 6) };
        int row = grid.RowCount;
        grid.RowCount = row + 1;
        grid.RowStyles.Add(new RowStyle(SizeType.AutoSize));
        grid.Controls.Add(label, 0, row);
        grid.Controls.Add(field, 1, row);
    }

    private static void ConfigureCheck(CheckBox box, string text)
    {
        box.Text = text;
        box.AutoSize = true;
        box.Margin = new Padding(0, 4, 0, 4);
        box.BackColor = Theme.Card;
    }

    private void BuildTray()
    {
        ContextMenuStrip menu = new ContextMenuStrip();
        menu.Font = Theme.Regular(9.5f);
        trayStatus.Enabled = false;
        menu.Items.Add(trayStatus);
        menu.Items.Add(new ToolStripSeparator());
        ToolStripMenuItem open = new ToolStripMenuItem(L.T("打开主窗口", "Open main window"));
        open.Font = Theme.Bold(9.5f);
        open.Click += delegate { ShowFromTray(); };
        menu.Items.Add(open);
        trayToggle.Click += delegate { if (engine.Running) StopReceiver(); else StartReceiver(); };
        menu.Items.Add(trayToggle);
        menu.Items.Add(new ToolStripSeparator());
        ToolStripMenuItem quit = new ToolStripMenuItem(L.T("退出", "Exit"));
        quit.Click += delegate { ExitApplication(); };
        menu.Items.Add(quit);
        tray.ContextMenuStrip = menu;
        tray.Icon = idleIcon;
        tray.Visible = true;
        tray.MouseClick += delegate(object s, MouseEventArgs e) { if (e.Button == MouseButtons.Left) ShowFromTray(); };
    }

    // ------------------------------------------------------------ window behaviour

    protected override void SetVisibleCore(bool value)
    {
        // Started with Windows: stay in the notification area.
        if (startInBackground && !shownOnce) {
            shownOnce = true;
            if (!IsHandleCreated) CreateHandle();
            value = false;
            BeginInvoke((MethodInvoker)delegate { StartReceiver(); });
        }
        base.SetVisibleCore(value);
    }

    protected override void OnShown(EventArgs e)
    {
        base.OnShown(e);
        shownOnce = true;
        if (!engine.Running) StartReceiver();
    }

    public void ShowFromTray()
    {
        if (!Visible) Show();
        if (WindowState == FormWindowState.Minimized) WindowState = FormWindowState.Normal;
        Activate();
        BringToFront();
    }

    protected override void OnFormClosing(FormClosingEventArgs e)
    {
        if (!exiting && e.CloseReason == CloseReason.UserClosing) {
            e.Cancel = true;
            ApplyName();
            ApplyPassword();
            Hide();
            if (!settings.TrayTipShown) {
                settings.TrayTipShown = true;
                settings.Save();
                tray.ShowBalloonTip(5000, L.T("仍在后台运行", "Still running in the background"), L.T("AirPlay 接收器会继续工作。点击通知区域的图标可以重新打开，右键可以退出。", "The AirPlay receiver keeps working. Click the notification area icon to reopen it, or right-click it to exit."), ToolTipIcon.Info);
            }
            return;
        }
        exiting = true;
        settings.Save();
        engine.Stop(true);
        tray.Visible = false;
        base.OnFormClosing(e);
    }

    private void ExitApplication()
    {
        exiting = true;
        Close();
    }

    // ------------------------------------------------------------ receiver control

    private bool ValidName(string name)
    {
        return name.Length > 0 && Encoding.UTF8.GetByteCount(name) <= 63;
    }

    private void ApplyName()
    {
        string name = nameBox.Text.Trim();
        if (name == settings.DeviceName) return;
        if (!ValidName(name)) {
            MessageBox.Show(this, L.T("设备名称不能为空，且不能超过 63 个字节（约 21 个汉字）。", "The device name must not be empty or longer than 63 bytes."), Text, MessageBoxButtons.OK, MessageBoxIcon.Information);
            nameBox.Text = settings.DeviceName;
            return;
        }
        nameBox.Text = name;
        settings.DeviceName = name;
        settings.Save();
        if (engine.Running) RestartReceiver();
    }

    private void ApplyPassword()
    {
        string password = passwordBox.Text;
        if (password == settings.Password) return;
        if (password.Trim().Length == 0 && password.Length > 0) {
            MessageBox.Show(this, L.T("密码不能只有空格。", "The password cannot consist only of spaces."), Text, MessageBoxButtons.OK, MessageBoxIcon.Information);
            passwordBox.Text = settings.Password;
            return;
        }
        settings.Password = password;
        settings.Save();
        if (engine.Running) RestartReceiver();
        else SetState(state);
    }

    private void StartReceiver()
    {
        restartTimer.Stop();
        if (engine.Running) return;
        if (!ValidName(settings.DeviceName)) settings.DeviceName = "AirPlay 2 Audio";
        failure = "";
        senderName = "";
        senderVolume.Text = "—";
        monitor.Clear();
        try {
            engine.Start(settings);
            SetState(ReceiverState.Starting);
        } catch (Exception ex) {
            failure = ex.Message;
            LogStore.Add("ERROR: " + ex.Message);
            SetState(ReceiverState.Failed);
        }
    }

    private void StopReceiver()
    {
        restartTimer.Stop();
        restartCount = 0;
        engine.Stop(false);
        toggleButton.Enabled = false;
    }

    private void RestartReceiver()
    {
        if (!engine.Running) { StartReceiver(); return; }
        Action<int, bool> once = null;
        once = delegate { engine.Exited -= once; Post(delegate { StartReceiver(); }); };
        engine.Exited += once;
        engine.Stop(false);
    }

    private static readonly Regex VolumePattern = new Regex(@"AUDIO volume (-?[0-9.]+) dB");
    private static readonly Regex OutputPattern = new Regex("AUDIO output device=\"(.*)\" format=([0-9]+) Hz ([0-9]+)-bit");

    private void OnStats(string message)
    {
        monitor.Update(message);
    }

    private static readonly Regex SenderPattern = new Regex("SESSION (?:started|active) sender=\"(.*)\"");

    private void OnEngineLine(string line)
    {
        if (logForm != null && !logForm.IsDisposed) { /* LogForm listens to LogStore directly */ }
        if (line.Contains("READY ")) { restartCount = 0; SetState(ReceiverState.Waiting); return; }
        Match m = SenderPattern.Match(line);
        if (m.Success) {
            senderName = Regex.Unescape(m.Groups[1].Value);
            monitor.ResetTotals();
            SetState(ReceiverState.Connected);
            return;
        }
        if (line.Contains("AUTH wrong password") && state == ReceiverState.Waiting) {
            ShowError(L.T("有设备输入了错误的访问密码，未能连接。", "A device entered the wrong password and could not connect."));
            return;
        }
        if (line.Contains("SESSION ended")) { senderName = ""; senderVolume.Text = "—"; SetState(ReceiverState.Waiting); return; }
        if (line.Contains("AUDIO connected") || line.Contains("AUDIO realtime ALAC ready") || line.Contains("PCM output active")) {
            SetState(ReceiverState.Playing); return;
        }
        if (line.Contains("AUDIO disconnected") || line.Contains("AUDIO realtime connection closed")) {
            if (state == ReceiverState.Playing) SetState(ReceiverState.Connected);
            return;
        }
        m = OutputPattern.Match(line);
        if (m.Success) {
            monitor.SetOutput(Regex.Unescape(m.Groups[1].Value), m.Groups[2].Value + " Hz / " + m.Groups[3].Value + L.T(" 位", "-bit"));
            return;
        }
        m = VolumePattern.Match(line);
        if (m.Success) {
            double db;
            if (Double.TryParse(m.Groups[1].Value, NumberStyles.Float, CultureInfo.InvariantCulture, out db)) {
                senderVolume.Text = db <= -144 ? L.T("静音", "Muted") : Math.Round(Math.Max(0, Math.Min(1, (db + 30) / 30)) * 100) + "%";
            }
            return;
        }
        if (line.Contains("ERROR: ")) failure = FriendlyError(line.Substring(line.IndexOf("ERROR: ") + 7));
    }

    private string FriendlyError(string message)
    {
        if (message.Contains(settings.Port.ToString(CultureInfo.InvariantCulture)) || message.Contains(settings.EventPort.ToString(CultureInfo.InvariantCulture)) || message.Contains("319") || message.Contains("320") || message.Contains("bind"))
            return L.T("网络端口被占用。可能已有另一个 AirPlay 接收器（例如 UxPlay）在运行，请先关闭它。", "A network port is in use. Another AirPlay receiver (such as UxPlay) may be running; close it first.");
        if (message.Contains("no active network interface"))
            return L.T("找不到可用的网络连接。请确认电脑已连上局域网，或在高级设置中选择网卡。", "No usable network connection was found. Make sure this PC is on the local network, or choose a network adapter in Advanced settings.");
        if (message.Contains("audio output"))
            return L.T("无法打开音频输出设备。请检查 Windows 声音设置中的默认播放设备。", "Could not open the audio output device. Check the default playback device in Windows sound settings.");
        return message;
    }

    private void OnEngineExited(int code, bool requested)
    {
        toggleButton.Enabled = true;
        if (exiting) return;
        if (requested || code == 0) { SetState(ReceiverState.Stopped); return; }
        // Unexpected exit after a successful start: restart a few times.
        if (engine.Ready || failure.Length == 0) {
            if (DateTime.Now - restartWindow > TimeSpan.FromMinutes(5)) { restartWindow = DateTime.Now; restartCount = 0; }
            if (restartCount < 3) {
                restartCount++;
                LogStore.Add(L.T("接收服务意外退出（代码 " + code + "），3 秒后自动重启。", "The receiver service exited unexpectedly (code " + code + "); restarting in 3 seconds."));
                failure = L.T("接收服务意外退出，正在重新启动…", "The receiver service exited unexpectedly. Restarting…");
                SetState(ReceiverState.Starting);
                restartTimer.Interval = 3000;
                restartTimer.Start();
                return;
            }
        }
        if (failure.Length == 0) failure = L.T("接收服务已退出（代码 " + code + "）。详情请查看运行日志。", "The receiver service exited (code " + code + "). See the log for details.");
        SetState(ReceiverState.Failed);
    }

    private void SetState(ReceiverState next)
    {
        state = next;
        if (media != null) media.SetActive(next == ReceiverState.Connected || next == ReceiverState.Playing, senderName);
        string status;
        Color color;
        switch (next) {
            case ReceiverState.Starting:
                status = L.T("● 正在启动", "● Starting"); color = Theme.Orange;
                break;
            case ReceiverState.Waiting:
                status = L.T("● 等待连接", "● Waiting for a connection"); color = Theme.Green;
                break;
            case ReceiverState.Connected:
                status = L.T("● 已连接", "● Connected") + (senderName.Length > 0 ? L.T("：", ": ") + senderName : ""); color = Theme.Green;
                break;
            case ReceiverState.Playing:
                status = L.T("● 正在播放", "● Playing") + (senderName.Length > 0 ? L.T("：", ": ") + senderName : ""); color = Theme.Accent;
                break;
            case ReceiverState.Failed:
                status = L.T("● 无法启动", "● Could not start"); color = Theme.Red;
                break;
            default:
                status = L.T("● 已停止", "● Stopped"); color = Theme.Grey;
                break;
        }
        statusLabel.Text = status;
        statusLabel.ForeColor = color;
        ShowError(next == ReceiverState.Failed || next == ReceiverState.Starting ? failure : "");
        bool running = engine.Running;
        toggleButton.Text = running ? L.T("停止接收", "Stop receiving") : L.T("开始接收", "Start receiving");
        toggleButton.BackColor = running ? Color.White : Theme.Accent;
        toggleButton.ForeColor = running ? Theme.Text : Color.White;
        toggleButton.FlatAppearance.BorderColor = running ? Theme.Border : Theme.Accent;
        bool active = next == ReceiverState.Waiting || next == ReceiverState.Connected || next == ReceiverState.Playing;
        logo.Image = Theme.Logo(104, active || next == ReceiverState.Starting);
        tray.Icon = active ? activeIcon : idleIcon;
        string tip = L.T("AirPlay 2 音频接收器 - ", "AirPlay 2 Audio Receiver - ") + status.Substring(2);
        tray.Text = tip.Length > 63 ? tip.Substring(0, 63) : tip;
        trayStatus.Text = status.Substring(2);
        trayToggle.Text = running ? L.T("停止接收", "Stop receiving") : L.T("开始接收", "Start receiving");
    }

    private void ShowError(string text)
    {
        errorLabel.Text = text;
        errorLabel.Visible = text.Length > 0;
    }

    // ------------------------------------------------------------ dialogs

    private void ShowAdvanced()
    {
        ApplyName();
        ApplyPassword();
        using (AdvancedForm dialog = new AdvancedForm(settings)) {
            if (dialog.ShowDialog(this) != DialogResult.OK || !dialog.Changed) return;
            settings.Save();
            if (engine.Running) RestartReceiver();
        }
    }

    private void ShowMonitor()
    {
        if (monitorForm == null || monitorForm.IsDisposed) monitorForm = new MonitorForm(monitor, activeIcon);
        monitorForm.Show();
        if (monitorForm.WindowState == FormWindowState.Minimized) monitorForm.WindowState = FormWindowState.Normal;
        monitorForm.Activate();
    }

    private void ShowLog()
    {
        if (logForm == null || logForm.IsDisposed) logForm = new LogForm(activeIcon);
        logForm.Show();
        if (logForm.WindowState == FormWindowState.Minimized) logForm.WindowState = FormWindowState.Normal;
        logForm.Activate();
    }

    private void Post(MethodInvoker action)
    {
        if (IsDisposed || !IsHandleCreated) return;
        try { BeginInvoke(action); } catch (InvalidOperationException) { }
    }
}

// ---------------------------------------------------------------- advanced settings

internal sealed class NetworkChoice
{
    public string Name;
    public string Label;
    public override string ToString() { return Label; }
}

internal sealed class OutputFormat
{
    public int Rate;
    public int Bits;
    public override string ToString()
    {
        string rate = (Rate % 1000 == 0 ? (Rate / 1000).ToString(CultureInfo.InvariantCulture) : (Rate / 1000.0).ToString("0.0", CultureInfo.InvariantCulture)) + " kHz";
        string text = rate + " / " + Bits + L.T(" 位", "-bit");
        return Rate == 44100 && Bits == 16 ? text + L.T("（默认，不转换）", " (default, no conversion)") : text;
    }
    public static bool ValidRate(int rate)
    {
        return rate == 44100 || rate == 48000 || rate == 88200 || rate == 96000 || rate == 176400 || rate == 192000;
    }
    public static readonly OutputFormat[] All = {
        new OutputFormat { Rate = 44100, Bits = 16 }, new OutputFormat { Rate = 44100, Bits = 24 },
        new OutputFormat { Rate = 48000, Bits = 16 }, new OutputFormat { Rate = 48000, Bits = 24 },
        new OutputFormat { Rate = 88200, Bits = 24 }, new OutputFormat { Rate = 96000, Bits = 24 },
        new OutputFormat { Rate = 176400, Bits = 24 }, new OutputFormat { Rate = 192000, Bits = 24 },
        new OutputFormat { Rate = 44100, Bits = 32 }, new OutputFormat { Rate = 48000, Bits = 32 },
    };
}

// The output devices the engine can open, with the same names it matches
// against. Listing them starts the engine, which takes a moment, so the last
// list is kept and new scans run in the background.
internal static class OutputDevices
{
    private static readonly object gate = new object();
    private static List<string> cached;
    private static bool scanning;
    private static Action<List<string>> waiting;

    // null until the first scan finishes.
    public static List<string> Cached { get { lock (gate) return cached; } }

    // Starts a scan unless one is running; done (may be null) is called on a
    // worker thread with the result.
    public static void Refresh(Action<List<string>> done)
    {
        lock (gate) {
            if (done != null) waiting += done;
            if (scanning) return;
            scanning = true;
        }
        ThreadPool.QueueUserWorkItem(delegate {
            List<string> names = Scan();
            Action<List<string>> callbacks;
            lock (gate) {
                if (names != null) cached = names;
                else names = cached;
                callbacks = waiting;
                waiting = null;
                scanning = false;
            }
            if (callbacks != null) callbacks(names);
        });
    }

    private static List<string> Scan()
    {
        try {
            List<string> names = new List<string>();
            string exe = Path.Combine(AppDomain.CurrentDomain.BaseDirectory, "AirPlay2Engine.exe");
            ProcessStartInfo info = new ProcessStartInfo {
                FileName = exe, Arguments = "-list-devices", WorkingDirectory = AppDomain.CurrentDomain.BaseDirectory,
                UseShellExecute = false, CreateNoWindow = true, RedirectStandardOutput = true, StandardOutputEncoding = Encoding.UTF8,
            };
            using (Process p = Process.Start(info)) {
                string line;
                while ((line = p.StandardOutput.ReadLine()) != null) {
                    // DEVICE <index> <name>
                    if (!line.StartsWith("DEVICE ", StringComparison.Ordinal)) continue;
                    int space = line.IndexOf(' ', 7);
                    if (space > 0) names.Add(line.Substring(space + 1));
                }
                p.WaitForExit(3000);
            }
            return names;
        } catch (Exception) { return null; }
    }
}

internal sealed class AdvancedForm : Form
{
    private readonly Settings settings;
    private readonly ComboBox network = new ComboBox();
    private readonly CheckBox ipv4Only = new CheckBox();
    private readonly ComboBox device = new ComboBox();
    private readonly ComboBox format = new ComboBox();
    private readonly ComboBox dither = new ComboBox();
    private readonly NumericUpDown delay = new NumericUpDown();
    private readonly CheckBox synchronize = new CheckBox();
    private readonly ComboBox anchor = new ComboBox();
    private readonly CheckBox driftCorrection = new CheckBox();
    private readonly NumericUpDown driftTolerance = new NumericUpDown();
    private readonly NumericUpDown resyncThreshold = new NumericUpDown();
    private readonly NumericUpDown sessionTimeout = new NumericUpDown();
    private readonly NumericUpDown activeTimeout = new NumericUpDown();
    private readonly CheckBox refuseInterruption = new CheckBox();
    private readonly TextBox onPlayBegin = new TextBox(), onPlayEnd = new TextBox(), onActiveBegin = new TextBox(), onActiveEnd = new TextBox();
    private readonly NumericUpDown port = new NumericUpDown();
    private readonly NumericUpDown eventPort = new NumericUpDown();
    private readonly NumericUpDown udpPortBase = new NumericUpDown();
    private readonly NumericUpDown udpPortRange = new NumericUpDown();
    private readonly NumericUpDown deviceIdOffset = new NumericUpDown();
    private readonly CheckBox trace = new CheckBox();
    private readonly CheckBox statistics = new CheckBox();
    private List<string> shownDevices;
    public bool Changed { get; private set; }

    public AdvancedForm(Settings s)
    {
        settings = s;
        SuspendLayout();
        Text = L.T("高级设置", "Advanced settings");
        Font = Theme.Regular(10f);
        BackColor = Theme.Window;
        FormBorderStyle = FormBorderStyle.FixedDialog;
        MaximizeBox = false; MinimizeBox = false; ShowInTaskbar = false;
        StartPosition = FormStartPosition.CenterParent;
        AutoSize = true; AutoSizeMode = AutoSizeMode.GrowAndShrink;
        Padding = new Padding(0, 0, 18, 14);

        TableLayoutPanel root = new TableLayoutPanel { ColumnCount = 2, AutoSize = true, Location = new Point(18, 14) };
        root.ColumnStyles.Add(new ColumnStyle(SizeType.AutoSize));
        root.ColumnStyles.Add(new ColumnStyle(SizeType.AutoSize));
        Controls.Add(root);
        TableLayoutPanel left = Column(new Padding(0));
        TableLayoutPanel right = Column(new Padding(18, 0, 0, 0));
        root.Controls.Add(left, 0, 0);
        root.Controls.Add(right, 1, 0);

        // ---- left: network, playback, output, troubleshooting

        TableLayoutPanel g = Section(left, L.T("网络", "Network"));
        network.DropDownStyle = ComboBoxStyle.DropDownList;
        network.Dock = DockStyle.Fill;
        network.Margin = new Padding(0, 4, 8, 4);
        Button refresh = Theme.MakeButton(L.T("刷新", "Refresh"), false);
        refresh.Margin = new Padding(0, 1, 0, 1);
        refresh.Click += delegate { LoadInterfaces(SelectedInterface()); };
        TableLayoutPanel netRow = new TableLayoutPanel { ColumnCount = 2, AutoSize = true, Dock = DockStyle.Fill, BackColor = Theme.Card, Margin = new Padding(0) };
        netRow.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100));
        netRow.ColumnStyles.Add(new ColumnStyle(SizeType.AutoSize));
        netRow.Controls.Add(network, 0, 0);
        netRow.Controls.Add(refresh, 1, 0);
        Row(g, null, netRow, null);
        Row(g, null, Note(L.T("选择监听的网卡", "Choose the network adapter to listen on")), null);
        Row(g, null, Check(ipv4Only, L.T("只使用 IPv4", "Use IPv4 only"), s.IPv4Only), null);

        g = Section(left, L.T("播放", "Playback"));
        Row(g, null, Check(synchronize, L.T("按发送端时间同步播放（推荐）", "Sync playback to the sender's clock (recommended)"), s.Synchronize), null);
        Row(g, null, Note(L.T("关闭后延迟低，但可能出现音画不同步", "Lower latency when off, but sound may drift out of sync with video")), null);
        Number(delay, -500, 500, s.DelayMs, 0, 10);
        Row(g, L.T("输出延迟补偿", "Output delay"), delay, L.T("毫秒", "ms"));
        Row(g, null, Note(L.T("如果声音比画面慢，可尝试增加补偿", "If sound lags behind video, try increasing the delay")), null);
        anchor.DropDownStyle = ComboBoxStyle.DropDownList; anchor.Width = 220;
        Choose(anchor, s.RealtimeAnchor, new NetworkChoice { Name = "shairport", Label = L.T("标准（与其他音箱同步）", "Standard (multi-room sync)") },
            new NetworkChoice { Name = "low-latency", Label = L.T("低延迟（约早 1.75 秒）", "Low latency (~1.75 s earlier)") });
        Row(g, L.T("实时流定时", "Realtime timing"), anchor, null);
        Row(g, null, Note(L.T("iOS 有声音明显滞后可使用低延迟模式", "If sound clearly lags on iOS, use low latency")), null);

        g = Section(left, L.T("音频输出", "Audio output"));
        device.DropDownStyle = ComboBoxStyle.DropDownList; device.Dock = DockStyle.Fill;
        format.DropDownStyle = ComboBoxStyle.DropDownList; format.Dock = DockStyle.Fill;
        Row(g, L.T("设备", "Device"), device, null);
        Row(g, L.T("格式", "Format"), format, null);
        foreach (OutputFormat f in OutputFormat.All) {
            format.Items.Add(f);
            if (f.Rate == s.OutputRate && f.Bits == s.OutputBits) format.SelectedItem = f;
        }
        if (format.SelectedIndex < 0) format.SelectedIndex = 0;
        dither.DropDownStyle = ComboBoxStyle.DropDownList; dither.Dock = DockStyle.Fill;
        Choose(dither, s.Dither, new NetworkChoice { Name = "auto", Label = L.T("自动（16 位且调过音量或重采样时）", "Auto (16-bit with volume change or resampling)") },
            new NetworkChoice { Name = "on", Label = L.T("始终加入", "Always") }, new NetworkChoice { Name = "off", Label = L.T("关闭", "Off") });
        Row(g, L.T("抖动", "Dither"), dither, null);

        g = Section(left, L.T("故障排查", "Troubleshooting"));
        Row(g, null, Check(trace, L.T("记录详细的协议日志", "Log detailed protocol messages"), s.TraceLog), null);
        Row(g, null, Check(statistics, L.T("每 10 秒记录同步误差、漂移校正和丢包统计", "Log sync error, drift correction and packet loss every 10 seconds"), s.Statistics), null);
        Button folder = Theme.MakeButton(L.T("打开数据文件夹", "Open data folder"), false);
        folder.Click += delegate {
            try { Directory.CreateDirectory(Settings.Directory); Process.Start("explorer.exe", "\"" + Settings.Directory + "\""); } catch (Exception) { }
        };
        Row(g, null, folder, null);
        folder.Margin = new Padding(0, 8, 0, 0);

        // ---- right: drift correction, sessions, event commands, ports

        g = Section(right, L.T("漂移校正", "Drift correction"));
        Row(g, null, Check(driftCorrection, L.T("逐个增减采样，平滑校正时钟漂移（推荐）", "Smooth drift correction, one sample at a time (recommended)"), s.DriftCorrection), null);
        Number(driftTolerance, 0.1m, 1000, (decimal)s.DriftToleranceMs, 1, 0.5m);
        Row(g, L.T("校正容差", "Tolerance"), driftTolerance, L.T("毫秒", "ms"));
        Number(resyncThreshold, 1, 10000, (decimal)s.ResyncThresholdMs, 0, 10);
        Row(g, L.T("跳变阈值", "Resync threshold"), resyncThreshold, L.T("毫秒", "ms"));
        Row(g, null, Note(L.T("误差超过容差时开始校正；持续超过跳变阈值（例如声卡欠载）时直接跳过或补静音。", "Correction starts when the error exceeds the tolerance. If it stays above the resync threshold (for example on a sound card underrun), audio is skipped or padded with silence.")), null);

        g = Section(right, L.T("会话", "Sessions"));
        Number(sessionTimeout, 0, 86400, s.SessionTimeout, 0, 10);
        Row(g, L.T("无响应断开", "Idle disconnect"), sessionTimeout, L.T("秒（0 不限）", "s (0 = never)"));
        Number(activeTimeout, 0, 86400, s.ActiveTimeout, 0, 5);
        Row(g, L.T("停止播放后", "Release device after"), activeTimeout, L.T("秒释放设备（0 不自动释放）", "s idle (0 = never)"));
        Row(g, null, Check(refuseInterruption, L.T("有设备连接时拒绝其他设备", "Refuse other devices while one is connected"), s.RefuseInterruption), null);
        Row(g, null, Note(L.T("不勾选时，后开始播放的设备会接管输出，之前的设备自动暂停。", "When unchecked, the device that starts playing last takes over, and the previous one pauses.")), null);

        g = Section(right, L.T("事件命令", "Event commands"));
        Command(g, L.T("开始播放", "Playback starts"), onPlayBegin, s.OnPlayBegin);
        Command(g, L.T("停止播放", "Playback stops"), onPlayEnd, s.OnPlayEnd);
        Command(g, L.T("打开输出设备", "Output device opened"), onActiveBegin, s.OnActiveBegin);
        Command(g, L.T("释放输出设备", "Output device released"), onActiveEnd, s.OnActiveEnd);
        Row(g, null, Note(L.T("通过 cmd.exe 运行，环境变量 AIRPLAY_EVENT 为事件名。留空不运行。", "Run with cmd.exe; the AIRPLAY_EVENT environment variable holds the event name. Leave empty to run nothing.")), null);

        g = Section(right, L.T("端口与多实例", "Ports and multiple instances"));
        Number(port, 1, 65535, s.Port, 0, 1);
        Row(g, L.T("控制端口", "Control port"), port, "TCP");
        Number(eventPort, 1, 65535, s.EventPort, 0, 1);
        Row(g, L.T("事件端口", "Event port"), eventPort, "TCP");
        Number(udpPortBase, 0, 65535, s.UdpPortBase, 0, 1);
        Row(g, L.T("音频端口起点", "First audio port"), udpPortBase, L.T("0 由系统分配", "0 = chosen by the system"));
        Number(udpPortRange, 3, 65535, s.UdpPortRange, 0, 1);
        Row(g, L.T("音频端口数量", "Audio port count"), udpPortRange, L.T("个", "ports"));
        Number(deviceIdOffset, 0, 999, s.DeviceIdOffset, 0, 1);
        Row(g, L.T("设备 ID 偏移", "Device ID offset"), deviceIdOffset, null);
        Row(g, null, Note(L.T("修改以避免多实例间冲突", "Change these to avoid conflicts between instances")), null);

        synchronize.CheckedChanged += delegate { UpdateEnabled(); };
        driftCorrection.CheckedChanged += delegate { UpdateEnabled(); };
        udpPortBase.ValueChanged += delegate { UpdateEnabled(); };
        UpdateEnabled();

        FlowLayoutPanel buttons = new FlowLayoutPanel { AutoSize = true, Anchor = AnchorStyles.Right, FlowDirection = FlowDirection.RightToLeft, Margin = new Padding(0) };
        Button cancel = Theme.MakeButton(L.T("取消", "Cancel"), false);
        cancel.DialogResult = DialogResult.Cancel;
        Button ok = Theme.MakeButton(L.T("确定", "OK"), true);
        ok.Click += delegate { if (Apply()) DialogResult = DialogResult.OK; };
        cancel.Margin = new Padding(8, 0, 0, 0); ok.Margin = new Padding(8, 0, 0, 0);
        cancel.MinimumSize = ok.MinimumSize = new Size(88, 34);
        buttons.Controls.Add(cancel); buttons.Controls.Add(ok);
        root.Controls.Add(buttons, 0, 1);
        root.SetColumnSpan(buttons, 2);
        AcceptButton = ok; CancelButton = cancel;

        AutoScaleDimensions = new SizeF(96F, 96F);
        AutoScaleMode = AutoScaleMode.Dpi;
        ResumeLayout(true);
        LoadInterfaces(s.NetworkInterface);
        ShowDevices(OutputDevices.Cached, s.OutputDevice);
    }

    // On a small or highly scaled screen, fit the dialog and scroll instead.
    protected override void OnLoad(EventArgs e)
    {
        base.OnLoad(e);
        Rectangle area = Screen.FromControl(Owner != null ? (Control)Owner : this).WorkingArea;
        if (Height <= area.Height) return;
        Size size = Size;
        AutoSize = false;
        AutoScroll = true;
        AutoScrollMargin = new Size(0, 14);
        Size = new Size(Math.Min(area.Width, size.Width + SystemInformation.VerticalScrollBarWidth), area.Height);
        Location = new Point(Math.Max(area.Left, Math.Min(Left, area.Right - Width)), area.Top);
    }

    protected override void OnShown(EventArgs e)
    {
        base.OnShown(e);
        RefreshDevices(settings.OutputDevice);
    }

    private void UpdateEnabled()
    {
        bool sync = synchronize.Checked;
        anchor.Enabled = sync;
        driftCorrection.Enabled = sync;
        driftTolerance.Enabled = sync && driftCorrection.Checked;
        resyncThreshold.Enabled = sync;
        udpPortRange.Enabled = udpPortBase.Value > 0;
    }

    private static TableLayoutPanel Column(Padding margin)
    {
        TableLayoutPanel column = new TableLayoutPanel { ColumnCount = 1, AutoSize = true, Margin = margin, Padding = new Padding(0) };
        column.ColumnStyles.Add(new ColumnStyle(SizeType.Absolute, 460));
        return column;
    }

    // A titled card holding a two-column grid: caption, field.
    private static TableLayoutPanel Section(TableLayoutPanel column, string title)
    {
        column.Controls.Add(Title(title));
        Card card = new Card { Margin = new Padding(0, 0, 0, 14) };
        TableLayoutPanel grid = new TableLayoutPanel { ColumnCount = 2, AutoSize = true, Dock = DockStyle.Top, BackColor = Theme.Card };
        grid.ColumnStyles.Add(new ColumnStyle(SizeType.AutoSize));
        grid.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100));
        card.Controls.Add(grid);
        column.Controls.Add(card);
        return grid;
    }

    // Adds a row; without a caption the field spans both columns, with a
    // unit the field keeps its own width and the unit follows it.
    private static void Row(TableLayoutPanel grid, string caption, Control field, string unit)
    {
        int row = grid.RowCount;
        grid.RowCount = row + 1;
        grid.RowStyles.Add(new RowStyle(SizeType.AutoSize));
        if (caption == null) {
            grid.Controls.Add(field, 0, row);
            grid.SetColumnSpan(field, 2);
            return;
        }
        grid.Controls.Add(new Label { Text = caption, AutoSize = true, Margin = new Padding(0, 8, 10, 0) }, 0, row);
        field.Margin = new Padding(0, 4, 0, 4);
        if (unit == null) { grid.Controls.Add(field, 1, row); return; }
        FlowLayoutPanel cell = new FlowLayoutPanel { AutoSize = true, WrapContents = false, BackColor = Theme.Card, Margin = new Padding(0) };
        cell.Controls.Add(field);
        cell.Controls.Add(new Label { Text = unit, AutoSize = true, ForeColor = Theme.SubText, Margin = new Padding(6, 8, 0, 0) });
        grid.Controls.Add(cell, 1, row);
    }

    private static CheckBox Check(CheckBox box, string text, bool value)
    {
        box.Text = text;
        box.AutoSize = true; box.BackColor = Theme.Card;
        box.Margin = new Padding(0, 4, 0, 4);
        box.Checked = value;
        return box;
    }

    private static void Number(NumericUpDown n, decimal min, decimal max, decimal value, int decimals, decimal increment)
    {
        n.Minimum = min; n.Maximum = max; n.DecimalPlaces = decimals; n.Increment = increment; n.Width = 90;
        n.Value = Math.Max(min, Math.Min(max, value));
    }

    private static void Choose(ComboBox box, string selected, params NetworkChoice[] choices)
    {
        foreach (NetworkChoice c in choices) {
            box.Items.Add(c);
            if (c.Name == selected) box.SelectedItem = c;
        }
        if (box.SelectedIndex < 0) box.SelectedIndex = 0;
    }

    private static void Command(TableLayoutPanel grid, string caption, TextBox box, string value)
    {
        box.Text = value;
        box.Dock = DockStyle.Fill;
        Row(grid, caption, box, null);
    }

    private static string Chosen(ComboBox box, string fallback)
    {
        NetworkChoice choice = box.SelectedItem as NetworkChoice;
        return choice == null ? fallback : choice.Name;
    }

    // The list is filled from the cache at once, and again in OnShown when a
    // fresh scan finishes.
    private void RefreshDevices(string selectName)
    {
        OutputDevices.Refresh(delegate(List<string> names) {
            if (IsDisposed || !IsHandleCreated) return;
            try {
                BeginInvoke((MethodInvoker)delegate {
                    if (!IsDisposed && !SameDevices(names)) ShowDevices(names, Chosen(device, selectName));
                });
            } catch (InvalidOperationException) { }
        });
    }

    private bool SameDevices(List<string> names)
    {
        if (names == null || shownDevices == null || names.Count != shownDevices.Count) return names == shownDevices;
        for (int i = 0; i < names.Count; i++) if (names[i] != shownDevices[i]) return false;
        return true;
    }

    private void ShowDevices(List<string> names, string selectName)
    {
        shownDevices = names;
        device.BeginUpdate();
        device.Items.Clear();
        device.Items.Add(new NetworkChoice { Name = "", Label = L.T("Windows 默认设备（跟随系统切换）", "Windows default device (follows the system)") });
        int selected = 0;
        if (names != null) {
            foreach (string name in names) {
                device.Items.Add(new NetworkChoice { Name = name, Label = name });
                if (name == selectName) selected = device.Items.Count - 1;
            }
        }
        if (selected == 0 && !String.IsNullOrEmpty(selectName)) {
            // While the first scan runs, the saved device is shown without a note.
            string label = names == null ? selectName : selectName + L.T("（当前不可用）", " (not available)");
            device.Items.Add(new NetworkChoice { Name = selectName, Label = label });
            selected = device.Items.Count - 1;
        }
        device.SelectedIndex = selected;
        device.EndUpdate();
    }

    private static Label Title(string text)
    {
        return new Label { Text = text, Font = Theme.Bold(10.5f), AutoSize = true, Margin = new Padding(2, 0, 0, 6) };
    }

    private static Label Note(string text)
    {
        return new Label { Text = text, AutoSize = true, MaximumSize = new Size(420, 0), ForeColor = Theme.SubText, BackColor = Theme.Card, Margin = new Padding(0, 4, 0, 2) };
    }

    private string SelectedInterface()
    {
        NetworkChoice choice = network.SelectedItem as NetworkChoice;
        return choice == null ? settings.NetworkInterface : choice.Name;
    }

    private void LoadInterfaces(string selectName)
    {
        List<NetworkChoice> choices = new List<NetworkChoice>();
        choices.Add(new NetworkChoice { Name = "", Label = L.T("自动选择", "Automatic") });
        try {
            foreach (NetworkInterface nic in NetworkInterface.GetAllNetworkInterfaces()) {
                if (nic.OperationalStatus != OperationalStatus.Up || nic.NetworkInterfaceType == NetworkInterfaceType.Loopback) continue;
                foreach (UnicastIPAddressInformation address in nic.GetIPProperties().UnicastAddresses) {
                    IPAddress ip = address.Address;
                    byte[] bytes = ip.GetAddressBytes();
                    if (ip.AddressFamily == AddressFamily.InterNetwork && !IPAddress.IsLoopback(ip) && !(bytes[0] == 169 && bytes[1] == 254)) {
                        choices.Add(new NetworkChoice { Name = nic.Name, Label = nic.Name + L.T("（", " (") + ip + L.T("）", ")") });
                        break;
                    }
                }
            }
        } catch (NetworkInformationException) { }
        bool found = false;
        foreach (NetworkChoice c in choices) if (c.Name == selectName) found = true;
        if (!found && !String.IsNullOrEmpty(selectName)) choices.Add(new NetworkChoice { Name = selectName, Label = selectName + L.T("（当前不可用）", " (not available)") });
        network.Items.Clear();
        int selected = 0;
        for (int i = 0; i < choices.Count; i++) { network.Items.Add(choices[i]); if (choices[i].Name == selectName) selected = i; }
        network.SelectedIndex = selected;
    }

    // Copies the dialog into the settings; false (nothing changed) if a value is invalid.
    private bool Apply()
    {
        string error = null;
        if (port.Value == eventPort.Value) error = L.T("控制端口和事件端口不能相同。", "The control port and event port must be different.");
        else if (udpPortBase.Value > 0 && udpPortBase.Value + udpPortRange.Value - 1 > 65535) error = L.T("音频端口范围超出了 65535。", "The audio port range goes beyond 65535.");
        else if (driftTolerance.Value >= resyncThreshold.Value) error = L.T("漂移校正容差必须小于跳变阈值。", "The drift correction tolerance must be smaller than the resync threshold.");
        if (error != null) {
            MessageBox.Show(this, error, L.T("高级设置", "Advanced settings"), MessageBoxButtons.OK, MessageBoxIcon.Warning);
            return false;
        }

        Settings n = new Settings();
        n.NetworkInterface = SelectedInterface();
        n.IPv4Only = ipv4Only.Checked;
        n.DelayMs = (int)delay.Value;
        n.Synchronize = synchronize.Checked;
        n.RealtimeAnchor = Chosen(anchor, settings.RealtimeAnchor);
        n.OutputDevice = Chosen(device, settings.OutputDevice);
        OutputFormat outputFormat = format.SelectedItem as OutputFormat ?? OutputFormat.All[0];
        n.OutputRate = outputFormat.Rate;
        n.OutputBits = outputFormat.Bits;
        n.Dither = Chosen(dither, settings.Dither);
        n.TraceLog = trace.Checked;
        n.Statistics = statistics.Checked;
        n.DriftCorrection = driftCorrection.Checked;
        n.DriftToleranceMs = (double)driftTolerance.Value;
        n.ResyncThresholdMs = (double)resyncThreshold.Value;
        n.SessionTimeout = (int)sessionTimeout.Value;
        n.ActiveTimeout = (int)activeTimeout.Value;
        n.RefuseInterruption = refuseInterruption.Checked;
        n.OnPlayBegin = onPlayBegin.Text.Trim();
        n.OnPlayEnd = onPlayEnd.Text.Trim();
        n.OnActiveBegin = onActiveBegin.Text.Trim();
        n.OnActiveEnd = onActiveEnd.Text.Trim();
        n.Port = (int)port.Value;
        n.EventPort = (int)eventPort.Value;
        n.UdpPortBase = (int)udpPortBase.Value;
        n.UdpPortRange = (int)udpPortRange.Value;
        n.DeviceIdOffset = (int)deviceIdOffset.Value;

        Changed = n.NetworkInterface != settings.NetworkInterface || n.IPv4Only != settings.IPv4Only
            || n.DelayMs != settings.DelayMs || n.Synchronize != settings.Synchronize || n.RealtimeAnchor != settings.RealtimeAnchor
            || n.OutputDevice != settings.OutputDevice || n.OutputRate != settings.OutputRate || n.OutputBits != settings.OutputBits
            || n.Dither != settings.Dither
            || n.TraceLog != settings.TraceLog || n.Statistics != settings.Statistics
            || n.DriftCorrection != settings.DriftCorrection || n.DriftToleranceMs != settings.DriftToleranceMs
            || n.ResyncThresholdMs != settings.ResyncThresholdMs
            || n.SessionTimeout != settings.SessionTimeout || n.ActiveTimeout != settings.ActiveTimeout
            || n.RefuseInterruption != settings.RefuseInterruption
            || n.OnPlayBegin != settings.OnPlayBegin || n.OnPlayEnd != settings.OnPlayEnd
            || n.OnActiveBegin != settings.OnActiveBegin || n.OnActiveEnd != settings.OnActiveEnd
            || n.Port != settings.Port || n.EventPort != settings.EventPort
            || n.UdpPortBase != settings.UdpPortBase || n.UdpPortRange != settings.UdpPortRange
            || n.DeviceIdOffset != settings.DeviceIdOffset;

        settings.NetworkInterface = n.NetworkInterface;
        settings.IPv4Only = n.IPv4Only;
        settings.DelayMs = n.DelayMs;
        settings.Synchronize = n.Synchronize;
        settings.RealtimeAnchor = n.RealtimeAnchor;
        settings.OutputDevice = n.OutputDevice;
        settings.OutputRate = n.OutputRate;
        settings.OutputBits = n.OutputBits;
        settings.Dither = n.Dither;
        settings.TraceLog = n.TraceLog;
        settings.Statistics = n.Statistics;
        settings.DriftCorrection = n.DriftCorrection;
        settings.DriftToleranceMs = n.DriftToleranceMs;
        settings.ResyncThresholdMs = n.ResyncThresholdMs;
        settings.SessionTimeout = n.SessionTimeout;
        settings.ActiveTimeout = n.ActiveTimeout;
        settings.RefuseInterruption = n.RefuseInterruption;
        settings.OnPlayBegin = n.OnPlayBegin;
        settings.OnPlayEnd = n.OnPlayEnd;
        settings.OnActiveBegin = n.OnActiveBegin;
        settings.OnActiveEnd = n.OnActiveEnd;
        settings.Port = n.Port;
        settings.EventPort = n.EventPort;
        settings.UdpPortBase = n.UdpPortBase;
        settings.UdpPortRange = n.UdpPortRange;
        settings.DeviceIdOffset = n.DeviceIdOffset;
        return true;
    }
}

// ---------------------------------------------------------------- playback monitor

// The engine's once-a-second "@stats key=value ..." report, plus totals
// since the sender connected. Updated on the UI thread.
internal sealed class PlaybackMonitor
{
    public string Mode = "idle", Codec = "-";
    public long Rate, Bits, Kbps; // source sample rate (Hz), sample size (0 if lossy or unknown), measured bit rate
    public bool Synchronized = true, ClockLocked, HaveData;
    public long LatencyMs, DelayMs;
    public double BufferMs;
    public bool HaveError;
    public double ErrMs, ErrMinMs, ErrMaxMs, SmoothedMs;
    public long Output, Silent, Added, Removed, Resyncs, Dropped, CatchUp;
    public bool HaveLead;
    public long LeadMin, LeadMax;
    public long Rx, Miss, Late, Req, Rec, Full;
    public long TotalRx, TotalMiss, TotalLate, TotalReq, TotalRec, TotalFull, TotalResyncs, TotalDropped;
    public string OutputDevice = "", OutputFormat = "";
    public event Action Changed;

    public bool Streaming { get { return Mode != "idle"; } }

    public void Update(string line)
    {
        Dictionary<string, string> v = new Dictionary<string, string>();
        foreach (string field in line.Split(' ')) {
            int eq = field.IndexOf('=');
            if (eq > 0) v[field.Substring(0, eq)] = field.Substring(eq + 1);
        }
        Mode = Str(v, "mode", "idle");
        Codec = Str(v, "codec", "-");
        Rate = Int(v, "rate"); Bits = Int(v, "bits"); Kbps = Int(v, "kbps");
        Synchronized = Str(v, "sync", "true") == "true";
        ClockLocked = Str(v, "clock", "false") == "true";
        LatencyMs = Int(v, "latency"); DelayMs = Int(v, "delay"); BufferMs = Num(v, "buffer");
        HaveError = v.ContainsKey("err");
        ErrMs = Num(v, "err"); ErrMinMs = Num(v, "errmin"); ErrMaxMs = Num(v, "errmax"); SmoothedMs = Num(v, "smoothed");
        Output = Int(v, "output"); Silent = Int(v, "silent");
        Added = Int(v, "added"); Removed = Int(v, "removed"); Resyncs = Int(v, "resyncs");
        Dropped = Int(v, "dropped"); CatchUp = Int(v, "catchup");
        HaveLead = v.ContainsKey("leadmin");
        LeadMin = Int(v, "leadmin"); LeadMax = Int(v, "leadmax");
        Rx = Int(v, "rx"); Miss = Int(v, "miss"); Late = Int(v, "late");
        Req = Int(v, "req"); Rec = Int(v, "rec"); Full = Int(v, "full");
        TotalRx += Rx; TotalMiss += Miss; TotalLate += Late; TotalReq += Req; TotalRec += Rec; TotalFull += Full;
        TotalResyncs += Resyncs; TotalDropped += Dropped;
        HaveData = true;
        Raise();
    }

    // A new sender: start the totals again.
    public void ResetTotals()
    {
        TotalRx = TotalMiss = TotalLate = TotalReq = TotalRec = TotalFull = TotalResyncs = TotalDropped = 0;
        Raise();
    }

    // The engine (re)started.
    public void Clear()
    {
        Mode = "idle"; Codec = "-"; Rate = Bits = Kbps = 0; HaveData = false; HaveError = false; HaveLead = false;
        Output = Silent = Rx = Miss = 0;
        OutputDevice = OutputFormat = "";
        ResetTotals();
    }

    public void SetOutput(string device, string format)
    {
        OutputDevice = device; OutputFormat = format;
        Raise();
    }

    private void Raise() { Action handler = Changed; if (handler != null) handler(); }

    private static string Str(Dictionary<string, string> v, string key, string fallback)
    {
        string s; return v.TryGetValue(key, out s) ? s : fallback;
    }
    private static long Int(Dictionary<string, string> v, string key)
    {
        string s; long n;
        return v.TryGetValue(key, out s) && Int64.TryParse(s, NumberStyles.AllowLeadingSign, CultureInfo.InvariantCulture, out n) ? n : 0;
    }
    private static double Num(Dictionary<string, string> v, string key)
    {
        string s; double n;
        return v.TryGetValue(key, out s) && Double.TryParse(s, NumberStyles.Float, CultureInfo.InvariantCulture, out n) ? n : 0;
    }

    // ---- wording shared by the main window and the monitor window

    public string ModeText()
    {
        switch (Mode) {
            case "buffered": return L.T("AirPlay 2 缓冲模式", "AirPlay 2 buffered");
            case "realtime": return L.T("AirPlay 2 实时模式", "AirPlay 2 realtime");
            case "classic": return "AirPlay 1";
            default: return L.T("未在接收音频", "Not receiving audio");
        }
    }

    public string CodecText()
    {
        switch (Codec) {
            case "aac": return L.T("AAC（TCP 传输）", "AAC (over TCP)");
            case "alac": return L.T("ALAC 无损（UDP 传输）", "ALAC lossless (over UDP)");
            case "l16": return L.T("PCM 无压缩（UDP 传输）", "Uncompressed PCM (over UDP)");
            default: return "—";
        }
    }

    // "48 kHz", "44.1 kHz / 16-bit" or "" while the first packet is pending.
    public string RateText()
    {
        if (Rate <= 0) return "";
        string text = (Rate / 1000.0).ToString("0.###", CultureInfo.InvariantCulture) + " kHz";
        return Bits > 0 ? text + " / " + Bits + "-bit" : text;
    }

    // Measured from the audio received, so it varies with the music.
    public string BitrateText()
    {
        if (Kbps <= 0) return "";
        return Kbps >= 1000 ? (Kbps / 1000.0).ToString("0.00", CultureInfo.InvariantCulture) + " Mbps" : Kbps + " kbps";
    }

    // Packets lost for good, as a share of the packets expected.
    public static double LossPercent(long received, long missing)
    {
        long expected = received + missing;
        return expected == 0 ? 0 : missing * 100.0 / expected;
    }

    public bool HasNetworkCounts { get { return Mode == "realtime" || Mode == "classic"; } }
}

internal sealed class MonitorForm : Form
{
    private readonly PlaybackMonitor model;
    private readonly Action onChanged;
    private readonly Label mode = new Label(), codec = new Label(), rate = new Label(), bitrate = new Label(), sync = new Label(), clock = new Label(), output = new Label();
    private readonly Label latency = new Label(), latencyCaption = new Label(), delay = new Label(), buffer = new Label(), error = new Label(), correction = new Label(), resyncs = new Label();
    private readonly Label packets = new Label(), loss = new Label(), resend = new Label(), late = new Label(), lead = new Label(), underrun = new Label();

    public MonitorForm(PlaybackMonitor model, Icon icon)
    {
        this.model = model;
        SuspendLayout();
        Text = L.T("播放监控", "Playback monitor");
        Icon = icon;
        Font = Theme.Regular(10f);
        BackColor = Theme.Window;
        FormBorderStyle = FormBorderStyle.FixedSingle;
        MaximizeBox = false;
        StartPosition = FormStartPosition.CenterScreen;
        AutoSize = true; AutoSizeMode = AutoSizeMode.GrowAndShrink;
        Padding = new Padding(0, 0, 18, 14);

        TableLayoutPanel root = new TableLayoutPanel { ColumnCount = 1, AutoSize = true, Location = new Point(18, 14) };
        root.ColumnStyles.Add(new ColumnStyle(SizeType.Absolute, 460));
        Controls.Add(root);

        root.Controls.Add(Title(L.T("播放模式", "Playback mode")));
        TableLayoutPanel g = Section(root);
        Row(g, L.T("模式", "Mode"), mode);
        Row(g, L.T("编码", "Codec"), codec);
        Row(g, L.T("采样率", "Sample rate"), rate);
        Row(g, L.T("码率", "Bit rate"), bitrate);
        Row(g, L.T("播放方式", "Timing"), sync);
        Row(g, L.T("时钟同步", "Clock sync"), clock);
        Row(g, L.T("输出设备", "Output device"), output);

        root.Controls.Add(Title(L.T("延迟与同步", "Latency and sync")));
        g = Section(root);
        latencyCaption.Text = L.T("发送端延迟", "Sender latency");
        Row(g, latencyCaption, latency);
        Row(g, L.T("输出延迟补偿", "Output delay"), delay);
        Row(g, L.T("已缓冲音频", "Buffered audio"), buffer);
        Row(g, L.T("同步误差", "Sync error"), error);
        Row(g, L.T("漂移校正", "Drift correction"), correction);
        Row(g, L.T("重新对齐", "Resyncs"), resyncs);

        root.Controls.Add(Title(L.T("网络", "Network")));
        g = Section(root);
        Row(g, L.T("收包", "Packets"), packets);
        Row(g, L.T("丢包", "Lost"), loss);
        Row(g, L.T("重传", "Resent"), resend);
        Row(g, L.T("迟到/重复", "Late/duplicate"), late);
        Row(g, L.T("到达提前量", "Arrival lead"), lead);
        Row(g, L.T("输出", "Output"), underrun);

        AutoScaleDimensions = new SizeF(96F, 96F);
        AutoScaleMode = AutoScaleMode.Dpi;
        ResumeLayout(true);

        onChanged = delegate { if (!IsDisposed) UpdateView(); };
        model.Changed += onChanged;
        UpdateView();
    }

    protected override void OnFormClosed(FormClosedEventArgs e)
    {
        model.Changed -= onChanged;
        base.OnFormClosed(e);
    }

    private static Label Title(string text)
    {
        return new Label { Text = text, Font = Theme.Bold(10.5f), AutoSize = true, Margin = new Padding(2, 0, 0, 6) };
    }

    private static TableLayoutPanel Section(TableLayoutPanel root)
    {
        Card card = new Card { Margin = new Padding(0, 0, 0, 14) };
        TableLayoutPanel grid = new TableLayoutPanel { ColumnCount = 2, AutoSize = true, Dock = DockStyle.Top, BackColor = Theme.Card };
        grid.ColumnStyles.Add(new ColumnStyle(SizeType.Absolute, L.Zh ? 110 : 140));
        grid.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100));
        card.Controls.Add(grid);
        root.Controls.Add(card);
        return grid;
    }

    private static void Row(TableLayoutPanel grid, string caption, Label value)
    {
        Row(grid, new Label { Text = caption }, value);
    }

    private static void Row(TableLayoutPanel grid, Label caption, Label value)
    {
        caption.AutoSize = true;
        caption.ForeColor = Theme.SubText;
        caption.Margin = new Padding(0, 4, 8, 4);
        value.AutoSize = true;
        value.MaximumSize = new Size(L.Zh ? 320 : 290, 0);
        value.Margin = new Padding(0, 4, 0, 4);
        int row = grid.RowCount;
        grid.RowCount = row + 1;
        grid.RowStyles.Add(new RowStyle(SizeType.AutoSize));
        grid.Controls.Add(caption, 0, row);
        grid.Controls.Add(value, 1, row);
    }

    private static void Set(Label label, string text, Color color)
    {
        label.Text = text;
        label.ForeColor = color;
    }

    private static string Ms(double value) { return value.ToString("+0.00;-0.00;0.00", CultureInfo.InvariantCulture) + " ms"; }

    private void UpdateView()
    {
        PlaybackMonitor m = model;
        bool streaming = m.HaveData && m.Streaming;
        string none = "—";

        Set(mode, m.HaveData ? m.ModeText() : L.T("接收器未运行", "Receiver not running"), streaming ? Theme.Accent : Theme.Grey);
        Set(codec, streaming ? m.CodecText() : none, Theme.Text);
        Set(rate, streaming && m.RateText().Length > 0 ? m.RateText() : none, Theme.Text);
        Set(bitrate, streaming && m.BitrateText().Length > 0 ? m.BitrateText() : none, Theme.Text);
        Set(sync, m.Synchronized ? L.T("按发送端时间同步", "Synced to the sender's clock") : L.T("低延迟（不按时间同步）", "Low latency (not synced)"), Theme.Text);
        if (!streaming) Set(clock, none, Theme.Text);
        else if (!m.Synchronized) Set(clock, L.T("不需要", "Not needed"), Theme.Grey);
        else if (m.ClockLocked) Set(clock, m.Mode == "classic" ? L.T("已锁定（NTP）", "Locked (NTP)") : L.T("已锁定（PTP）", "Locked (PTP)"), Theme.Green);
        else Set(clock, L.T("等待时钟同步…", "Waiting for clock sync…"), Theme.Orange);
        string device = m.OutputDevice.Length > 0 ? m.OutputDevice : L.T("Windows 默认设备", "Windows default device");
        Set(output, m.OutputFormat.Length > 0 ? device + L.T("，", ", ") + m.OutputFormat : device, Theme.Text);

        latencyCaption.Text = m.Mode == "buffered" ? L.T("调度提前量", "Scheduling lead") : L.T("发送端延迟", "Sender latency");
        Set(latency, streaming && m.LatencyMs != 0 ? m.LatencyMs + " ms" : none, Theme.Text);
        Set(delay, (m.DelayMs > 0 ? "+" : "") + m.DelayMs + " ms", Theme.Text);
        Set(buffer, streaming ? FormatBuffer(m.BufferMs) : none, Theme.Text);
        if (streaming && m.HaveError) {
            double worst = Math.Max(Math.Abs(m.ErrMinMs), Math.Abs(m.ErrMaxMs));
            Set(error, L.T("平均 " + Ms(m.ErrMs) + "（" + Ms(m.ErrMinMs) + " ~ " + Ms(m.ErrMaxMs) + "），平滑后 " + Ms(m.SmoothedMs),
                    "Average " + Ms(m.ErrMs) + " (" + Ms(m.ErrMinMs) + " to " + Ms(m.ErrMaxMs) + "), smoothed " + Ms(m.SmoothedMs)),
                worst > 20 ? Theme.Orange : Theme.Text);
        } else {
            Set(error, streaming && !m.Synchronized ? L.T("不适用", "Not applicable") : none, Theme.Text);
        }
        if (streaming && m.Synchronized && m.Output > 0) {
            double ppm = (m.Removed - m.Added) * 1e6 / m.Output;
            Set(correction, ppm.ToString("+0;-0;0", CultureInfo.InvariantCulture) + L.T(" ppm（本秒 +" + m.Removed + " / -" + m.Added + " 次）", " ppm (this second +" + m.Removed + " / -" + m.Added + ")"), Theme.Text);
        } else if (streaming && !m.Synchronized) {
            Set(correction, m.CatchUp > 0 ? L.T("正在追赶积压：本秒删除 " + m.CatchUp + " 个采样", "Catching up: removed " + m.CatchUp + " samples this second") : L.T("无积压", "No backlog"), m.CatchUp > 0 ? Theme.Orange : Theme.Text);
        } else {
            Set(correction, none, Theme.Text);
        }
        Set(resyncs, m.TotalResyncs + L.T(" 次", "") + (m.TotalDropped > 0 ? L.T("，过时丢弃 " + m.TotalDropped + " 个采样", ", " + m.TotalDropped + " stale samples dropped") : ""),
            m.TotalResyncs > 0 ? Theme.Orange : Theme.Text);

        if (streaming && m.HasNetworkCounts) {
            Set(packets, L.T(m.Rx + " 个/秒，累计 " + m.TotalRx + " 个", m.Rx + "/s, " + m.TotalRx + " total"), Theme.Text);
            double total = PlaybackMonitor.LossPercent(m.TotalRx, m.TotalMiss);
            Set(loss, L.T("本秒 " + m.Miss + " 个，累计 " + m.TotalMiss + " 个（" + total.ToString("0.00", CultureInfo.InvariantCulture) + "%）",
                    m.Miss + " this second, " + m.TotalMiss + " total (" + total.ToString("0.00", CultureInfo.InvariantCulture) + "%)"),
                total >= 5 ? Theme.Red : total >= 0.5 || m.Miss > 0 ? Theme.Orange : Theme.Green);
            Set(resend, L.T("请求 " + m.TotalReq + " 个，已补回 " + m.TotalRec + " 个", m.TotalReq + " requested, " + m.TotalRec + " recovered"), Theme.Text);
            Set(late, m.TotalLate + L.T(" 个", "") + (m.TotalFull > 0 ? L.T("，输出队列满丢弃 " + m.TotalFull + " 个", ", " + m.TotalFull + " dropped (output queue full)") : ""), m.TotalFull > 0 ? Theme.Orange : Theme.Text);
            Set(lead, m.HaveLead ? m.LeadMin + " ~ " + m.LeadMax + " ms" : none, m.HaveLead && m.LeadMin < 50 ? Theme.Orange : Theme.Text);
        } else {
            string tcp = streaming ? L.T("TCP 传输，不会丢包", "Over TCP, no packet loss") : none;
            Set(packets, tcp, Theme.Text);
            Set(loss, tcp, streaming ? Theme.Green : Theme.Text);
            Set(resend, none, Theme.Text); Set(late, none, Theme.Text); Set(lead, none, Theme.Text);
        }
        if (streaming && m.Output > 0) {
            double silent = m.Silent * 100.0 / m.Output;
            Set(underrun, silent < 0.05 ? L.T("正常", "Normal") : L.T("本秒 " + silent.ToString("0.0", CultureInfo.InvariantCulture) + "% 为静音填充（无数据或等待播放时间）",
                    silent.ToString("0.0", CultureInfo.InvariantCulture) + "% silence this second (no data, or waiting for play time)"),
                silent < 0.05 ? Theme.Green : Theme.Orange);
        } else {
            Set(underrun, streaming ? L.T("已暂停", "Paused") : none, Theme.Text);
        }
    }

    private static string FormatBuffer(double ms)
    {
        if (ms >= 1000) return (ms / 1000).ToString("0.0", CultureInfo.InvariantCulture) + L.T(" 秒", " s");
        return Math.Round(ms) + " ms";
    }
}

// ---------------------------------------------------------------- log window

internal sealed class LogForm : Form
{
    private readonly TextBox box = new TextBox();
    private readonly Action<string> onAdded;
    private readonly Action onCleared;

    public LogForm(Icon icon)
    {
        Text = L.T("运行日志", "Log");
        Icon = icon;
        Font = Theme.Regular(10f);
        BackColor = Theme.Window;
        StartPosition = FormStartPosition.CenterScreen;
        Size = new Size(820, 520);
        MinimumSize = new Size(480, 300);

        box.Multiline = true; box.ReadOnly = true; box.WordWrap = false;
        box.ScrollBars = ScrollBars.Both;
        box.Dock = DockStyle.Fill;
        box.Font = new Font("Consolas", 10f);
        box.BackColor = Color.White;
        box.BorderStyle = BorderStyle.None;

        FlowLayoutPanel bar = new FlowLayoutPanel { Dock = DockStyle.Bottom, AutoSize = true, Padding = new Padding(10, 8, 10, 8), WrapContents = false };
        Button copy = Theme.MakeButton(L.T("复制全部", "Copy all"), false);
        copy.Click += delegate { if (box.TextLength > 0) Clipboard.SetText(box.Text); };
        Button clear = Theme.MakeButton(L.T("清空", "Clear"), false);
        clear.Click += delegate { LogStore.Clear(); };
        Button folder = Theme.MakeButton(L.T("打开日志文件夹", "Open log folder"), false);
        folder.Click += delegate {
            try { Directory.CreateDirectory(LogStore.LogDirectory); Process.Start("explorer.exe", "\"" + LogStore.LogDirectory + "\""); } catch (Exception) { }
        };
        foreach (Button b in new Button[] { copy, clear, folder }) { b.Margin = new Padding(0, 0, 8, 0); bar.Controls.Add(b); }

        Controls.Add(box);
        Controls.Add(bar);
        box.Lines = LogStore.Snapshot();

        onAdded = delegate(string line) {
            if (IsDisposed || !IsHandleCreated) return;
            try { BeginInvoke((MethodInvoker)delegate {
                if (box.TextLength > 400000) box.Clear();
                box.AppendText(line + Environment.NewLine);
            }); } catch (InvalidOperationException) { }
        };
        onCleared = delegate {
            if (IsDisposed || !IsHandleCreated) return;
            try { BeginInvoke((MethodInvoker)delegate { box.Clear(); }); } catch (InvalidOperationException) { }
        };
        LogStore.Added += onAdded;
        LogStore.Cleared += onCleared;
        AutoScaleDimensions = new SizeF(96F, 96F);
        AutoScaleMode = AutoScaleMode.Dpi;
    }

    protected override void OnShown(EventArgs e)
    {
        base.OnShown(e);
        box.SelectionStart = box.TextLength;
        box.ScrollToCaret();
    }

    protected override void OnFormClosed(FormClosedEventArgs e)
    {
        LogStore.Added -= onAdded;
        LogStore.Cleared -= onCleared;
        base.OnFormClosed(e);
    }
}
