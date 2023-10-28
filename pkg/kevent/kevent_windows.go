/*
 * Copyright 2020-2021 by Nedim Sabic Sabic
 * https://www.fibratus.io
 * All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package kevent

import (
	"encoding/binary"
	"fmt"
	"github.com/rabbitstack/fibratus/pkg/kevent/kparams"
	"github.com/rabbitstack/fibratus/pkg/kevent/ktypes"
	"github.com/rabbitstack/fibratus/pkg/sys"
	"github.com/rabbitstack/fibratus/pkg/sys/etw"
	"github.com/rabbitstack/fibratus/pkg/util/filetime"
	"github.com/rabbitstack/fibratus/pkg/util/hashers"
	"github.com/rabbitstack/fibratus/pkg/util/hostname"
	"github.com/rabbitstack/fibratus/pkg/util/ntstatus"
	"golang.org/x/sys/windows"
	"os"
	"strings"
	"unsafe"
)

var (
	// DropCurrentProc determines if the events generated by the current, i.e. Fibratus process, are dropped
	DropCurrentProc = true
	// currentPid is the current process identifier
	currentPid = uint32(os.Getpid())
	// rundowns stores the hashes of processed rundown events
	rundowns = map[uint64]bool{}
)

// New constructs a fresh event instance with basic fields and parameters. If the published
// ETW event is not recognized as a valid event in our internal types, then we return a nil
// event.
func New(seq uint64, ktype ktypes.Ktype, evt *etw.EventRecord) *Kevent {
	var (
		pid = evt.Header.ProcessID
		tid = evt.Header.ThreadID
		cpu = *(*uint8)(unsafe.Pointer(&evt.BufferContext.ProcessorIndex[0]))
		ts  = filetime.ToEpoch(evt.Header.Timestamp)
	)
	e := pool.Get().(*Kevent)
	*e = Kevent{
		Seq:         seq,
		PID:         pid,
		Tid:         tid,
		CPU:         cpu,
		Type:        ktype,
		Category:    ktype.Category(),
		Name:        ktype.String(),
		Kparams:     make(map[string]*Kparam),
		Description: ktype.Description(),
		Timestamp:   ts,
		Metadata:    make(map[MetadataKey]any),
		Host:        hostname.Get(),
	}
	e.produceParams(evt)
	e.adjustPID()
	return e
}

func (e *Kevent) adjustPID() {
	switch e.Category {
	case ktypes.Image:
		// sometimes the pid present in event header is invalid
		// but, we can get the valid one from the event parameters
		if e.InvalidPid() {
			e.PID, _ = e.Kparams.GetPid()
		}
	case ktypes.File:
		e.Tid, _ = e.Kparams.GetTid()
		switch {
		case e.InvalidPid() && e.Type == ktypes.MapFileRundown:
			// a valid pid for map rundown events
			// is located in the event parameters
			e.PID = e.Kparams.MustGetPid()
		case e.InvalidPid():
			// on some Windows versions the value of
			// the PID is invalid in the event header
			access := uint32(windows.THREAD_QUERY_LIMITED_INFORMATION)
			thread, err := windows.OpenThread(access, false, e.Tid)
			if err != nil {
				return
			}
			defer func() {
				_ = windows.CloseHandle(thread)
			}()
			e.PID = sys.GetProcessIdOfThread(thread)
		}
	case ktypes.Process:
		// process start events may be logged in the context of the parent or child process.
		// As a result, the ProcessId member of EVENT_TRACE_HEADER may not correspond to the
		// process being created, so we set the event pid to be the one of the parent process
		if e.IsCreateProcess() {
			e.PID, _ = e.Kparams.GetPpid()
		}
	case ktypes.Net:
		if !e.IsDNS() {
			e.PID, _ = e.Kparams.GetPid()
		}
	case ktypes.Handle:
		if e.Type == ktypes.DuplicateHandle {
			e.PID, _ = e.Kparams.GetUint32(kparams.TargetProcessID)
			e.Kparams.Remove(kparams.TargetProcessID)
		}
	}
}

// IsDropped determines if the event should be dropped. The event
// is dropped in under the following circumstances:
//
// 1. The event is dealing with state management, and as long as
// we're not storing them into the capture file, it can be dropped
// 2. Rundowns events are dropped if they haven't been processed already
// 3. If the event is generated by Fibratus process, we can safely ignore it
func (e Kevent) IsDropped(capture bool) bool {
	if e.IsState() && !capture {
		return true
	}
	if e.IsRundown() && e.IsRundownProcessed() {
		return true
	}
	return DropCurrentProc && e.CurrentPid()
}

// DelayKey returns the value that is used to
// store and reference delayed events in the event
// backlog state. The delayed event is indexed by
// the sequence identifier.
func (e *Kevent) DelayKey() uint64 {
	switch e.Type {
	case ktypes.CreateHandle, ktypes.CloseHandle:
		return e.Kparams.MustGetUint64(kparams.HandleObject)
	}
	return 0
}

// IsNetworkTCP determines whether the event pertains to network TCP events.
func (e Kevent) IsNetworkTCP() bool {
	return e.Category == ktypes.Net && !e.IsNetworkUDP()
}

// IsNetworkUDP determines whether the event pertains to network UDP events.
func (e Kevent) IsNetworkUDP() bool {
	return e.Type == ktypes.RecvUDPv4 || e.Type == ktypes.RecvUDPv6 || e.Type == ktypes.SendUDPv4 || e.Type == ktypes.SendUDPv6
}

// IsDNS determines whether the event is a DNS question/answer.
func (e Kevent) IsDNS() bool {
	return e.Type.Subcategory() == ktypes.DNS
}

// IsRundown determines if this is a rundown events.
func (e Kevent) IsRundown() bool {
	return e.Type == ktypes.ProcessRundown || e.Type == ktypes.ThreadRundown || e.Type == ktypes.ImageRundown ||
		e.Type == ktypes.FileRundown || e.Type == ktypes.RegKCBRundown
}

// IsSuccess checks if the event contains the status parameter
// and in such case, returns true if the operation completed
// successfully, i.e. the system code is equal to ERROR_SUCCESS.
func (e Kevent) IsSuccess() bool {
	if !e.Kparams.Contains(kparams.NTStatus) {
		return true
	}
	return e.GetParamAsString(kparams.NTStatus) == ntstatus.Success
}

// IsRundownProcessed checks if the rundown events was processed
// to discard writing the snapshot state if the process/module is
// already present. This usually happens when we purposely alter
// the tracing session to induce the arrival of rundown events
// by calling into the `etw.SetTraceInformation` Windows API
// function which causes duplicate rundown events.
// For more pointers check `kstream/controller_windows.go`
// and the `etw.SetTraceInformation` API function.
func (e Kevent) IsRundownProcessed() bool {
	key := e.RundownKey()
	_, isProcessed := rundowns[key]
	if isProcessed {
		return true
	}
	rundowns[key] = true
	return false
}

func (e Kevent) IsCreateFile() bool       { return e.Type == ktypes.CreateFile }
func (e Kevent) IsCreateProcess() bool    { return e.Type == ktypes.CreateProcess }
func (e Kevent) IsCreateThread() bool     { return e.Type == ktypes.CreateThread }
func (e Kevent) IsCloseFile() bool        { return e.Type == ktypes.CloseFile }
func (e Kevent) IsCreateHandle() bool     { return e.Type == ktypes.CreateHandle }
func (e Kevent) IsCloseHandle() bool      { return e.Type == ktypes.CloseHandle }
func (e Kevent) IsDeleteFile() bool       { return e.Type == ktypes.DeleteFile }
func (e Kevent) IsEnumDirectory() bool    { return e.Type == ktypes.EnumDirectory }
func (e Kevent) IsTerminateProcess() bool { return e.Type == ktypes.TerminateProcess }
func (e Kevent) IsTerminateThread() bool  { return e.Type == ktypes.TerminateThread }
func (e Kevent) IsUnloadImage() bool      { return e.Type == ktypes.UnloadImage }
func (e Kevent) IsLoadImage() bool        { return e.Type == ktypes.LoadImage }
func (e Kevent) IsImageRundown() bool     { return e.Type == ktypes.ImageRundown }
func (e Kevent) IsFileOpEnd() bool        { return e.Type == ktypes.FileOpEnd }
func (e Kevent) IsRegSetValue() bool      { return e.Type == ktypes.RegSetValue }
func (e Kevent) IsProcessRundown() bool   { return e.Type == ktypes.ProcessRundown }
func (e Kevent) IsVirtualAlloc() bool     { return e.Type == ktypes.VirtualAlloc }
func (e Kevent) IsMapViewFile() bool      { return e.Type == ktypes.MapViewFile }
func (e Kevent) IsUnmapViewFile() bool    { return e.Type == ktypes.UnmapViewFile }

// InvalidPid indicates if the process generating the event is invalid.
func (e Kevent) InvalidPid() bool { return e.PID == sys.InvalidProcessID }

// CurrentPid indicates if Fibratus is the process generating the event.
func (e Kevent) CurrentPid() bool { return e.PID == currentPid }

// IsState indicates if this event is only used for state management.
func (e Kevent) IsState() bool { return e.Type.OnlyState() }

// IsCreateDisposition determines if the file disposition leads to creating a new file.
func (e Kevent) IsCreateDisposition() bool {
	return e.IsCreateFile() && e.Kparams.MustGetUint32(kparams.FileOperation) == windows.FILE_CREATE
}

// RundownKey calculates the rundown event hash. The hash is
// used to determine if the rundown event was already processed.
func (e Kevent) RundownKey() uint64 {
	switch e.Type {
	case ktypes.ProcessRundown:
		b := make([]byte, 4)
		pid, _ := e.Kparams.GetPid()

		binary.LittleEndian.PutUint32(b, pid)

		return hashers.FnvUint64(b)
	case ktypes.ThreadRundown:
		b := make([]byte, 8)
		pid, _ := e.Kparams.GetPid()
		tid, _ := e.Kparams.GetTid()

		binary.LittleEndian.PutUint32(b, pid)
		binary.LittleEndian.PutUint32(b, tid)

		return hashers.FnvUint64(b)
	case ktypes.ImageRundown:
		pid, _ := e.Kparams.GetPid()
		mod, _ := e.Kparams.GetString(kparams.ImageFilename)
		b := make([]byte, 4+len(mod))

		binary.LittleEndian.PutUint32(b, pid)
		b = append(b, mod...)

		return hashers.FnvUint64(b)
	case ktypes.FileRundown:
		b := make([]byte, 8)
		fileObject, _ := e.Kparams.GetUint64(kparams.FileObject)
		binary.LittleEndian.PutUint64(b, fileObject)

		return hashers.FnvUint64(b)
	case ktypes.MapFileRundown:
		b := make([]byte, 12)
		fileKey, _ := e.Kparams.GetUint64(kparams.FileKey)
		binary.LittleEndian.PutUint32(b, e.PID)
		binary.LittleEndian.PutUint64(b, fileKey)

		return hashers.FnvUint64(b)
	case ktypes.RegKCBRundown:
		key, _ := e.Kparams.GetString(kparams.RegKeyName)
		b := make([]byte, 4+len(key))

		binary.LittleEndian.PutUint32(b, e.PID)
		b = append(b, key...)
		return hashers.FnvUint64(b)
	}
	return 0
}

// PartialKey computes the unique hash of the event
// that can be employed to determine if the event
// from the given process and source has been processed
// in the rule sequences.
func (e Kevent) PartialKey() uint64 {
	switch e.Type {
	case ktypes.WriteFile, ktypes.ReadFile:
		b := make([]byte, 12)
		object, _ := e.Kparams.GetUint64(kparams.FileObject)

		binary.LittleEndian.PutUint32(b, e.PID)
		binary.LittleEndian.PutUint64(b, object)

		return hashers.FnvUint64(b)
	case ktypes.MapFileRundown, ktypes.UnmapViewFile:
		b := make([]byte, 12)
		fileKey, _ := e.Kparams.GetUint64(kparams.FileKey)
		binary.LittleEndian.PutUint32(b, e.PID)
		binary.LittleEndian.PutUint64(b, fileKey)

		return hashers.FnvUint64(b)
	case ktypes.CreateFile:
		file, _ := e.Kparams.GetString(kparams.FileName)
		b := make([]byte, 4+len(file))

		binary.LittleEndian.PutUint32(b, e.PID)
		b = append(b, []byte(file)...)

		return hashers.FnvUint64(b)
	case ktypes.OpenProcess:
		b := make([]byte, 8)
		pid, _ := e.Kparams.GetUint32(kparams.ProcessID)
		access, _ := e.Kparams.GetUint32(kparams.DesiredAccess)

		binary.LittleEndian.PutUint32(b, e.PID)
		binary.LittleEndian.PutUint32(b, pid)
		binary.LittleEndian.PutUint32(b, access)
		return hashers.FnvUint64(b)
	case ktypes.OpenThread:
		b := make([]byte, 8)
		tid, _ := e.Kparams.GetUint32(kparams.ThreadID)
		access, _ := e.Kparams.GetUint32(kparams.DesiredAccess)

		binary.LittleEndian.PutUint32(b, e.PID)
		binary.LittleEndian.PutUint32(b, tid)
		binary.LittleEndian.PutUint32(b, access)
		return hashers.FnvUint64(b)
	case ktypes.AcceptTCPv4, ktypes.RecvTCPv4, ktypes.RecvUDPv4:
		b := make([]byte, 10)

		ip, _ := e.Kparams.GetIP(kparams.NetSIP)
		port, _ := e.Kparams.GetUint16(kparams.NetSport)

		binary.LittleEndian.PutUint32(b, e.PID)
		binary.LittleEndian.PutUint32(b, binary.BigEndian.Uint32(ip.To4()))
		binary.LittleEndian.PutUint16(b, port)
		return hashers.FnvUint64(b)
	case ktypes.AcceptTCPv6, ktypes.RecvTCPv6, ktypes.RecvUDPv6:
		b := make([]byte, 22)

		ip, _ := e.Kparams.GetIP(kparams.NetSIP)
		port, _ := e.Kparams.GetUint16(kparams.NetSport)

		binary.LittleEndian.PutUint32(b, e.PID)
		binary.LittleEndian.PutUint64(b, binary.BigEndian.Uint64(ip.To16()[0:8]))
		binary.LittleEndian.PutUint64(b, binary.BigEndian.Uint64(ip.To16()[8:16]))
		binary.LittleEndian.PutUint16(b, port)
		return hashers.FnvUint64(b)
	case ktypes.ConnectTCPv4, ktypes.SendTCPv4, ktypes.SendUDPv4:
		b := make([]byte, 10)

		ip, _ := e.Kparams.GetIP(kparams.NetDIP)
		port, _ := e.Kparams.GetUint16(kparams.NetDport)

		binary.LittleEndian.PutUint32(b, e.PID)
		binary.LittleEndian.PutUint32(b, binary.BigEndian.Uint32(ip.To4()))
		binary.LittleEndian.PutUint16(b, port)
		return hashers.FnvUint64(b)
	case ktypes.ConnectTCPv6, ktypes.SendTCPv6, ktypes.SendUDPv6:
		b := make([]byte, 22)

		ip, _ := e.Kparams.GetIP(kparams.NetDIP)
		port, _ := e.Kparams.GetUint16(kparams.NetDport)

		binary.LittleEndian.PutUint32(b, e.PID)
		binary.LittleEndian.PutUint64(b, binary.BigEndian.Uint64(ip.To16()[0:8]))
		binary.LittleEndian.PutUint64(b, binary.BigEndian.Uint64(ip.To16()[8:16]))
		binary.LittleEndian.PutUint16(b, port)
		return hashers.FnvUint64(b)
	case ktypes.RegOpenKey, ktypes.RegQueryKey, ktypes.RegQueryValue,
		ktypes.RegDeleteKey, ktypes.RegDeleteValue, ktypes.RegSetValue,
		ktypes.RegCloseKey:
		key, _ := e.Kparams.GetString(kparams.RegKeyName)
		b := make([]byte, 4+len(key))

		binary.LittleEndian.PutUint32(b, e.PID)
		b = append(b, key...)
		return hashers.FnvUint64(b)
	case ktypes.VirtualAlloc, ktypes.VirtualFree:
		b := make([]byte, 12)

		addr, _ := e.Kparams.GetUint64(kparams.MemBaseAddress)

		binary.LittleEndian.PutUint32(b, e.PID)
		binary.LittleEndian.PutUint64(b, addr)
		return hashers.FnvUint64(b)
	case ktypes.DuplicateHandle:
		b := make([]byte, 16)
		pid, _ := e.Kparams.GetUint32(kparams.ProcessID)
		object, _ := e.Kparams.GetUint64(kparams.HandleObject)

		binary.LittleEndian.PutUint32(b, e.PID)
		binary.LittleEndian.PutUint32(b, pid)
		binary.LittleEndian.PutUint64(b, object)
		return hashers.FnvUint64(b)
	case ktypes.QueryDNS, ktypes.ReplyDNS:
		n, _ := e.Kparams.GetString(kparams.DNSName)
		b := make([]byte, 4+len(n))

		binary.LittleEndian.PutUint32(b, e.PID)
		b = append(b, n...)
		return hashers.FnvUint64(b)
	}
	return 0
}

// BacklogKey represents the key used to index the events in the backlog store.
func (e *Kevent) BacklogKey() uint64 {
	switch e.Type {
	case ktypes.CreateHandle, ktypes.CloseHandle:
		return e.Kparams.MustGetUint64(kparams.HandleObject)
	}
	return 0
}

// CopyState adds parameters, tags, or process state from the provided event.
func (e *Kevent) CopyState(evt *Kevent) {
	switch evt.Type {
	case ktypes.CloseHandle:
		if evt.Kparams.Contains(kparams.ImageFilename) {
			e.Kparams.Append(kparams.ImageFilename, kparams.UnicodeString, evt.GetParamAsString(kparams.ImageFilename))
		}
		_ = e.Kparams.SetValue(kparams.HandleObjectName, evt.GetParamAsString(kparams.HandleObjectName))
	}
}

// Summary returns a brief summary of this event. Various important substrings
// in the summary text are highlighted by surrounding them inside <code> HTML tags.
func (e *Kevent) Summary() string {
	switch e.Type {
	case ktypes.CreateProcess:
		exe := e.Kparams.MustGetString(kparams.Exe)
		sid := e.GetParamAsString(kparams.Username)
		return printSummary(e, fmt.Sprintf("spawned <code>%s</code> process as <code>%s</code> user", exe, sid))
	case ktypes.TerminateProcess:
		exe := e.Kparams.MustGetString(kparams.Exe)
		sid := e.GetParamAsString(kparams.Username)
		return printSummary(e, fmt.Sprintf("terminated <code>%s</code> process as <code>%s</code> user", exe, sid))
	case ktypes.OpenProcess:
		access := e.GetParamAsString(kparams.DesiredAccess)
		exe, _ := e.Kparams.GetString(kparams.Exe)
		return printSummary(e, fmt.Sprintf("opened <code>%s</code> process object with <code>%s</code> access right(s)",
			exe, access))
	case ktypes.CreateThread:
		tid, _ := e.Kparams.GetTid()
		addr := e.GetParamAsString(kparams.StartAddr)
		return printSummary(e, fmt.Sprintf("spawned a new thread with <code>%d</code> id at <code>%s</code> address",
			tid, addr))
	case ktypes.TerminateThread:
		tid, _ := e.Kparams.GetTid()
		addr := e.GetParamAsString(kparams.StartAddr)
		return printSummary(e, fmt.Sprintf("terminated a thread with <code>%d</code> id at <code>%s</code> address",
			tid, addr))
	case ktypes.OpenThread:
		access := e.GetParamAsString(kparams.DesiredAccess)
		exe, _ := e.Kparams.GetString(kparams.Exe)
		return printSummary(e, fmt.Sprintf("opened <code>%s</code> process' thread object with <code>%s</code> access right(s)",
			exe, access))
	case ktypes.LoadImage:
		filename := e.GetParamAsString(kparams.FileName)
		return printSummary(e, fmt.Sprintf("loaded </code>%s</code> module", filename))
	case ktypes.UnloadImage:
		filename := e.GetParamAsString(kparams.FileName)
		return printSummary(e, fmt.Sprintf("unloaded </code>%s</code> module", filename))
	case ktypes.CreateFile:
		op := e.GetParamAsString(kparams.FileOperation)
		filename := e.GetParamAsString(kparams.FileName)
		return printSummary(e, fmt.Sprintf("%sed a file <code>%s</code>", strings.ToLower(op), filename))
	case ktypes.ReadFile:
		filename := e.GetParamAsString(kparams.FileName)
		size, _ := e.Kparams.GetUint32(kparams.FileIoSize)
		return printSummary(e, fmt.Sprintf("read <code>%d</code> bytes from <code>%s</code> file", size, filename))
	case ktypes.WriteFile:
		filename := e.GetParamAsString(kparams.FileName)
		size, _ := e.Kparams.GetUint32(kparams.FileIoSize)
		return printSummary(e, fmt.Sprintf("wrote <code>%d</code> bytes to <code>%s</code> file", size, filename))
	case ktypes.SetFileInformation:
		filename := e.GetParamAsString(kparams.FileName)
		class := e.GetParamAsString(kparams.FileInfoClass)
		return printSummary(e, fmt.Sprintf("set <code>%s</code> information class on <code>%s</code> file", class, filename))
	case ktypes.DeleteFile:
		filename := e.GetParamAsString(kparams.FileName)
		return printSummary(e, fmt.Sprintf("deleted <code>%s</code> file", filename))
	case ktypes.RenameFile:
		filename := e.GetParamAsString(kparams.FileName)
		return printSummary(e, fmt.Sprintf("renamed <code>%s</code> file", filename))
	case ktypes.CloseFile:
		filename := e.GetParamAsString(kparams.FileName)
		return printSummary(e, fmt.Sprintf("closed <code>%s</code> file", filename))
	case ktypes.EnumDirectory:
		filename := e.GetParamAsString(kparams.FileName)
		return printSummary(e, fmt.Sprintf("enumerated <code>%s</code> directory", filename))
	case ktypes.RegCreateKey:
		key := e.GetParamAsString(kparams.RegKeyName)
		return printSummary(e, fmt.Sprintf("created <code>%s</code> key", key))
	case ktypes.RegOpenKey:
		key := e.GetParamAsString(kparams.RegKeyName)
		return printSummary(e, fmt.Sprintf("opened <code>%s</code> key", key))
	case ktypes.RegDeleteKey:
		key := e.GetParamAsString(kparams.RegKeyName)
		return printSummary(e, fmt.Sprintf("deleted <code>%s</code> key", key))
	case ktypes.RegQueryKey:
		key := e.GetParamAsString(kparams.RegKeyName)
		return printSummary(e, fmt.Sprintf("queried <code>%s</code> key", key))
	case ktypes.RegSetValue:
		key := e.GetParamAsString(kparams.RegKeyName)
		val, err := e.Kparams.GetString(kparams.RegValue)
		if err != nil {
			return printSummary(e, fmt.Sprintf("set <code>%s</code> value", key))
		}
		return printSummary(e, fmt.Sprintf("set <code>%s</code> payload in <code>%s</code> value", val, key))
	case ktypes.RegDeleteValue:
		key := e.GetParamAsString(kparams.RegKeyName)
		return printSummary(e, fmt.Sprintf("deleted <code>%s</code> value", key))
	case ktypes.RegQueryValue:
		key := e.GetParamAsString(kparams.RegKeyName)
		return printSummary(e, fmt.Sprintf("queried <code>%s</code> value", key))
	case ktypes.AcceptTCPv4, ktypes.AcceptTCPv6:
		ip, _ := e.Kparams.GetIP(kparams.NetSIP)
		port, _ := e.Kparams.GetUint16(kparams.NetSport)
		return printSummary(e, fmt.Sprintf("accepted connection from <code>%v</code> and <code>%d</code> port", ip, port))
	case ktypes.ConnectTCPv4, ktypes.ConnectTCPv6:
		ip, _ := e.Kparams.GetIP(kparams.NetDIP)
		port, _ := e.Kparams.GetUint16(kparams.NetDport)
		return printSummary(e, fmt.Sprintf("connected to <code>%v</code> and <code>%d</code> port", ip, port))
	case ktypes.SendTCPv4, ktypes.SendTCPv6, ktypes.SendUDPv4, ktypes.SendUDPv6:
		ip, _ := e.Kparams.GetIP(kparams.NetDIP)
		port, _ := e.Kparams.GetUint16(kparams.NetDport)
		size, _ := e.Kparams.GetUint32(kparams.NetSize)
		return printSummary(e, fmt.Sprintf("sent <code>%d</code> bytes to <code>%v</code> and <code>%d</code> port",
			size, ip, port))
	case ktypes.RecvTCPv4, ktypes.RecvTCPv6, ktypes.RecvUDPv4, ktypes.RecvUDPv6:
		ip, _ := e.Kparams.GetIP(kparams.NetSIP)
		port, _ := e.Kparams.GetUint16(kparams.NetSport)
		size, _ := e.Kparams.GetUint32(kparams.NetSize)
		return printSummary(e, fmt.Sprintf("received <code>%d</code> bytes from <code>%v</code> and <code>%d</code> port",
			size, ip, port))
	case ktypes.CreateHandle:
		handleType := e.GetParamAsString(kparams.HandleObjectTypeID)
		handleName := e.GetParamAsString(kparams.HandleObjectName)
		return printSummary(e, fmt.Sprintf("created <code>%s</code> handle of <code>%s</code> type",
			handleName, handleType))
	case ktypes.CloseHandle:
		handleType := e.GetParamAsString(kparams.HandleObjectTypeID)
		handleName := e.GetParamAsString(kparams.HandleObjectName)
		return printSummary(e, fmt.Sprintf("closed <code>%s</code> handle of <code>%s</code> type",
			handleName, handleType))
	case ktypes.VirtualAlloc:
		addr := e.GetParamAsString(kparams.MemBaseAddress)
		return printSummary(e, fmt.Sprintf("allocated memory at <code>%s</code> address", addr))
	case ktypes.VirtualFree:
		addr := e.GetParamAsString(kparams.MemBaseAddress)
		return printSummary(e, fmt.Sprintf("released memory at <code>%s</code> address", addr))
	case ktypes.MapViewFile:
		sec := e.GetParamAsString(kparams.FileViewSectionType)
		return printSummary(e, fmt.Sprintf("mapped view of <code>%s</code> section", sec))
	case ktypes.UnmapViewFile:
		sec := e.GetParamAsString(kparams.FileViewSectionType)
		return printSummary(e, fmt.Sprintf("unmapped view of <code>%s</code> section", sec))
	case ktypes.DuplicateHandle:
		handleType := e.GetParamAsString(kparams.HandleObjectTypeID)
		return printSummary(e, fmt.Sprintf("duplicated <code>%s</code> handle", handleType))
	case ktypes.QueryDNS:
		dnsName := e.GetParamAsString(kparams.DNSName)
		return printSummary(e, fmt.Sprintf("sent <code>%s</code> DNS query", dnsName))
	case ktypes.ReplyDNS:
		dnsName := e.GetParamAsString(kparams.DNSName)
		return printSummary(e, fmt.Sprintf("received DNS response for <code>%s</code> query", dnsName))
	}
	return ""
}

func printSummary(e *Kevent, text string) string {
	ps := e.PS
	if ps != nil {
		return fmt.Sprintf("<code>%s</code> %s", ps.Name, text)
	}
	return fmt.Sprintf("process with <code>%d</code> id %s", e.PID, text)
}
