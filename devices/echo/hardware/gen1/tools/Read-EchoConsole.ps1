param(
    [string]$PortName = 'COM6',
    [ValidateRange(1, 43200)][int]$Seconds = 3600,
    [string]$RuntimeDirectory = (Join-Path $PSScriptRoot '..\..\..\local\runtime')
)
$ErrorActionPreference = 'Stop'
$null = New-Item -ItemType Directory -Path $RuntimeDirectory -Force
$taskDir = (Resolve-Path -LiteralPath $RuntimeDirectory).Path
$logPath = Join-Path $taskDir ('boot-' + (Get-Date -Format 'yyyy-MM-dd-HHmmss-fff') + '.log')
$controlPath = Join-Path $taskDir 'echo-console-control.json'
$statusPath = Join-Path $taskDir 'echo-console-status.json'
$serial = New-Object IO.Ports.SerialPort($PortName, 115200, [IO.Ports.Parity]::None, 8, [IO.Ports.StopBits]::One)
$serial.Handshake = [IO.Ports.Handshake]::None
$serial.DtrEnable = $false
$serial.RtsEnable = $false
$serial.Encoding = [Text.Encoding]::ASCII
$lastCommand = $null
$writer = $null
$opened = $false
try {
    if (Test-Path -LiteralPath $controlPath) {
        $initialControl = Get-Content -LiteralPath $controlPath -Raw | ConvertFrom-Json
        $lastCommand = $initialControl.id
    }
    # Open the port before publishing state; a failed second reader must not
    # overwrite the status of an already active reader.
    $serial.Open()
    $opened = $true
    $writer = New-Object IO.StreamWriter($logPath, $false, (New-Object Text.UTF8Encoding($false)))
    $writer.AutoFlush = $true
    @{phase='listening'; port=$PortName; baud=115200; log=$logPath; pid=$PID; opened=(Get-Date).ToString('o')} | ConvertTo-Json | Set-Content -LiteralPath $statusPath -Encoding UTF8
    Write-Output ('CONSOLE READY: ' + $PortName + ', 115200 baud, 8N1, no flow control. Log: ' + $logPath)
    $deadline = (Get-Date).AddSeconds($Seconds)
    while ((Get-Date) -lt $deadline) {
        $chunk = $serial.ReadExisting()
        if ($chunk.Length -gt 0) {
            $writer.Write($chunk)
            [Console]::Write($chunk)
        }
        if (Test-Path -LiteralPath $controlPath) {
            try { $control = Get-Content -LiteralPath $controlPath -Raw | ConvertFrom-Json } catch { $control = $null }
            if ($control -and $control.id -ne $lastCommand) {
                $lastCommand = $control.id
                if ($control.action -eq 'stop') { break }
                if ($control.action -eq 'send' -and $control.text -is [string] -and $control.text.Length -le 4096) {
                    $serial.Write($control.text)
                    Write-Output ('[Sent console input, command ' + $control.id + ']')
                }
            }
        }
        Start-Sleep -Milliseconds 100
    }
} finally {
    if ($serial.IsOpen) { $serial.Close() }
    $serial.Dispose()
    if ($writer) { $writer.Dispose() }
    if ($opened) {
        @{phase='closed'; port=$PortName; log=$logPath; pid=$PID; closed=(Get-Date).ToString('o')} | ConvertTo-Json | Set-Content -LiteralPath $statusPath -Encoding UTF8
    }
}
