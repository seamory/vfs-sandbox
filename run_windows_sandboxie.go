//go:build windows

package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type windowsSandboxieRequest struct {
	Command          string   `json:"command"`
	Arguments        []string `json:"arguments"`
	WorkingDirectory string   `json:"working_directory"`
	CaptureDirectory string   `json:"capture_directory"`
}

func runWindowsSandboxieFallback(invocation portableInvocation, workingDirectory, runDir, layerDir string, manifest portableManifest, removeLayer *bool) (exitCode int) {
	startPath, iniPath, err := findWindowsSandboxie()
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: Sandboxie-Plus is unavailable (%v); Minifilter is reserved and is not implemented\n", err)
		return 125
	}
	captureDirectory := filepath.Join(runDir, "sandboxie-output")
	if _, err := os.Lstat(captureDirectory); err == nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: Sandboxie-Plus capture path already exists: %s\n", captureDirectory)
		return 125
	} else if !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: inspect Sandboxie-Plus capture path: %v\n", err)
		return 125
	}
	if portablePathWithin(captureDirectory, workingDirectory) {
		fmt.Fprintln(os.Stderr, "vfs-sandbox: Sandboxie-Plus capture directory must be outside the working directory")
		return 125
	}
	captureRelative, err := windowsSandboxieDrivePath(captureDirectory)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: prepare Sandboxie-Plus capture path: %v\n", err)
		return 125
	}
	randomName := make([]byte, 12)
	if _, err := rand.Read(randomName); err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: generate Sandboxie-Plus box name: %v\n", err)
		return 125
	}
	boxName := "VFSSandbox" + hex.EncodeToString(randomName)
	boxRoot := filepath.Join(layerDir, "sandboxie-container")
	if err := os.Mkdir(boxRoot, 0700); err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: create Sandboxie-Plus file root: %v\n", err)
		return 125
	}
	boxConfigurationAttempted := false
	sandboxRunAttempted := false
	cleanup := func() error {
		if sandboxRunAttempted {
			if err := runWindowsSandboxieCommand(startPath, "/box:"+boxName, "/terminate"); err != nil {
				return fmt.Errorf("terminate box: %w", err)
			}
			if err := runWindowsSandboxieCommand(startPath, "/box:"+boxName, "delete_sandbox_silent"); err != nil {
				return fmt.Errorf("delete box contents: %w", err)
			}
		}
		if boxConfigurationAttempted {
			if err := runWindowsSandboxieConfig(iniPath, "set", boxName, "*", ""); err != nil {
				return fmt.Errorf("remove box configuration: %w", err)
			}
			if err := runWindowsSandboxieCommand(startPath, "/reload"); err != nil {
				return fmt.Errorf("reload configuration: %w", err)
			}
		}
		if err := os.RemoveAll(boxRoot); err != nil {
			return fmt.Errorf("remove file root: %w", err)
		}
		return nil
	}
	defer func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			*removeLayer = false
			fmt.Fprintf(os.Stderr, "vfs-sandbox: Sandboxie-Plus cleanup failed: %v; box data may remain under %s\n", cleanupErr, boxRoot)
			exitCode = 125
		}
	}()

	manifest.Platform = "sandboxie-plus"
	if err := writePortableManifest(layerDir, manifest); err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: write Sandboxie-Plus layer manifest: %v\n", err)
		return 125
	}
	request, err := json.Marshal(windowsSandboxieRequest{
		Command:          invocation.Command,
		Arguments:        invocation.Args,
		WorkingDirectory: workingDirectory,
		CaptureDirectory: captureDirectory,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: encode Sandboxie-Plus request: %v\n", err)
		return 125
	}
	inputDirectory := filepath.Join(runDir, "input")
	requestPath := filepath.Join(inputDirectory, "sandboxie-request.json")
	if err := os.WriteFile(requestPath, request, 0600); err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: write Sandboxie-Plus request: %v\n", err)
		return 125
	}
	scriptPath := filepath.Join(inputDirectory, "sandboxie-run.ps1")
	if err := os.WriteFile(scriptPath, []byte(windowsSandboxieScript), 0600); err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: write Sandboxie-Plus runner: %v\n", err)
		return 125
	}
	settings := [][2]string{
		{"FileRootPath", boxRoot},
		{"SeparateUserFolders", "n"},
		{"UseVolumeSerialNumbers", "n"},
		{"Enabled", "y"},
	}
	for _, setting := range settings {
		boxConfigurationAttempted = true
		if err := runWindowsSandboxieConfig(iniPath, "set", boxName, setting[0], setting[1]); err != nil {
			fmt.Fprintf(os.Stderr, "vfs-sandbox: configure Sandboxie-Plus box: %v\n", err)
			return 125
		}
	}
	if err := runWindowsSandboxieCommand(startPath, "/reload"); err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: activate Sandboxie-Plus box: %v\n", err)
		return 125
	}
	fmt.Fprintln(os.Stderr, "vfs-sandbox: starting a temporary Sandboxie-Plus box; installed network and direct-write rules apply")
	sandboxRunAttempted = true
	runErr := exec.Command(startPath,
		"/box:"+boxName,
		"/wait",
		"/silent",
		"powershell.exe",
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy",
		"Bypass",
		"-File",
		scriptPath,
		"-Request",
		requestPath,
	).Run()

	captureRoot, err := safePortableTreePath(boxRoot, captureRelative)
	if err != nil {
		message := "read Sandboxie-Plus capture"
		if runErr != nil {
			message += " after launcher error " + runErr.Error()
		}
		fmt.Fprintf(os.Stderr, "vfs-sandbox: %s: %v\n", message, err)
		return 125
	}
	workspaceSource, err := safePortableTreePath(captureRoot, "workspace")
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: read Sandboxie-Plus workspace: %v\n", err)
		return 125
	}
	if err := copyPortableTree(workspaceSource, filepath.Join(layerDir, manifest.Workspace)); err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: export Sandboxie-Plus workspace: %v\n", err)
		return 125
	}
	for _, name := range []string{"stdout.txt", "stderr.txt", "status.json"} {
		source := filepath.Join(captureRoot, name)
		destination := filepath.Join(layerDir, name)
		if name == "status.json" {
			data, err := readWindowsSandboxResult(source)
			if err != nil {
				fmt.Fprintf(os.Stderr, "vfs-sandbox: read Sandboxie-Plus result: %v\n", err)
				return 125
			}
			if err := os.WriteFile(destination, data, 0600); err != nil {
				fmt.Fprintf(os.Stderr, "vfs-sandbox: save Sandboxie-Plus result: %v\n", err)
				return 125
			}
			continue
		}
		if err := copyWindowsSandboxieOutput(source, destination); err != nil {
			fmt.Fprintf(os.Stderr, "vfs-sandbox: export Sandboxie-Plus command output: %v\n", err)
			return 125
		}
	}
	if invocation.SaveArtifacts {
		artifactPath, artifactCount, artifactBytes, artifactErr := exportPortableArtifacts(layerDir, manifest, invocation.ArtifactDir, workingDirectory)
		if artifactErr != nil {
			*removeLayer = false
			fmt.Fprintf(os.Stderr, "vfs-sandbox: export runtime artifacts: %v\n", artifactErr)
			return 125
		}
		portableArtifactsMessage(artifactPath, artifactCount, artifactBytes)
	}
	if err := replaceWindowsSandboxManifest(layerDir, manifest); err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: refresh layer manifest: %v\n", err)
		return 125
	}
	var result windowsSandboxResult
	resultBytes, err := readWindowsSandboxResult(filepath.Join(layerDir, "status.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: Sandboxie-Plus result is unavailable: %v\n", err)
		return 125
	}
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: decode Sandboxie-Plus result: %v\n", err)
		return 125
	}
	if err := streamLayerFile(filepath.Join(layerDir, "stdout.txt"), os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: read Sandboxie-Plus stdout: %v\n", err)
		return 125
	}
	if err := streamLayerFile(filepath.Join(layerDir, "stderr.txt"), os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: read Sandboxie-Plus stderr: %v\n", err)
		return 125
	}
	if result.Error != "" {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: %s\n", result.Error)
	}
	return result.ExitCode
}

