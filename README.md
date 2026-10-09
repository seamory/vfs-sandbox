# VFS Sandbox

A Go command launcher with filesystem isolation on Linux, macOS, and Windows. The available isolation model depends on the operating system: Linux uses a full path OverlayFS view, macOS applies a write policy around a private workspace clone, and Windows tries Windows Sandbox first, then Sandboxie-Plus. Minifilter dependencies are unavailable, so that backend is reserved but not implemented.

## Build and use

The launcher uses the Go standard library. Build it with the Go toolchain for the target operating system.

```sh
go build -o vfs-sandbox .
```

Linux:

```sh
sudo ./vfs-sandbox -- /bin/sh
sudo ./vfs-sandbox --keep-layer -- /usr/bin/make
sudo ./vfs-sandbox --save-artifacts --artifact-dir ./run-output -- /usr/bin/make
sudo ./vfs-sandbox diff /dev/shm/vfs-sandbox-XXXXXXXX
```

macOS:

```sh
./vfs-sandbox -- /bin/sh
./vfs-sandbox --keep-layer -- make
./vfs-sandbox --save-artifacts --artifact-dir ./run-output -- make
./vfs-sandbox diff /path/to/vfs-sandbox-layer
```

Windows:

```powershell
vfs-sandbox.exe -- cmd.exe /c ver
vfs-sandbox.exe --keep-layer -- cmd.exe /c echo sandboxed
vfs-sandbox.exe --save-artifacts --artifact-dir .\run-output -- cmd.exe /c echo sandboxed
vfs-sandbox.exe diff C:\path\to\vfs-sandbox-layer
```

Linux removes its writable layer after the command exits unless `--keep-layer` or `--layer-dir DIR` is used. Its layer must be on a writable tmpfs below `/dev/shm`, `/run`, `/tmp`, or `/var/tmp`, so it can be hidden from the sandbox root. A retained Linux layer is inspectable only while that tmpfs remains mounted.

Artifact saving is off by default. Add `--save-artifacts` to save newly created or changed regular files after the command exits, including when the command returns a nonzero status. Use `--artifact-dir DIR` with that switch to choose a new output directory whose parent already exists. Without `--artifact-dir`, the launcher creates `vfs-sandbox-artifacts-YYYYMMDD-HHMMSS-RANDOM` under the host's current working directory. On Linux, exported files keep their paths relative to the sandbox root. On macOS and Windows, workspace files are saved under `workspace/`; macOS private-directory files are saved under `sandbox/`.

On macOS and Windows, `--keep-layer` retains an automatically created directory under the system temporary directory. `--layer-dir DIR` creates a retained layer at a new directory whose parent already exists. On macOS, the layer contains the copied workspace, private home and temporary directories, and a manifest. On Windows, it contains the exported workspace, command output, exit status, and a manifest.

`diff LAYER_DIR` reports Linux OverlayFS upper-layer changes. On macOS and Windows it compares the exported workspace with the source workspace snapshot; macOS also lists changes under the private home and temporary directories. Windows discards the rest of the guest VM when it closes, so changes elsewhere in the guest filesystem are not exported by `diff` or saved as artifacts. Linux also supports `export LAYER_DIR [DEST_DIR]` to copy changed regular files from a retained layer; without `DEST_DIR`, it creates an artifact directory in the current working directory.

## Platform behavior

| Platform | Filesystem isolation | Command runs in | Change report |
| --- | --- | --- | --- |
| Linux | Full visible mount tree is reconstructed with OverlayFS, plus explicit policies for kernel and shared filesystems | Host Linux installation under a private mount namespace | Per-mount upper-layer changes and whiteouts |
| macOS | Seatbelt denies filesystem writes outside the private workspace, home, and temporary directories; APFS clone-on-write is used for workspace files when supported | Host macOS installation | Workspace changes and writes under the private home and temporary directories |
| Windows | Windows Sandbox first, Sandboxie-Plus fallback; Minifilter reserved, not implemented | A disposable Windows Sandbox VM or a temporary Sandboxie-Plus box | Exported workspace changes, command output, and exit status |

### Linux mount policies

The launcher reads `/proc/self/mountinfo`, keeps the visible mount at each mountpoint, and applies a policy to every discovered mount. It composes the root first and then reconstructs nested mounts.

