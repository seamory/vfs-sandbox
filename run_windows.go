//go:build windows

package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	windowsSandboxInput         = `C:\vfs-sandbox-input`
	windowsSandboxSource        = `C:\vfs-sandbox-source`
	windowsSandboxWork          = `C:\vfs-sandbox-work`
	windowsSandboxOutput        = `C:\vfs-sandbox-output`
	maxWindowsSandboxResultSize = 64 * 1024
)

type windowsSandboxRequest struct {
	Command string   `json:"command"`
	Args    []string `json:"arguments"`
}

type windowsSandboxResult struct {
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
}

type windowsSandboxConfig struct {
	XMLName              xml.Name                 `xml:"Configuration"`
	Networking           string                   `xml:"Networking"`
	ClipboardRedirection string                   `xml:"ClipboardRedirection"`
	VGPU                 string                   `xml:"VGpu"`
	MappedFolders        windowsMappedFolders     `xml:"MappedFolders"`
	LogonCommand         windowsSandboxLogonEntry `xml:"LogonCommand"`
}

type windowsMappedFolders struct {
	Folders []windowsMappedFolder `xml:"MappedFolder"`
}

type windowsMappedFolder struct {
	HostFolder    string `xml:"HostFolder"`
	SandboxFolder string `xml:"SandboxFolder"`
	ReadOnly      bool   `xml:"ReadOnly"`
}

type windowsSandboxLogonEntry struct {
	Command string `xml:"Command"`
}

func run(args []string) int {
	if len(args) > 0 && args[0] == "diff" {
		return runPortableDiff(args[1:])
	}
	invocation, code, done := parsePortableInvocation(args, printUsage)
	if done {
		return code
	}
	launcher, err := findWindowsSandbox()
	if err != nil {
		fmt.Fprintln(os.Stderr, "vfs-sandbox: Windows Sandbox is unavailable; enable the Windows Sandbox feature on a supported Windows edition with Hyper-V")
		return 125
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: get working directory: %v\n", err)
		return 125
	}
	workingDirectory, err = filepath.EvalSymlinks(workingDirectory)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: resolve working directory: %v\n", err)
		return 125
	}
	layerDir, removeLayer, err := preparePortableLayer(invocation.LayerDir, invocation.KeepLayer)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: %v\n", err)
		return 2
	}
	defer func() {
		if removeLayer {
			_ = os.RemoveAll(layerDir)
		} else {
			portableLayerMessage(true, layerDir)
		}
	}()
	if err := validatePortableLayerLocation(layerDir, workingDirectory); err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: %v\n", err)
		return 2
	}
	if invocation.SaveArtifacts {
		if err := validatePortableArtifactDirectory(layerDir, invocation.ArtifactDir, workingDirectory); err != nil {
			fmt.Fprintf(os.Stderr, "vfs-sandbox: artifact directory: %v\n", err)
			return 2
		}
	}
	runDir, err := os.MkdirTemp(os.TempDir(), "vfs-sandbox-run-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: create launcher directory: %v\n", err)
		return 125
	}
	defer os.RemoveAll(runDir)
	runDir, err = filepath.EvalSymlinks(runDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: resolve launcher directory: %v\n", err)
		return 125
	}
	if portablePathWithin(runDir, workingDirectory) {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: temporary launcher directory %q is inside the working directory; choose a working directory outside the system temporary directory\n", runDir)
		return 2
	}
	inputDir := filepath.Join(runDir, "input")
	if err := os.Mkdir(inputDir, 0700); err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: create launcher input directory: %v\n", err)
		return 125
	}
	snapshot, err := snapshotPortableTree(workingDirectory)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: record working directory: %v\n", err)
		return 125
	}
	manifest := portableManifest{
		Version:         1,
		Platform:        "windows-sandbox",
		CreatedAt:       time.Now().UTC(),
		SourceDirectory: workingDirectory,
		Workspace:       "workspace",
		Snapshot:        snapshot,
		Command:         invocation.Command,
		Arguments:       invocation.Args,
	}
	if err := writePortableManifest(layerDir, manifest); err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: %v\n", err)
		return 125
	}
	request, err := json.Marshal(windowsSandboxRequest{
		Command: windowsGuestCommandPath(invocation.Command, workingDirectory),
		Args:    invocation.Args,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: encode sandbox command: %v\n", err)
		return 125
	}
	if err := os.WriteFile(filepath.Join(inputDir, "request.json"), request, 0600); err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: write sandbox command: %v\n", err)
		return 125
	}
	if err := os.WriteFile(filepath.Join(inputDir, "run.ps1"), []byte(windowsSandboxScript), 0600); err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: write sandbox runner: %v\n", err)
		return 125
	}
	config := windowsSandboxConfig{
		Networking:           "Disable",
		ClipboardRedirection: "Disable",
		VGPU:                 "Disable",
		MappedFolders: windowsMappedFolders{Folders: []windowsMappedFolder{
			{HostFolder: inputDir, SandboxFolder: windowsSandboxInput, ReadOnly: true},
			{HostFolder: workingDirectory, SandboxFolder: windowsSandboxSource, ReadOnly: true},
			{HostFolder: layerDir, SandboxFolder: windowsSandboxOutput, ReadOnly: false},
		}},
		LogonCommand: windowsSandboxLogonEntry{
			Command: `powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File C:\vfs-sandbox-input\run.ps1`,
		},
	}
	configBytes, err := xml.MarshalIndent(config, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: encode Windows Sandbox configuration: %v\n", err)
		return 125
	}
	configPath := filepath.Join(runDir, "launch.wsb")
	if err := os.WriteFile(configPath, append([]byte(xml.Header), configBytes...), 0600); err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: write Windows Sandbox configuration: %v\n", err)
		return 125
	}
	fmt.Fprintln(os.Stderr, "vfs-sandbox: starting a disposable Windows Sandbox virtual machine")
	command := exec.Command(launcher, configPath)
	if err := command.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: Windows Sandbox did not finish normally: %v\n", err)
	}
	if invocation.SaveArtifacts {
		artifactPath, artifactCount, artifactBytes, artifactErr := exportPortableArtifacts(layerDir, manifest, invocation.ArtifactDir, workingDirectory)
		if artifactErr != nil {
			removeLayer = false
			fmt.Fprintf(os.Stderr, "vfs-sandbox: export runtime artifacts: %v\n", artifactErr)
			return 125
		}
		portableArtifactsMessage(artifactPath, artifactCount, artifactBytes)
	}
	if err := replaceWindowsSandboxManifest(layerDir, manifest); err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: refresh layer manifest: %v\n", err)
		return 125
	}
	resultPath := filepath.Join(layerDir, "status.json")
	resultBytes, err := readWindowsSandboxResult(resultPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: sandbox result is unavailable: %v\n", err)
		return 125
	}
	var result windowsSandboxResult
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: decode sandbox result: %v\n", err)
		return 125
	}
	if err := streamLayerFile(filepath.Join(layerDir, "stdout.txt"), os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: read sandbox stdout: %v\n", err)
		return 125
	}
	if err := streamLayerFile(filepath.Join(layerDir, "stderr.txt"), os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: read sandbox stderr: %v\n", err)
		return 125
	}
	if result.Error != "" {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: %s\n", result.Error)
	}
	return result.ExitCode
}

