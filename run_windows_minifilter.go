//go:build windows

package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"vfs-sandbox/internal/minicow"
)

const miniPathChars = 2048
const miniRequestSize = 16 + 4*miniPathChars
const miniReplySize = 8 + 2*miniPathChars

// Filter Manager headers use an 8-byte-aligned ULONGLONG MessageId on both
// Windows x86 and x64. Do not use Go's uint64 struct alignment on 386 here.
const miniFilterHeaderSize = 16
const miniVersion = 1
const miniRegister = 1
const miniResolve = 2
const miniList = 3
const miniRename = 4
const miniWritable = 1
const miniDirectory = 2
const miniReplace = 4

var miniKernel = syscall.NewLazyDLL("kernel32.dll")
var miniFlt = syscall.NewLazyDLL("fltlib.dll")

func miniHRESULT(value uintptr) error {
	if int32(value) < 0 {
		return fmt.Errorf("Windows HRESULT 0x%08x", uint32(value))
	}
	return nil
}
func miniPutPath(buffer []byte, path string) error {
	if strings.ContainsRune(path, 0) {
		return errors.New("path contains NUL")
	}
	units := utf16.Encode([]rune(path))
	if len(units) >= miniPathChars {
		return errors.New("path exceeds Minifilter protocol limit (2047 UTF-16 units)")
	}
	for i, v := range units {
		binary.LittleEndian.PutUint16(buffer[2*i:], v)
	}
	return nil
}
func miniGetPath(buffer []byte) (string, error) {
	var units []uint16
	for i := 0; i+1 < len(buffer); i += 2 {
		v := binary.LittleEndian.Uint16(buffer[i:])
		if v == 0 {
			return string(utf16.Decode(units)), nil
		}
		units = append(units, v)
	}
	return "", errors.New("unterminated protocol path")
}

type miniVolume struct{ drive, device string }
type miniBroker struct {
	overlay          *minicow.Overlay
	volumes          []miniVolume
	upperNT, layerNT string
	mu               sync.Mutex
}

