# Minifilter builds in Docker

This directory builds **unsigned x64 and x86 Minifilter packages** with EWDK in a
native Windows container. It reuses `../build.ps1` and the existing driver project.
The container only compiles; driver installation, signing policy and runtime
validation belong on a separate Windows test machine/VM.

## Requirements

- An **x64 Windows Docker engine**, with Windows containers enabled. Local Docker
  Desktop on macOS/Linux cannot run this image. A remote Windows Docker context
  can build it, but Compose bind mounts refer to files on the engine's machine:
  copy this repository there and run the commands there for the supplied workflow.
- The default base is `windows/servercore:ltsc2022`. Use a compatible Windows host
  for process isolation, or set `WINDOWS_ISOLATION=hyperv` on a host that supports
  Hyper-V isolation. Image building also requires a compatible host/isolation
  configuration; for example pass `--isolation=hyperv` to `docker build` as needed.
- GNU Make plus a POSIX shell for Makefile commands (for example Git Bash on
  Windows). PowerShell alternatives are shown below.
- Complete, extracted **EWDK ISO contents** in `drivers/minifilter/docker/ewdk/`,
  including `BuildEnv/SetupBuildEnv.cmd`, `Program Files`, and `Program Files (x86)`.
  This directory is ignored by Git. Obtain the kit from Microsoft's download
  pages and use it under its license. No third-party toolchain image is required.
- For `x86` or `all`, use a VS 2022 / **22621 EWDK** that contains the
  `10.0.22621.0` SDK and x86 kernel libraries. Do not substitute the latest EWDK:
  WDK 24H2 and newer removed x86 kernel-driver support. For an x64-only image,
  a matching VS 2022 EWDK can be used instead. Keep differently provisioned images
  under different tags. Allow sufficient disk space for the extracted kit and
  Windows image layers; the kit is large.

Expected local layout:

```text
drivers/minifilter/docker/
  Dockerfile
  entrypoint.cmd
  build-container.ps1
  compose.yaml
  ewdk/
    BuildEnv/SetupBuildEnv.cmd
    ...complete EWDK media...
```

## Using Make

From the repository root, on the Windows build host:

```sh
make minifilter-image
# Replace the altitude with your assigned/isolated-lab value.
make minifilter-docker ALTITUDE="$FILTER_ALTITUDE"
make minifilter-docker ALTITUDE="$FILTER_ALTITUDE" ARCHITECTURE=x86 CONFIGURATION=Debug
```

`ARCHITECTURE` defaults to `all`; accepted values are `x64`, `x86`, and `all`.
`CONFIGURATION` defaults to `Release`. `ALTITUDE` is required and must be numeric,
with an optional fractional part. The build does not choose a production altitude.
`MINIFILTER_IMAGE` overrides the image tag in both image and driver builds.
Set `WINDOWS_ISOLATION=hyperv` on both Make commands when the host requires
Hyper-V isolation. `WINDOWS_BASE` overrides the base image for `minifilter-image`.
Build the image again when updating the EWDK or container scripts; driver source
changes only require rerunning `minifilter-docker`.

## Using PowerShell without Make

From the repository root:

```powershell
docker build --memory 4g -t vfs-sandbox-minifilter:ewdk drivers/minifilter/docker
if ($LASTEXITCODE -ne 0) { throw 'Image build failed' }
New-Item -ItemType Directory -Force dist/minifilter | Out-Null
$env:ALTITUDE = Read-Host 'Filter altitude'
$env:ARCHITECTURE = 'all'
$env:CONFIGURATION = 'Release'
docker compose -f drivers/minifilter/docker/compose.yaml run --rm --no-deps builder
if ($LASTEXITCODE -ne 0) { throw 'Driver build failed' }
```

For a Hyper-V-compatible host that needs Hyper-V isolation, add
`--isolation=hyperv` to the image build and set `$env:WINDOWS_ISOLATION='hyperv'`
before the Compose command.

## Results and boundaries

The source is mounted read-only at `C:\src`. Intermediate files are built under
`C:\work` in the disposable container. Successful packages appear under:

```text
dist/minifilter/x64/Release/build-<unique-id>/VfsSandbox.sys
                                             VfsSandbox.inf
                                             VfsSandbox.pdb  (when produced)
dist/minifilter/x86/Release/build-<unique-id>/...
```

Each invocation creates a fresh package directory; a failed build does not
publish new packages. Existing results remain available. INF architecture
sections are generated separately for each build. Catalog generation and signing
are intentionally separate steps, as documented in [the driver guide](../README.md).
The Go launcher is built by `make build`, `make windows-amd64`, `make windows-386`
or `make cross`, outside this driver image.

These Docker files have not been executed on the macOS development host. A Go
cross-build cannot establish that the driver compiles with WDK or works at
runtime. After building and signing, use the matching Windows test VM and the
existing `../smoke.ps1`; container compilation does not load the driver.

## Sources

- [Microsoft: Enterprise WDK](https://learn.microsoft.com/en-us/windows-hardware/drivers/develop/using-the-enterprise-wdk)
- [Microsoft: supported WDK versions](https://learn.microsoft.com/en-us/windows-hardware/drivers/other-wdk-downloads)
- [Microsoft: WDK known issues](https://learn.microsoft.com/en-us/windows-hardware/drivers/wdk-known-issues)
- [Microsoft: Windows container version compatibility](https://learn.microsoft.com/en-us/virtualization/windowscontainers/deploy-containers/version-compatibility)
