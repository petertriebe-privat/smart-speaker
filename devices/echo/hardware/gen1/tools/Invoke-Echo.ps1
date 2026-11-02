[CmdletBinding()]
param([Parameter(Mandatory=$true)][string]$Command, [string]$EchoHost = 'echo-dev')
$ErrorActionPreference = 'Stop'
$config = Join-Path $PSScriptRoot '..\..\..\local\ssh\config'
if (!(Test-Path -LiteralPath $config)) { throw 'Private SSH configuration is missing; read hardware/gen1/docs/network.md.' }
$ssh = Join-Path $env:WINDIR 'System32\OpenSSH\ssh.exe'
& $ssh -F $config $EchoHost $Command
exit $LASTEXITCODE
