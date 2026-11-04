param([string]$Command)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$project = (Resolve-Path (Join-Path $PSScriptRoot '..\..\..')).Path
$config = Join-Path $project 'local\dot-ssh\config'
if (!(Test-Path -LiteralPath $config)) {
    throw 'Private Dot SSH profile is missing. Read hardware/dot2/docs/21-echo-dot-linux-ssh.md.'
}
if ($Command) {
    & ssh -F $config dot-dev $Command
} else {
    & ssh -F $config dot-dev
}
exit $LASTEXITCODE
