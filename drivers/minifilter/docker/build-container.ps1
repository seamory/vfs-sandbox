[CmdletBinding()]
param(
  [Parameter(Mandatory=$true)]
  [ValidatePattern('^[0-9]+(\.[0-9]+)?$')]
  [string]$Altitude,
  [ValidateSet('x64','x86','all')]
  [string]$Architecture='all',
  [ValidateSet('Debug','Release')]
  [string]$Configuration='Release'
)
$ErrorActionPreference='Stop'
# Keep the source mount read-only; all intermediate files live in the container.
$source='C:\src'
$work='C:\work'
$output='C:\out'
New-Item -ItemType Directory -Force $work,$output | Out-Null
$files=@('build.ps1','VfsSandbox.vcxproj','VfsSandbox.inf.in','filter.c','protocol.h')
foreach ($file in $files) {
  Copy-Item -LiteralPath (Join-Path $source $file) -Destination $work
}
$architectures=@($Architecture)
if ($Architecture -eq 'all') {$architectures=@('x64','x86')}
foreach ($arch in $architectures) {
  & (Join-Path $work 'build.ps1') -Altitude $Altitude -Architecture $arch -Configuration $Configuration
  $built=Join-Path $work "out\$arch\$Configuration"
  foreach ($required in @('VfsSandbox.sys','VfsSandbox.inf')) {
    if (-not (Test-Path -LiteralPath (Join-Path $built $required) -PathType Leaf)) {
      throw "Build did not produce $required for $arch"
    }
  }
}
# Export only after every requested architecture built successfully.
foreach ($arch in $architectures) {
  $built=Join-Path $work "out\$arch\$Configuration"
  $destination=Join-Path $output "$arch\$Configuration"
  # A unique run directory prevents older signed catalogs or binaries from being
  # mistaken for part of this unsigned package.
  $package=Join-Path $destination ('build-'+[Guid]::NewGuid().ToString('N'))
  New-Item -ItemType Directory -Force $package | Out-Null
  foreach ($name in @('VfsSandbox.sys','VfsSandbox.inf','VfsSandbox.pdb')) {
    $file=Join-Path $built $name
    if (Test-Path -LiteralPath $file -PathType Leaf) {Copy-Item -LiteralPath $file -Destination $package}
  }
  Write-Host "Exported unsigned $arch driver package to $package"
}
