[CmdletBinding()]
param(
  [Parameter(Mandatory=$true)]
  [ValidatePattern('^[0-9]+(\.[0-9]+)?$')]
  [string]$Altitude,
  [ValidateSet('Debug','Release')]
  [string]$Configuration='Release',
  [ValidateSet('x64','x86')]
  [string]$Architecture='x64'
)
$ErrorActionPreference='Stop'
$project=Join-Path $PSScriptRoot 'VfsSandbox.vcxproj'
$output=Join-Path $PSScriptRoot "out\$Architecture\$Configuration"
$platform='x64'
$infArchitecture='NTamd64'
$kitArguments=@()
if ($Architecture -eq 'x86') {
  $platform='Win32'
  $infArchitecture='NTx86'
  # WDK 24H2 and newer no longer include x86 kernel libraries/tools.
  $kitArguments=@('/p:WindowsTargetPlatformVersion=10.0.22621.0')
}
New-Item -ItemType Directory -Force $output | Out-Null
# Run in a VS 2022 Developer PowerShell with matching SDK and WDK installed.
& msbuild.exe $project /m "/p:Configuration=$Configuration" "/p:Platform=$platform" "/p:OutDir=$output\" @kitArguments
if ($LASTEXITCODE -ne 0) {throw 'WDK build failed'}
$inf=[IO.File]::ReadAllText((Join-Path $PSScriptRoot 'VfsSandbox.inf.in')).Replace('@ALTITUDE@',$Altitude).Replace('@ARCH@',$infArchitecture)
[IO.File]::WriteAllText((Join-Path $output 'VfsSandbox.inf'),$inf,[Text.Encoding]::ASCII)
Write-Host "Built in $output. Validate the INF, generate the catalog, and sign the driver package before installation."
Write-Host 'Use a Microsoft-assigned virtualization altitude for distribution; a lab altitude is only for an isolated test VM.'