| Mount or filesystem | Sandbox policy |
| --- | --- |
| `/` and ordinary mounted filesystems | OverlayFS with an independent writable upper/work pair |
| `/proc` | A fresh procfs mount |
| `/sys` | A fresh read-only sysfs mount |
| `/dev` | A private tmpfs with null, zero, full, random, urandom, tty, and a private devpts instance |
| `/run`, `/tmp`, `/var/tmp`, `/dev/shm`, `/dev/mqueue` | Private tmpfs mounts; these volatile writes are separate from the OverlayFS layer |
| cgroup and cgroup2 | Recursive bind mount made read-only |
| NFS, CIFS/SMB, 9p, virtiofs, Ceph, AFS, Lustre, and FUSE mounts | Recursive bind mount made read-only |
| Other kernel interface filesystems, such as debugfs, tracefs, securityfs, and efivarfs | Hidden behind an empty private tmpfs |
| Standalone regular-file mounts | Copied into the nearest ordinary mount's upper layer so their original contents remain visible and writes stay private |
| Standalone socket and device mounts outside `/dev` | Hidden with an OverlayFS whiteout |

Mount aliases that are symlinks, such as a common `/var/run -> /run`, are not mounted a second time. Layer storage must be on tmpfs so its backing directories cannot overlap an OverlayFS lower tree.

### macOS behavior

macOS does not provide the Linux mount namespace and OverlayFS combination used for its full path view. This backend uses the macOS Seatbelt (`sandbox-exec`) write policy and clones the current working directory into the layer with APFS copy-on-write where the source and layer are on the same supported filesystem. It falls back to a regular copy when cloning is unavailable. Writes to the private workspace and `HOME`, `TMPDIR`, and XDG directories are retained. Other filesystem writes are denied. Reads and network access remain available, consistent with the Linux launcher's shared host network behavior.

The macOS backend does not redirect writes to arbitrary host paths into a full-system copy-on-write tree. Programs that need to modify paths outside the copied workspace and private directories will receive a write error. macOS currently marks `sandbox-exec` deprecated; availability and profile behavior depend on the installed macOS version.

### Windows behavior

Windows tries a disposable Windows Sandbox VM first. It disables guest networking, clipboard redirection, and virtual GPU access. The host working directory is mapped read-only, copied into a writable guest workspace, and exported to the retained layer after the command exits. An absolute command path inside the host working directory is redirected to its guest copy. Standard input is closed in the guest, so commands must be noninteractive. The VM shuts down after the command completes.

If Windows Sandbox is unavailable or its launcher cannot be started, the command falls back to a uniquely named, temporary Sandboxie-Plus box. The fallback uses the host installation, runs the command under Sandboxie, and copies the sandboxed view of the working directory and command output into the layer before clearing the box. It requires `Start.exe`, `SbieIni.exe`, and a working Sandboxie-Plus driver/service. It is not a VM: it follows the installed Sandboxie configuration, and network access or configured direct-write exceptions may remain enabled. The launcher does not retry through Sandboxie after Windows Sandbox has started, to avoid running a command twice. If neither backend is available, it returns an error. Minifilter dependencies are unavailable, so its backend is reserved but not implemented.

Windows Sandbox is an optional Windows feature and requires a supported Windows edition and Hyper-V. In that backend, the command runs in the clean guest OS, so its executable and dependencies must be present in that guest or in the copied working directory. Guest filesystem changes outside that workspace are discarded when the VM closes and are not included in `diff`.

## Scope and limitations

On Linux, mount namespace setup requires `CAP_SYS_ADMIN`, creating the private `/dev` requires `CAP_MKNOD`, and OverlayFS must be available. The selected tmpfs must have enough space for changes and copied-up files.

The Linux backend is filesystem copy-on-write, not a complete security sandbox. It does not create user, PID, network, or IPC namespaces, drop capabilities, or close inherited file descriptors. A process with mount capabilities, an inherited host file descriptor, or another kernel interface can access host resources outside this filesystem view. The fresh `/proc` mount still shows the host PID namespace.

The macOS and Windows backends use different operating-system isolation mechanisms and do not provide identical behavior to Linux. macOS permits reads across the host filesystem and shares the host network. Windows may use a VM or the weaker Sandboxie-Plus process sandbox; only the working directory is exported for review. Do not use this launcher as the sole security boundary for untrusted programs.