func miniPrefix(path, root string) bool {
	return strings.EqualFold(path, root) || (len(path) > len(root) && strings.EqualFold(path[:len(root)], root) && path[len(root)] == '\\')
}
func (b *miniBroker) logical(path string) (string, error) {
	if miniPrefix(path, b.upperNT) {
		rel := strings.TrimPrefix(path[len(b.upperNT):], `\`)
		if rel == "" {
			return "", fs.ErrPermission
		}
		return filepath.ToSlash(rel), nil
	}
	if miniPrefix(path, b.layerNT) {
		return "", fs.ErrPermission
	}
	for _, v := range b.volumes {
		if miniPrefix(path, v.device) {
			return v.drive + "/" + filepath.ToSlash(strings.TrimPrefix(path[len(v.device):], `\`)), nil
		}
	}
	return "", fmt.Errorf("unsupported device path %q: %w", path, fs.ErrPermission)
}
func (b *miniBroker) nt(path string) (string, error) {
	volume := filepath.VolumeName(path)
	for _, v := range b.volumes {
		if strings.EqualFold(volume, v.drive+":") {
			return v.device + path[len(volume):], nil
		}
	}
	return "", fs.ErrPermission
}
func miniStatus(err error) uint32 {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, fs.ErrNotExist):
		return 0xc0000034
	case errors.Is(err, fs.ErrExist):
		return 0xc0000035
	case errors.Is(err, fs.ErrPermission):
		return 0xc0000022
	default:
		return 0xc0000001
	}
}
func (b *miniBroker) request(data []byte) []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	reply := make([]byte, miniReplySize)
	var err error
	if len(data) != miniRequestSize || binary.LittleEndian.Uint32(data) != miniVersion {
		binary.LittleEndian.PutUint32(reply, 0xc000000d)
		return reply
	}
	op, flags := binary.LittleEndian.Uint32(data[4:]), binary.LittleEndian.Uint32(data[8:])
	name, err := miniGetPath(data[16 : 16+2*miniPathChars])
	var logical string
	if err == nil {
		logical, err = b.logical(name)
	}
	var result string
	if err == nil {
		switch op {
		case miniResolve:
			result, err = b.overlay.Resolve(logical, flags&miniWritable != 0, flags&miniDirectory != 0)
			if err == nil {
				result, err = b.nt(result)
			}
			if err == nil {
				err = miniPutPath(reply[8:], result)
			}
		case miniList:
			err = b.overlay.List(logical)
		case miniRename:
			var target string
			target, err = miniGetPath(data[16+2*miniPathChars:])
			if err == nil {
				target, err = b.logical(target)
			}
			if err == nil {
				err = b.overlay.Rename(logical, target, flags&miniReplace != 0)
			}
		default:
			err = fs.ErrInvalid
		}
	}
	binary.LittleEndian.PutUint32(reply, miniStatus(err))
	return reply
}

func miniVolumes() ([]miniVolume, error) {
	var result []miniVolume
	query := miniKernel.NewProc("QueryDosDeviceW")
	getType := miniKernel.NewProc("GetDriveTypeW")
	getInfo := miniKernel.NewProc("GetVolumeInformationW")
	for drive := 'A'; drive <= 'Z'; drive++ {
		name, _ := syscall.UTF16PtrFromString(string(drive) + ":")
		root, _ := syscall.UTF16PtrFromString(string(drive) + `:\`)
		kind, _, _ := getType.Call(uintptr(unsafe.Pointer(root)))
		if kind != 3 {
			continue
		} // fixed local disks
		var format [64]uint16
		ok, _, _ := getInfo.Call(uintptr(unsafe.Pointer(root)), 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&format[0])), uintptr(len(format)))
		if ok == 0 || !strings.EqualFold(syscall.UTF16ToString(format[:]), "NTFS") {
			continue
		}
		var device [miniPathChars]uint16
		n, _, err := query.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&device[0])), uintptr(len(device)))
		if n == 0 {
			return nil, err
		}
		result = append(result, miniVolume{string(drive), syscall.UTF16ToString(device[:])})
	}
	if len(result) == 0 {
		return nil, errors.New("no fixed NTFS volumes available")
	}
	return result, nil
}

func miniConnect() (syscall.Handle, error) {
	// Load from System32 explicitly; do not search the working directory for DLLs.
	system := os.Getenv("SystemRoot")
	if system == "" {
		return 0, errors.New("SystemRoot is unset")
	}
	miniFlt = syscall.NewLazyDLL(filepath.Join(system, "System32", "fltlib.dll"))
	proc := miniFlt.NewProc("FilterConnectCommunicationPort")
	if err := proc.Find(); err != nil {
		return 0, err
	}
	name, _ := syscall.UTF16PtrFromString(`\VfsSandboxPort`)
	var handle syscall.Handle
	code, _, _ := proc.Call(uintptr(unsafe.Pointer(name)), 0, 0, 0, 0, uintptr(unsafe.Pointer(&handle)))
	return handle, miniHRESULT(code)
}

// serve uses one overlapped receive, which can be cancelled on process exit.
// Serial replies make file-copy and whiteout transitions atomic to the broker.
func (b *miniBroker) serve(port syscall.Handle, stop <-chan struct{}, done chan<- error) {
	var result error
	defer func() { done <- result }()
	event, _, err := miniKernel.NewProc("CreateEventW").Call(0, 1, 0, 0)
	if event == 0 {
		result = err
		return
	}
	defer syscall.CloseHandle(syscall.Handle(event))
	for {
		select {
		case <-stop:
			return
		default:
		}
		message := make([]byte, miniFilterHeaderSize+miniRequestSize)
		ov := syscall.Overlapped{HEvent: syscall.Handle(event)}
		miniKernel.NewProc("ResetEvent").Call(event)
		code, _, _ := miniFlt.NewProc("FilterGetMessage").Call(uintptr(port), uintptr(unsafe.Pointer(&message[0])), uintptr(len(message)), uintptr(unsafe.Pointer(&ov)))
		if uint32(code) == 0x800703e5 {
			for {
				wait, e := syscall.WaitForSingleObject(syscall.Handle(event), 100)
				if e != nil {
					miniKernel.NewProc("CancelIoEx").Call(uintptr(port), uintptr(unsafe.Pointer(&ov)))
					var transferred uint32
					miniKernel.NewProc("GetOverlappedResult").Call(uintptr(port), uintptr(unsafe.Pointer(&ov)), uintptr(unsafe.Pointer(&transferred)), 1)
					result = e
					return
				}
				if wait == 0 {
					break
				}
				select {
				case <-stop:
					miniKernel.NewProc("CancelIoEx").Call(uintptr(port), uintptr(unsafe.Pointer(&ov)))
					syscall.WaitForSingleObject(syscall.Handle(event), syscall.INFINITE)
					return
				default:
				}
			}
			var transferred uint32
			ok, _, e := miniKernel.NewProc("GetOverlappedResult").Call(uintptr(port), uintptr(unsafe.Pointer(&ov)), uintptr(unsafe.Pointer(&transferred)), 0)
			if ok == 0 {
				result = e
				return
			}
		} else if e := miniHRESULT(code); e != nil {
			result = e
			return
		}
		body := b.request(message[miniFilterHeaderSize:])
		response := make([]byte, miniFilterHeaderSize+len(body)) // FILTER_REPLY_HEADER.Status == STATUS_SUCCESS
		copy(response[8:16], message[8:16])
		copy(response[miniFilterHeaderSize:], body)
		code, _, _ = miniFlt.NewProc("FilterReplyMessage").Call(uintptr(port), uintptr(unsafe.Pointer(&response[0])), uintptr(len(response)))
		if e := miniHRESULT(code); e != nil {
			result = e
			return
		}
	}
}

func runWindowsMinifilter(invocation portableInvocation, workingDirectory, layerDir string, manifest portableManifest, removeLayer *bool) int {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "386" {
		fmt.Fprintln(os.Stderr, "vfs-sandbox: Minifilter requires Windows amd64 or 386")
		return 125
	}
	port, err := miniConnect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "vfs-sandbox: Minifilter unavailable: %v; install/load the signed VfsSandbox driver and run with administrator privileges\n", err)
		return 125
	}
	defer syscall.CloseHandle(port)
	volumes, err := miniVolumes()
	if err != nil {
		fmt.Fprintln(os.Stderr, "vfs-sandbox:", err)
		return 125
	}
	if err = miniCheckAttachments(volumes); err != nil {
		fmt.Fprintln(os.Stderr, "vfs-sandbox: Minifilter volume coverage:", err)
		return 125
	}
	roots := map[string]string{}
	for _, v := range volumes {
		roots[v.drive] = v.drive + `:\`
	}
	upper := filepath.Join(layerDir, "minifilter-upper")
	overlay, err := minicow.New(upper, roots)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vfs-sandbox:", err)
		return 125
	}
	broker := &miniBroker{overlay: overlay, volumes: volumes}
	broker.upperNT, err = broker.nt(upper)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vfs-sandbox: layer must be on a fixed NTFS drive")
		return 125
	}
	broker.layerNT, err = broker.nt(layerDir)
	if err != nil {
		return 125
	}
	cwdNT, err := broker.nt(workingDirectory)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vfs-sandbox: working directory must be on a fixed NTFS drive")
		return 125
	}
	cwdLogical, err := broker.logical(cwdNT)
	if err != nil {
		return 125
	}
	manifest.Platform = "windows-minifilter"
	if err = writePortableManifest(layerDir, manifest); err != nil {
		fmt.Fprintln(os.Stderr, "vfs-sandbox:", err)
		return 125
	}
	controlDir := filepath.Join(upper, ".control")
	if err = os.Mkdir(controlDir, 0700); err != nil {
		return 125
	}
	process, job, err := miniStartSuspended(invocation, workingDirectory, controlDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vfs-sandbox: create suspended command:", err)
		return 125
	}
	defer syscall.CloseHandle(process.Process)
	defer syscall.CloseHandle(process.Thread)
	defer syscall.CloseHandle(job) // kill-on-close also applies if the launcher crashes
	resumed := false
	defer func() {
		if !resumed {
			syscall.TerminateProcess(process.Process, 125)
		}
	}()
	message := make([]byte, 16+2*miniPathChars)
	binary.LittleEndian.PutUint32(message, miniVersion)
	binary.LittleEndian.PutUint32(message[4:], miniRegister)
	binary.LittleEndian.PutUint32(message[8:], process.ProcessId)
	if err = miniPutPath(message[16:], broker.upperNT); err != nil {
		fmt.Fprintln(os.Stderr, "vfs-sandbox:", err)
		return 125
	}
	var returned uint32
	code, _, _ := miniFlt.NewProc("FilterSendMessage").Call(uintptr(port), uintptr(unsafe.Pointer(&message[0])), uintptr(len(message)), 0, 0, uintptr(unsafe.Pointer(&returned)))
	if err = miniHRESULT(code); err != nil {
		fmt.Fprintln(os.Stderr, "vfs-sandbox: register process with Minifilter:", err)
		return 125
	}
	done := make(chan error, 1)
	stop := make(chan struct{})
	var stopOnce sync.Once
	brokerStopped := false
	stopBroker := func() {
		stopOnce.Do(func() { close(stop) })
		if !brokerStopped {
			<-done
			brokerStopped = true
		}
	}
	go broker.serve(port, stop, done)
	defer stopBroker()
	fmt.Fprintln(os.Stderr, "vfs-sandbox: running with Minifilter CoW on fixed NTFS volumes")
	result, _, resumeErr := miniKernel.NewProc("ResumeThread").Call(uintptr(process.Thread))
	if uint32(result) == 0xffffffff {
		fmt.Fprintln(os.Stderr, "vfs-sandbox: resume command:", resumeErr)
		return 125
	}
	resumed = true
	waited := make(chan error, 1)
	go func() { _, e := syscall.WaitForSingleObject(process.Process, syscall.INFINITE); waited <- e }()
	var runErr error
	select {
	case runErr = <-waited:
	case runErr = <-done:
		brokerStopped = true
		miniKernel.NewProc("TerminateJobObject").Call(uintptr(job), 125)
		<-waited
	}
	var exitCode uint32
	if err = syscall.GetExitCodeProcess(process.Process, &exitCode); err != nil {
		runErr = err
	}
	// Stop descendants before examining or exporting their private files.
	ok, _, terminateErr := miniKernel.NewProc("TerminateJobObject").Call(uintptr(job), uintptr(exitCode))
	if ok == 0 {
		*removeLayer = false
		fmt.Fprintln(os.Stderr, "vfs-sandbox: terminate Minifilter process tree:", terminateErr)
		return 125
	}
	// Job accounting reaches zero only after all descendant process exits.
	deadline := time.Now().Add(30 * time.Second)
	for {
		var accounting [48]byte
		var size uint32
		ok, _, e := miniKernel.NewProc("QueryInformationJobObject").Call(uintptr(job), 1, uintptr(unsafe.Pointer(&accounting[0])), uintptr(len(accounting)), uintptr(unsafe.Pointer(&size)))
		if ok == 0 {
			*removeLayer = false
			fmt.Fprintln(os.Stderr, "vfs-sandbox: wait for process tree:", e)
			return 125
		}
		if binary.LittleEndian.Uint32(accounting[40:]) == 0 {
			break
		}
		if time.Now().After(deadline) {
			*removeLayer = false
			fmt.Fprintln(os.Stderr, "vfs-sandbox: timed out waiting for Minifilter descendants; retaining layer")
			return 125
		}
		time.Sleep(10 * time.Millisecond)
	}
	stopBroker()
	if runErr != nil {
		fmt.Fprintln(os.Stderr, "vfs-sandbox: Minifilter session:", runErr)
		return 125
	}
	if err = overlay.ExportView(cwdLogical, filepath.Join(layerDir, "workspace")); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			err = os.MkdirAll(filepath.Join(layerDir, "workspace"), 0700)
		}
		if err != nil {
			*removeLayer = false
			fmt.Fprintln(os.Stderr, "vfs-sandbox: export Minifilter workspace:", err)
			return 125
		}
	}
	changes, err := overlay.Changes()
	if err != nil {
		*removeLayer = false
		fmt.Fprintln(os.Stderr, "vfs-sandbox: collect Minifilter changes:", err)
		return 125
	}
	if err = writeMinifilterReport(layerDir, changes); err != nil {
		*removeLayer = false
		fmt.Fprintln(os.Stderr, "vfs-sandbox: save Minifilter changes:", err)
		return 125
	}
	for _, name := range []string{"stdout.txt", "stderr.txt"} {
		if err = copyWindowsSandboxieOutput(filepath.Join(controlDir, name), filepath.Join(layerDir, name)); err != nil {
			fmt.Fprintln(os.Stderr, "vfs-sandbox:", err)
			return 125
		}
	}
	status, _ := json.Marshal(windowsSandboxResult{ExitCode: int(exitCode)})
	if err = os.WriteFile(filepath.Join(layerDir, "status.json"), status, 0600); err != nil {
		return 125
	}
	if invocation.SaveArtifacts {
		path, count, bytes, e := exportMinifilterArtifacts(layerDir, changes, invocation.ArtifactDir, workingDirectory)
		if e != nil {
			*removeLayer = false
			fmt.Fprintln(os.Stderr, "vfs-sandbox: export artifacts:", e)
			return 125
		}
		portableArtifactsMessage(path, count, bytes)
	}
	if err = streamLayerFile(filepath.Join(layerDir, "stdout.txt"), os.Stdout); err != nil {
		return 125
	}
	if err = streamLayerFile(filepath.Join(layerDir, "stderr.txt"), os.Stderr); err != nil {
		return 125
	}
	return int(exitCode)
}

func miniStartSuspended(invocation portableInvocation, cwd, output string) (pi syscall.ProcessInformation, job syscall.Handle, err error) {
	// Only standard I/O handles are deliberately inherited. The driver port and
	// job handles are not inheritable, so a child cannot release its isolation.
	var handles []syscall.Handle
	defer func() {
		for _, h := range handles {
			syscall.CloseHandle(h)
		}
	}()
	open := func(path string, access, disposition uint32) (syscall.Handle, error) {
		p, e := syscall.UTF16PtrFromString(path)
		if e != nil {
			return 0, e
		}
		sa := syscall.SecurityAttributes{Length: uint32(unsafe.Sizeof(syscall.SecurityAttributes{})), InheritHandle: 1}
		h, e := syscall.CreateFile(p, access, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, &sa, disposition, syscall.FILE_ATTRIBUTE_NORMAL, 0)
		if e == nil {
			handles = append(handles, h)
		}
		return h, e
	}
	input, e := open("NUL", syscall.GENERIC_READ, syscall.OPEN_EXISTING)
	if e != nil {
		err = e
		return
	}
	stdout, e := open(filepath.Join(output, "stdout.txt"), syscall.GENERIC_WRITE, syscall.CREATE_NEW)
	if e != nil {
		err = e
		return
	}
	stderr, e := open(filepath.Join(output, "stderr.txt"), syscall.GENERIC_WRITE, syscall.CREATE_NEW)
	if e != nil {
		err = e
		return
	}
	command, e := exec.LookPath(invocation.Command)
	if e != nil {
		err = e
		return
	}
	command, e = filepath.Abs(command)
	if e != nil {
		err = e
		return
	}
	parts := []string{syscall.EscapeArg(command)}
	for _, arg := range invocation.Args {
		parts = append(parts, syscall.EscapeArg(arg))
	}
	cmd, e := syscall.UTF16PtrFromString(strings.Join(parts, " "))
	if e != nil {
		err = e
		return
	}
	app, e := syscall.UTF16PtrFromString(command)
	if e != nil {
		err = e
		return
	}
	dir, e := syscall.UTF16PtrFromString(cwd)
	if e != nil {
		err = e
		return
	}
	si := syscall.StartupInfo{Cb: uint32(unsafe.Sizeof(syscall.StartupInfo{})), Flags: syscall.STARTF_USESTDHANDLES, StdInput: input, StdOutput: stdout, StdErr: stderr}
	j, _, e := miniKernel.NewProc("CreateJobObjectW").Call(0, 0)
	if j == 0 {
		err = e
		return
	}
	job = syscall.Handle(j)
	defer func() {
		if err != nil {
			syscall.CloseHandle(job)
		}
	}()
	// JOBOBJECT_EXTENDED_LIMIT_INFORMATION contains SIZE_T/ULONG_PTR fields.
	// Basic limits occupy 48 bytes on x86 and 64 on x64, followed by 48 bytes
	// of IO_COUNTERS and four SIZE_T fields. LimitFlags is at offset 16 in both.
	limitSize := 144
	if runtime.GOARCH == "386" {
		limitSize = 112
	}
	limits := make([]byte, limitSize)
	binary.LittleEndian.PutUint32(limits[16:], 0x2000) // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	ok, _, e := miniKernel.NewProc("SetInformationJobObject").Call(j, 9, uintptr(unsafe.Pointer(&limits[0])), uintptr(len(limits)))
	if ok == 0 {
		err = e
		return
	}
	err = syscall.CreateProcess(app, cmd, nil, nil, true, 0x4|0x400, nil, dir, &si, &pi)
	if err != nil {
		return
	}
	ok, _, e = miniKernel.NewProc("AssignProcessToJobObject").Call(j, uintptr(pi.Process))
	if ok == 0 {
		syscall.TerminateProcess(pi.Process, 125)
		syscall.CloseHandle(pi.Process)
		syscall.CloseHandle(pi.Thread)
		err = e
	}
	return
}

func miniCheckAttachments(volumes []miniVolume) error {
	name, _ := syscall.UTF16PtrFromString("VfsSandbox")
	buffer := make([]byte, 64*1024)
	var returned uint32
	var search syscall.Handle
	code, _, _ := miniFlt.NewProc("FilterInstanceFindFirst").Call(uintptr(unsafe.Pointer(name)), 2, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), uintptr(unsafe.Pointer(&returned)), uintptr(unsafe.Pointer(&search)))
	if err := miniHRESULT(code); err != nil {
		return err
	}
	defer miniFlt.NewProc("FilterInstanceFindClose").Call(uintptr(search))
	attached := map[string]bool{}
	for {
		if returned > uint32(len(buffer)) {
			return errors.New("invalid filter instance length")
		}
		current := buffer[:returned]
		for {
			if len(current) < 20 {
				return errors.New("truncated filter instance information")
			}
			length := int(binary.LittleEndian.Uint16(current[12:]))
			offset := int(binary.LittleEndian.Uint16(current[14:]))
			if length%2 != 0 || offset < 20 || offset+length > len(current) {
				return errors.New("invalid filter volume name")
			}
			units := make([]uint16, length/2)
			for i := range units {
				units[i] = binary.LittleEndian.Uint16(current[offset+2*i:])
			}
			attached[strings.ToLower(string(utf16.Decode(units)))] = true
			next := int(binary.LittleEndian.Uint32(current))
			if next == 0 {
				break
			}
			if next < 20 || next%8 != 0 || next >= len(current) {
				return errors.New("invalid filter instance offset")
			}
			current = current[next:]
		}
		code, _, _ = miniFlt.NewProc("FilterInstanceFindNext").Call(uintptr(search), 2, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), uintptr(unsafe.Pointer(&returned)))
		if uint32(code) == 0x80070103 {
			break
		} // ERROR_NO_MORE_ITEMS
		if err := miniHRESULT(code); err != nil {
			return err
		}
	}
	for _, volume := range volumes {
		if !attached[strings.ToLower(volume.device)] {
			return fmt.Errorf("VfsSandbox is not attached to %s: (%s)", volume.drive, volume.device)
		}
	}
	return nil
}