func findWindowsSandboxie() (startPath, iniPath string, err error) {
	if start, startErr := exec.LookPath("Start.exe"); startErr == nil {
		if ini, iniErr := sandboxieSibling(start, "SbieIni.exe"); iniErr == nil {
			return start, ini, nil
		}
		if ini, iniErr := exec.LookPath("SbieIni.exe"); iniErr == nil {
			return start, ini, nil
		}
	}
	roots := []string{
		os.Getenv("ProgramW6432"),
		os.Getenv("ProgramFiles"),
		os.Getenv("ProgramFiles(x86)"),
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		for _, directory := range []string{"Sandboxie-Plus", "Sandboxie"} {
			start, startErr := sandboxieSibling(filepath.Join(root, directory), "Start.exe")
			ini, iniErr := sandboxieSibling(filepath.Join(root, directory), "SbieIni.exe")
			if startErr == nil && iniErr == nil {
				return start, ini, nil
			}
		}
	}
	return "", "", errors.New("Start.exe and SbieIni.exe were not found in PATH or standard installation directories")
}

func sandboxieSibling(path, name string) (string, error) {
	candidate := path
	if filepath.Base(path) != name {
		if filepath.Ext(path) != "" {
			candidate = filepath.Join(filepath.Dir(path), name)
		} else {
			candidate = filepath.Join(path, name)
		}
	}
	info, err := os.Stat(candidate)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", candidate)
	}
	return candidate, nil
}

