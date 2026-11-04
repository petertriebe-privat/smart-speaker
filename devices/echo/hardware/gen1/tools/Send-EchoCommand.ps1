[CmdletBinding(DefaultParameterSetName='Command')]
param(
    [Parameter(Mandatory=$true, ParameterSetName='Command')][string]$Command,
    [Parameter(Mandatory=$true, ParameterSetName='Interrupt')][switch]$InterruptBoot,
    [Parameter(ParameterSetName='Interrupt')][ValidateRange(1,30)][int]$Seconds = 15,
    [Parameter(Mandatory=$true, ParameterSetName='Stop')][switch]$StopReader,
    [string]$RuntimeDirectory = (Join-Path $PSScriptRoot '..\..\..\local\runtime')
)
$ErrorActionPreference = 'Stop'
$taskDir = (Resolve-Path -LiteralPath $RuntimeDirectory).Path
$status = Get-Content -LiteralPath (Join-Path $taskDir 'echo-console-status.json') -Raw | ConvertFrom-Json
if ($status.phase -ne 'listening') { throw 'Serial console is not open. Inspect status and start the reader first.' }
if ($PSCmdlet.ParameterSetName -eq 'Command') {
    if ($Command.Length -gt 512 -or $Command.Contains([string][char]10) -or $Command.Contains([string][char]13)) {
        throw 'Send exactly one bounded console command without embedded line breaks.'
    }
}
function Write-Control([string]$Action, [string]$Text) {
    $payload = @{id=[guid]::NewGuid().ToString(); action=$Action; text=$Text} | ConvertTo-Json
    $bytes = [Text.Encoding]::UTF8.GetBytes($payload)
    $stream = [IO.File]::Open((Join-Path $taskDir 'echo-console-control.json'), [IO.FileMode]::Create, [IO.FileAccess]::Write, [IO.FileShare]::ReadWrite)
    try { $stream.Write($bytes, 0, $bytes.Length); $stream.Flush() } finally { $stream.Dispose() }
}
switch ($PSCmdlet.ParameterSetName) {
    'Command' {
        Write-Control 'send' ($Command + [char]13)
        Write-Output ('Queued, not acknowledged: ' + $Command)
    }
    'Interrupt' {
        $deadline = (Get-Date).AddSeconds($Seconds)
        while ((Get-Date) -lt $deadline) {
            Write-Control 'send' ([string][char]13)
            Start-Sleep -Milliseconds 250
        }
        Write-Output 'Queued repeated CR. Inspect the log for the U-Boot prompt.'
    }
    'Stop' {
        Write-Control 'stop' ''
        Write-Output 'Queued reader stop. This does not stop or power off the Echo.'
    }
}