func findWindowsSandbox() (string, error) {
	launcher, err := exec.LookPath("WindowsSandbox.exe")
	if err == nil {
		return launcher, nil
	}
	if systemRoot := os.Getenv("SystemRoot"); systemRoot != "" {
		candidate := filepath.Join(systemRoot, "System32", "WindowsSandbox.exe")
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", err
}

func streamLayerFile(path string, destination io.Writer) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to read non-regular output file %q", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(destination, file)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func readWindowsSandboxResult(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("refusing to read non-regular result file %q", path)
	}
	if info.Size() > maxWindowsSandboxResultSize {
		return nil, fmt.Errorf("result file %q exceeds the %d-byte limit", path, maxWindowsSandboxResultSize)
	}
	return os.ReadFile(path)
}

func replaceWindowsSandboxManifest(layerDir string, manifest portableManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode layer manifest: %w", err)
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(layerDir, ".manifest-*.tmp")
	if err != nil {
		return fmt.Errorf("create replacement manifest: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protect replacement manifest: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write replacement manifest: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("flush replacement manifest: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close replacement manifest: %w", err)
	}
	manifestPath := filepath.Join(layerDir, "manifest.json")
	if info, err := os.Lstat(manifestPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			err = os.Remove(manifestPath)
		} else {
			err = os.RemoveAll(manifestPath)
		}
		if err != nil {
			return fmt.Errorf("remove guest manifest path: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect guest manifest path: %w", err)
	}
	if err := os.Rename(temporaryPath, manifestPath); err != nil {
		return fmt.Errorf("install replacement manifest: %w", err)
	}
	return nil
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage:
  vfs-sandbox [--layer-dir DIR | --keep-layer] [--save-artifacts [--artifact-dir DIR]] -- COMMAND [ARG...]
  vfs-sandbox diff LAYER_DIR

Run COMMAND in a disposable Windows Sandbox virtual machine. The host working
directory is mapped read-only and copied into the guest; its changes, output,
and exit status are exported to the layer directory. Networking and clipboard
sharing are disabled. Absolute command paths inside the host working directory
are redirected to the guest copy. Standard input is closed in the guest.

Options:
  --layer-dir DIR  create a retained layer at this new directory
  --keep-layer     retain the automatically created layer and print its path
  --save-artifacts  save changed files after the command exits (default: off)
  --artifact-dir DIR  choose a new artifact directory (requires --save-artifacts)
  -h, --help       show this help

Examples:
  vfs-sandbox -- cmd.exe /c ver
  vfs-sandbox --keep-layer -- cmd.exe /c echo sandboxed
  vfs-sandbox diff C:\path\to\layer`)
}

func windowsGuestCommandPath(command, hostWorkingDirectory string) string {
	if !filepath.IsAbs(command) {
		return command
	}
	commandPath, err := filepath.Abs(command)
	if err != nil || !portablePathWithin(commandPath, hostWorkingDirectory) {
		return command
	}
	relative, err := filepath.Rel(hostWorkingDirectory, commandPath)
	if err != nil {
		return command
	}
	if relative == "." {
		return windowsSandboxWork
	}
	return filepath.Join(windowsSandboxWork, relative)
}

const windowsSandboxScript = `$ErrorActionPreference = 'Stop'
$exitCode = 125
$failure = ''
$stdout = ''
$stderr = ''
function Quote-WindowsArgument([string]$Value) {
  $builder = New-Object System.Text.StringBuilder
  [void]$builder.Append('"')
  $slashes = 0
  foreach ($character in $Value.ToCharArray()) {
    if ($character -eq '\') { $slashes++; continue }
    if ($character -eq '"') {
      [void]$builder.Append(('\' * (2 * $slashes + 1)))
      [void]$builder.Append('"')
      $slashes = 0
      continue
    }
    if ($slashes -gt 0) { [void]$builder.Append(('\' * $slashes)); $slashes = 0 }
    [void]$builder.Append($character)
  }
  if ($slashes -gt 0) { [void]$builder.Append(('\' * (2 * $slashes))) }
  [void]$builder.Append('"')
  return $builder.ToString()
}
function Invoke-Robocopy([string]$Source, [string]$Destination, [string[]]$Options) {
  & robocopy.exe $Source $Destination @Options /R:0 /W:0 /NFL /NDL /NJH /NJS /NP
  if ($LASTEXITCODE -gt 7) { throw "Robocopy failed with exit code $LASTEXITCODE" }
}
try {
  $requestPath = 'C:\vfs-sandbox-input\request.json'
  $request = ConvertFrom-Json ([System.IO.File]::ReadAllText($requestPath, [System.Text.Encoding]::UTF8))
  $work = 'C:\vfs-sandbox-work'
  New-Item -ItemType Directory -Force -Path $work | Out-Null
  Invoke-Robocopy 'C:\vfs-sandbox-source' $work @('/E', '/COPY:DAT', '/DCOPY:DAT', '/SL')
  $arguments = @()
  if ($null -ne $request.arguments) { $arguments = @($request.arguments) }
  $psi = New-Object System.Diagnostics.ProcessStartInfo
  $psi.FileName = [string]$request.command
  $psi.Arguments = (($arguments | ForEach-Object { Quote-WindowsArgument ([string]$_) }) -join ' ')
  $psi.WorkingDirectory = $work
  $psi.UseShellExecute = $false
  $psi.CreateNoWindow = $true
  $psi.RedirectStandardInput = $true
  $psi.RedirectStandardOutput = $true
  $psi.RedirectStandardError = $true
  $process = New-Object System.Diagnostics.Process
  $process.StartInfo = $psi
  if (-not $process.Start()) { throw 'Unable to start the requested command' }
  $process.StandardInput.Close()
  $stdoutTask = $process.StandardOutput.ReadToEndAsync()
  $stderrTask = $process.StandardError.ReadToEndAsync()
  $process.WaitForExit()
  $stdout = $stdoutTask.GetAwaiter().GetResult()
  $stderr = $stderrTask.GetAwaiter().GetResult()
  $exitCode = $process.ExitCode
  if (-not (Test-Path -LiteralPath $work -PathType Container)) {
    New-Item -ItemType Directory -Force -Path $work | Out-Null
  }
} catch {
  $failure = $_.Exception.Message
  if ($exitCode -eq 125) { $stderr = $failure + [Environment]::NewLine + $stderr }
} finally {
  $encoding = [System.Text.UTF8Encoding]::new($false)
  $output = 'C:\vfs-sandbox-output'
  try {
    New-Item -ItemType Directory -Force -Path $output | Out-Null
    $workspaceOutput = Join-Path $output 'workspace'
    New-Item -ItemType Directory -Force -Path $workspaceOutput | Out-Null
    if (Test-Path -LiteralPath 'C:\vfs-sandbox-work' -PathType Container) {
      Invoke-Robocopy 'C:\vfs-sandbox-work' $workspaceOutput @('/MIR', '/COPY:DAT', '/DCOPY:DAT', '/SL')
    }
  } catch {
    if ($failure -eq '') { $failure = $_.Exception.Message }
    if ($exitCode -eq 0) { $exitCode = 125 }
  }
  try {
    [System.IO.File]::WriteAllText((Join-Path $output 'stdout.txt'), $stdout, $encoding)
    [System.IO.File]::WriteAllText((Join-Path $output 'stderr.txt'), $stderr, $encoding)
    $result = @{ exit_code = $exitCode; error = $failure } | ConvertTo-Json -Compress
    [System.IO.File]::WriteAllText((Join-Path $output 'status.json'), $result, $encoding)
  } catch {
    try { [System.IO.File]::WriteAllText((Join-Path $output 'launcher-error.txt'), $_.Exception.Message, $encoding) } catch {}
  }
  Start-Process -FilePath 'shutdown.exe' -ArgumentList '/s', '/t', '0', '/f' -WindowStyle Hidden
}
`
