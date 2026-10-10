# Run only in a Windows test VM with the signed driver already loaded.
[CmdletBinding()]
param([Parameter(Mandatory=$true)][string]$Launcher)
$ErrorActionPreference='Stop'
$Launcher=(Resolve-Path $Launcher).Path
$root=Join-Path $env:TEMP ('vfs-minifilter-smoke-'+[Guid]::NewGuid().ToString('N'))
$hostFiles=Join-Path $root 'host'
$work=Join-Path $root 'work'
$layer=Join-Path $root 'layer'
New-Item -ItemType Directory -Path $hostFiles,$work | Out-Null
[IO.File]::WriteAllText((Join-Path $hostFiles 'modify.txt'),'host')
[IO.File]::WriteAllText((Join-Path $hostFiles 'delete.txt'),'keep')
[IO.File]::WriteAllText((Join-Path $hostFiles 'rename.txt'),'rename')
[IO.File]::WriteAllText((Join-Path $hostFiles 'mapped.txt'),'abcd')
$payload=@'
param([string]$HostFiles)
$ErrorActionPreference='Stop'
if ([IO.File]::ReadAllText((Join-Path $HostFiles 'modify.txt')) -ne 'host') {throw 'read-through'}
[IO.File]::WriteAllText((Join-Path $HostFiles 'modify.txt'),'private')
[IO.File]::WriteAllText((Join-Path $HostFiles 'new.txt'),'new')
[IO.File]::Delete((Join-Path $HostFiles 'delete.txt'))
[IO.File]::Move((Join-Path $HostFiles 'rename.txt'),(Join-Path $HostFiles 'renamed.txt'))
if (Test-Path (Join-Path $HostFiles 'delete.txt')) {throw 'whiteout'}
$names=@(Get-ChildItem -LiteralPath $HostFiles -Name)
if ($names -notcontains 'new.txt' -or $names -contains 'delete.txt') {throw 'merged enumeration'}
& powershell.exe -NoProfile -NonInteractive -Command "[IO.File]::WriteAllText('$HostFiles\child.txt','child')"
if ($LASTEXITCODE -ne 0) {throw 'child process'}
$mapped=[IO.MemoryMappedFiles.MemoryMappedFile]::CreateFromFile((Join-Path $HostFiles 'mapped.txt'),[IO.FileMode]::Open)
$view=$mapped.CreateViewAccessor()
try {$view.Write(0,[byte]88);$view.Flush()} finally {$view.Dispose();$mapped.Dispose()}
if ([IO.File]::ReadAllText((Join-Path $HostFiles 'mapped.txt')) -ne 'Xbcd') {throw 'mapped write'}
if ([IO.File]::ReadAllText((Join-Path $HostFiles 'modify.txt')) -ne 'private') {throw 'read-your-writes'}
'@
$script=Join-Path $root 'payload.ps1'
[IO.File]::WriteAllText($script,$payload)
$previous=$env:VFS_SANDBOX_WINDOWS_BACKEND
try {
 $env:VFS_SANDBOX_WINDOWS_BACKEND='minifilter'
 Push-Location $work
 try {
  & $Launcher --layer-dir $layer -- powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $script $hostFiles
  if ($LASTEXITCODE -ne 0) {throw "Sandbox failed with $LASTEXITCODE; retained $root"}
 } finally {Pop-Location}
 if ([IO.File]::ReadAllText((Join-Path $hostFiles 'modify.txt')) -ne 'host') {throw 'HOST MODIFIED'}
 if ([IO.File]::ReadAllText((Join-Path $hostFiles 'mapped.txt')) -ne 'abcd') {throw 'HOST MAPPED WRITE'}
 if (-not (Test-Path (Join-Path $hostFiles 'delete.txt'))) {throw 'HOST DELETED'}
 if (-not (Test-Path (Join-Path $hostFiles 'rename.txt'))) {throw 'HOST RENAMED'}
 foreach($name in @('new.txt','renamed.txt','child.txt')) {if (Test-Path (Join-Path $hostFiles $name)) {throw "HOST CREATED $name"}}
 & $Launcher diff $layer
 if ($LASTEXITCODE -ne 0) {throw 'Diff failed'}
 Write-Host "Smoke checks passed. Results retained at $root"
} finally {$env:VFS_SANDBOX_WINDOWS_BACKEND=$previous}
