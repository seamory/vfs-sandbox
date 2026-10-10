# VfsSandbox Minifilter backend

This is an experimental Windows filesystem CoW backend. The repository contains
its kernel driver, the Go broker, a WDK project, an INF template, and a Windows
smoke script. The Go code and portable overlay tests can be built on other
platforms. The kernel driver must still be built, signed, and exercised with WDK
and Driver Verifier in a Windows test VM before deployment. No signed `.sys` is
bundled, and the launcher does not install or load drivers automatically.

## Scope

The view covers ordinary files across **all fixed, drive-letter NTFS volumes
present when the session starts**, including absolute paths outside the working
directory. The broker verifies that the loaded filter is attached to each such
volume before starting the command. Driver disconnection, an unsupported request,
a copy failure, or a protocol error fails the operation rather than allowing a
host write. The driver refuses explicit detach/unload while a session is active.

This is filesystem virtualization for cooperative programs, not a complete
security sandbox. Registry access, networking, devices outside the filesystem
filter stack, other processes, externally duplicated handles, and writes
performed by an untracked service are not isolated. The child currently retains
the launcher's token; an administrator program is not confined against malicious
attempts to bypass the filter. Do not treat the backend as a security boundary.

Current compatibility limits:

- Windows x64 (Go `amd64`): Windows 10 2004 or later / Windows 11.
  Windows x86 (Go `386`): 32-bit Windows 10 2004 or later. One active session
  and at most 4096 tracked processes per driver instance. ARM64 is not supported.
  Driver architecture must match the Windows kernel, not the target application:
  32-bit applications on Windows x64 use the x64 driver. The fixed-width port
  protocol also permits a 32-bit launcher under WOW64; this combination still
  requires Windows runtime validation.
- UNC paths, removable/FAT/ReFS volumes, volumes without drive letters, alternate
  data streams, reparse points, and case-sensitive directories are not supported.
  Requests reaching the filter for these paths fail. Directory listings omit
  inaccessible reparse/case-sensitive entries.
- Open-by-ID, hard-link creation, extended rename classes, directory change
  notifications, transactional filesystem operations, and arbitrary filesystem
  control operations are not supported. Ordinary same-volume rename is brokered;
  sharing violations can make a rename fail. Cross-volume rename is rejected.
- Paths are limited to 2047 UTF-16 code units by the protocol. Native filename
  queries can expose physical layer paths. Existing hard-link/short-name aliases
  do not share an overlay identity. ACLs, extended attributes and all NTFS metadata
  are not cloned. Sparse directory placeholders can expose the sparse attribute.
- Lower files are read live until copied up; there is no atomic host snapshot.
  A directory's lower membership is captured on first enumeration. The broker
  serializes its requests, but target processes still have normal filesystem
  concurrency and sharing behavior.

## How the view works

1. The launcher connects to the administrator-only `\VfsSandboxPort` and creates
   the command suspended in a kill-on-close Job Object. It registers the process
   and private root with the driver before resuming its first user thread. The
   process callback tracks descendants by the actual creating process.
2. For an intercepted open, the driver asks the Go broker to resolve the logical
   NT device path. Read-only file opens go to the host until copy-up. A writable,
   delete-capable or create open first copies existing content into
   `minifilter-upper/<drive>/...`. Creates are reparsed to that private path.
3. Directory handles refer to private directories. Before enumeration, the
   broker creates sparse placeholders with lower file lengths and timestamps;
   placeholders are always redirected to lower content on read or hydrated on
   write. It does not copy file contents just to list a directory. Lower reads
   through already-open handles retain the usual pre-copy-up view.
4. Removing a known private entry leaves a whiteout in the broker's session map.
   Recreated directories are opaque to lower entries. Directory rename hydrates
   the moved subtree, performs the rename in the layer, and moves its whiteouts.
5. Handle contexts distinguish lower read handles from private handles. Write,
   metadata, delete and writable mapping operations are allowed only on private
   handles. Unsupported mutation classes are rejected. The broker runs outside
   the tracked tree so its own copy operations cannot recursively call itself.
6. When the root command exits, the launcher terminates remaining descendants,
   waits for the job to become empty, and stops the broker before collecting
   results. A lost broker connection does not unregister live processes from
   the driver's deny path. The job also terminates descendants if the launcher
   exits unexpectedly.

`diff LAYER_DIR` uses `minifilter-changes.json` to report observed changes across
all supported volumes. `--save-artifacts` writes changed regular files under
`drives/<drive>/...`; deletion records remain in the JSON report. The retained
`workspace/` is a fully materialized copy for inspection. The raw
`minifilter-upper/` also contains unread sparse placeholders, so it must not be
used as a standalone restored filesystem. A layer cannot be resumed as a new
session. Reports are produced only after normal process-tree cleanup; an
interrupted session may have no complete report.

## Build and install in a Windows test VM