func runWindowsSandboxieConfig(iniPath string, args ...string) error {
	output, err := exec.Command(iniPath, args...).CombinedOutput()
	if err != nil {
		details := strings.TrimSpace(string(output))
		if details != "" {
			return fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, details)
		}
		return fmt.Errorf("%s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func runWindowsSandboxieCommand(startPath string, args ...string) error {
	output, err := exec.Command(startPath, args...).CombinedOutput()
	if err != nil {
		details := strings.TrimSpace(string(output))
		if details != "" {
			return fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, details)
		}
		return fmt.Errorf("%s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func windowsSandboxieDrivePath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	volume := filepath.VolumeName(absolute)
	if len(volume) != 2 || volume[1] != ':' {
		return "", fmt.Errorf("capture directory %q must be on a drive-letter volume", absolute)
	}
	relative, err := filepath.Rel(volume+string(filepath.Separator), absolute)
	if err != nil {
		return "", err
	}
	if !filepath.IsLocal(relative) {
		return "", fmt.Errorf("capture directory %q is outside its drive root", absolute)
	}
	return filepath.Join("drive", strings.ToUpper(volume[:1]), relative), nil
}

func copyWindowsSandboxieOutput(sourcePath, destinationPath string) error {
	info, err := os.Lstat(sourcePath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to copy non-regular output file %q", sourcePath)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		_ = source.Close()
		return err
	}
	_, copyErr := io.Copy(destination, source)
	sourceErr := source.Close()
	destinationErr := destination.Close()
	if copyErr != nil {
		return copyErr
	}
	if sourceErr != nil {
		return sourceErr
	}
	return destinationErr
}

const windowsSandboxieScript = `param([string]$Request)
$ErrorActionPreference = 'Stop'
$exitCode = 125
$commandExitCode = 125
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
  $request = ConvertFrom-Json ([System.IO.File]::ReadAllText($Request, [System.Text.Encoding]::UTF8))
  $capture = [string]$request.capture_directory
  $source = [string]$request.working_directory
  New-Item -ItemType Directory -Force -Path $capture | Out-Null
  $psi = New-Object System.Diagnostics.ProcessStartInfo
  $psi.FileName = [string]$request.command
  $arguments = @()
  if ($null -ne $request.arguments) { $arguments = @($request.arguments) }
  $psi.Arguments = (($arguments | ForEach-Object { Quote-WindowsArgument ([string]$_) }) -join ' ')
  $psi.WorkingDirectory = $source
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
  $commandExitCode = $process.ExitCode
  $workspaceOutput = Join-Path $capture 'workspace'
  New-Item -ItemType Directory -Force -Path $workspaceOutput | Out-Null
  Invoke-Robocopy $source $workspaceOutput @('/MIR', '/COPY:DAT', '/DCOPY:DAT', '/SL')
} catch {
  $failure = $_.Exception.Message
} finally {
  $encoding = [System.Text.UTF8Encoding]::new($false)
  try {
    New-Item -ItemType Directory -Force -Path $capture | Out-Null
    if ($failure -eq '') {
      $exitCode = $commandExitCode
    } else {
      $exitCode = 125
      $stderr = $failure + [Environment]::NewLine + $stderr
    }
    [System.IO.File]::WriteAllText((Join-Path $capture 'stdout.txt'), $stdout, $encoding)
    [System.IO.File]::WriteAllText((Join-Path $capture 'stderr.txt'), $stderr, $encoding)
    $result = @{ exit_code = $exitCode; error = $failure } | ConvertTo-Json -Compress
    [System.IO.File]::WriteAllText((Join-Path $capture 'status.json'), $result, $encoding)
  } catch {
    try { [System.IO.File]::WriteAllText((Join-Path $capture 'launcher-error.txt'), $_.Exception.Message, $encoding) } catch {}
  }
}
exit $exitCode
`
