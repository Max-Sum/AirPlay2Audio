param(
    [string] $GoExe = "",
    [ValidateSet('', 'amd64', 'arm64')]
    [string] $Arch = "",
    [switch] $Test,
    # Skip slow real-time tests (go test -short), as CI does.
    [switch] $Short
)

$ErrorActionPreference = 'Stop'
Push-Location $PSScriptRoot
try {
    if (-not $GoExe) {
        $command = Get-Command go.exe -ErrorAction SilentlyContinue
        if ($command) { $GoExe = $command.Source }
        else { $GoExe = Join-Path $env:LOCALAPPDATA 'AirPlay2Audio\build-tools\go\bin\go.exe' }
    }
    if (-not (Test-Path -LiteralPath $GoExe)) { throw 'Go 1.26 or newer is required. Install Go or pass -GoExe with its full path.' }
    $hostArch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
    if (-not $Arch) { $Arch = $hostArch }
    $compiler = @('Framework64', 'FrameworkArm64', 'Framework') |
        ForEach-Object { Join-Path $env:WINDIR "Microsoft.NET\$_\v4.0.30319\csc.exe" } |
        Where-Object { Test-Path -LiteralPath $_ } | Select-Object -First 1
    if (-not $compiler) { throw 'The .NET Framework C# compiler was not found.' }
    $env:CGO_ENABLED = '0'
    $env:GOOS = 'windows'
    $env:GOARCH = $Arch
    if ($Test -and $Arch -ne $hostArch) { Write-Warning "Skipping tests: cannot run $Arch binaries on this $hostArch host." }
    elseif ($Test) {
        $testArgs = @('test')
        if ($Short) { $testArgs += '-short' }
        & $GoExe @testArgs ./...
        if ($LASTEXITCODE -ne 0) { throw 'Go tests failed.' }
    }
    & $GoExe build -o AirPlay2Engine.exe .
    if ($LASTEXITCODE -ne 0) { throw 'Receiver build failed.' }
    # WinRT metadata for the system media controls (media keys) ships with Windows.
    $winmd = Join-Path $env:WINDIR 'System32\WinMetadata'
    $winrt = @('Windows.Foundation.winmd', 'Windows.Media.winmd', 'Windows.Storage.winmd') | ForEach-Object { '/reference:' + (Join-Path $winmd $_) }
    & $compiler /nologo /target:winexe /codepage:65001 /out:AirPlay2Audio.exe /win32icon:ui\app.ico /reference:System.Windows.Forms.dll /reference:System.Drawing.dll /reference:System.Security.dll /reference:System.Runtime.dll /reference:System.Runtime.WindowsRuntime.dll /reference:System.Runtime.InteropServices.WindowsRuntime.dll @winrt ui\ReceiverApp.cs
    if ($LASTEXITCODE -ne 0) { throw 'Window build failed.' }
    Write-Host "Built AirPlay2Audio.exe and AirPlay2Engine.exe ($Arch)."
} finally {
    Pop-Location
}