For a reusable Windows container build environment, see the
[Docker build guide](docker/README.md). It supports x64/x86 packages and the root
Makefile targets `minifilter-image` and `minifilter-docker`. Native WDK builds can
also be invoked with `make minifilter ALTITUDE=... ARCHITECTURE=x86` from GNU Make
with a POSIX shell inside an initialized Windows WDK environment.

Install Visual Studio 2022 C++ tools and matching Windows SDK/WDK. Build x86
drivers with the serviced **22621 SDK/WDK** that Microsoft lists for Windows 10
x86 development (currently WDK 22621.5193; SDK target `10.0.22621.0`). WDK 24H2
and later removed x86 kernel-driver build support. Building the x86 driver on
a Windows x64 development machine is supported by this project's build setup;
use a 32-bit Windows 10 VM to exercise the native x86 driver.

In Developer PowerShell, choose one matching launcher/driver pair:

```powershell
# x64 Windows
$env:GOOS = 'windows'
$env:GOARCH = 'amd64'
go build -o vfs-sandbox-amd64.exe .
.\drivers\minifilter\build.ps1 -Architecture x64 -Altitude <your-virtualization-filter-altitude>

# x86 Windows (requires the 22621 SDK/WDK)
$env:GOARCH = '386'
go build -o vfs-sandbox-386.exe .
.\drivers\minifilter\build.ps1 -Architecture x86 -Altitude <your-virtualization-filter-altitude>
Remove-Item Env:GOOS, Env:GOARCH
```

`-Architecture` defaults to `x64`. Packages are separated under
`drivers/minifilter/out/x64/Release/` and `out/x86/Release/` (or `Debug`). Each
package contains its own `.sys` and an INF restricted to `NTamd64` or `NTx86`;
generate and sign each catalog separately. Both builds use the same driver
source and CoW protocol. The launcher supplies the architecture-specific Job
Object structure size (144 bytes on x64, 112 on x86); Filter Manager message
headers remain 16 bytes in both builds.

The altitude parameter is required deliberately. Obtain a Microsoft allocation
for distribution; any locally selected development value is for an isolated VM
and must be checked for conflicts. The script writes the INF with that value;
it does not invent a production altitude or change boot/signing policy.

Validate the generated INF with the WDK tools, generate its catalog with Inf2Cat
for the target Windows version, and sign the package using your configured test
or production signing workflow. Install the signed INF in the test VM, then run
from an elevated terminal:

```powershell
fltmc load VfsSandbox
fltmc instances -f VfsSandbox
```

Use Driver Verifier for this driver in the VM before real workloads. A successful
Go cross-build is not evidence that the kernel driver builds or passes Verifier.

The automatic order remains Windows Sandbox → Sandboxie-Plus → Minifilter.
For direct testing, select this backend explicitly:

```powershell
$env:VFS_SANDBOX_WINDOWS_BACKEND = 'minifilter'
.\vfs-sandbox-amd64.exe --keep-layer -- cmd.exe /c ver
.\drivers\minifilter\smoke.ps1 -Launcher .\vfs-sandbox-amd64.exe
Remove-Item Env:VFS_SANDBOX_WINDOWS_BACKEND
```

The smoke script checks absolute paths outside the working directory, creation,
modification, deletion, ordinary rename, merged directory enumeration, a child
process, and a writable memory mapping, then checks that host contents did not
change. It retains its layer and fixtures for inspection. It must be run on
Windows; it has not been run by the macOS development environment.
On 32-bit Windows, load the x86 driver and pass `vfs-sandbox-386.exe` instead.
On Windows x64, also exercise 32-bit target applications and the 386 launcher
against the x64 driver to cover WOW64 separately. Neither driver's WDK build
or these runtime combinations have been validated in this environment.

Before broader use, also exercise driver disconnect while a command is active,
launcher termination, copy-up failures, concurrent opens/deletes/renames,
different source/layer volumes, mapped files surviving handle closure, and
repeated attach/session/unload cycles under Driver Verifier. These kernel cases
cannot be established by the portable Go unit tests.

## References

- [Microsoft SimRep](https://learn.microsoft.com/en-us/samples/microsoft/windows-driver-samples/simrep-file-system-minifilter-driver/) describes pre-create reparsing; it does not by itself implement an overlay.
- [User-mode communication](https://learn.microsoft.com/en-us/windows-hardware/drivers/ifs/communication-between-user-mode-and-kernel-mode) documents the Filter Manager port used by the broker.
- [Minifilter INF requirements](https://learn.microsoft.com/en-us/windows-hardware/drivers/ifs/creating-an-inf-file-for-a-minifilter-driver) describe installation and signing requirements.
- [Filter altitude allocation](https://learn.microsoft.com/en-us/windows-hardware/drivers/ifs/minifilter-altitude-request) describes production altitude requests.
- [Supported WDK versions](https://learn.microsoft.com/en-us/windows-hardware/drivers/other-wdk-downloads) lists the serviced 22621 kit for Windows 10 x86 development.
- [WDK known issues](https://learn.microsoft.com/en-us/windows-hardware/drivers/wdk-known-issues) documents removal of x86 kernel-driver development from WDK 24H2 onward.
