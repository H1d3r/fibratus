/*
* Copyright 2019-2020 by Nedim Sabic Sabic
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
package etw

import (
	"context"
	"fmt"
	"github.com/rabbitstack/fibratus/pkg/config"
	"github.com/rabbitstack/fibratus/pkg/event"
	"github.com/rabbitstack/fibratus/pkg/event/params"
	"github.com/rabbitstack/fibratus/pkg/handle"
	htypes "github.com/rabbitstack/fibratus/pkg/handle/types"
	"github.com/rabbitstack/fibratus/pkg/ps"
	pstypes "github.com/rabbitstack/fibratus/pkg/ps/types"
	"github.com/rabbitstack/fibratus/pkg/symbolize"
	"github.com/rabbitstack/fibratus/pkg/sys"
	"github.com/rabbitstack/fibratus/pkg/sys/etw"
	"github.com/rabbitstack/fibratus/pkg/util/va"
	yara "github.com/rabbitstack/fibratus/pkg/yara/config"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// MockListener receives the event and does nothing but indicating the event was processed.
type MockListener struct {
	gotEvent bool
}

func (l *MockListener) CanEnqueue() bool { return true }

func (l *MockListener) ProcessEvent(e *event.Event) (bool, error) {
	l.gotEvent = true
	return true, nil
}

func TestEventSourceStartTraces(t *testing.T) {
	psnap := new(ps.SnapshotterMock)
	psnap.On("Write", mock.Anything).Return(nil)
	psnap.On("AddThread", mock.Anything).Return(nil)
	psnap.On("AddModule", mock.Anything).Return(nil)
	psnap.On("AddMmap", mock.Anything).Return(nil)
	psnap.On("RemoveMmap", mock.Anything, mock.Anything).Return(nil)
	psnap.On("RemoveThread", mock.Anything, mock.Anything).Return(nil)
	psnap.On("RemoveModule", mock.Anything, mock.Anything).Return(nil)
	psnap.On("FindModule", mock.Anything).Return(false, nil)
	psnap.On("FindAndPut", mock.Anything).Return(&pstypes.PS{})
	psnap.On("Find", mock.Anything).Return(true, &pstypes.PS{})
	psnap.On("Remove", mock.Anything).Return(nil)

	hsnap := new(handle.SnapshotterMock)
	hsnap.On("FindByObject", mock.Anything).Return(htypes.Handle{}, false)
	hsnap.On("FindHandles", mock.Anything).Return([]htypes.Handle{}, nil)

	var tests = []struct {
		name         string
		cfg          *config.Config
		wantSessions int
		wantFlags    []etw.EventTraceFlags
	}{
		{"start kernel logger session",
			&config.Config{
				EventSource: config.EventSourceConfig{
					EnableThreadEvents: true,
					EnableNetEvents:    true,
					EnableFileIOEvents: true,
					EnableVAMapEvents:  true,
					BufferSize:         1024,
					FlushTimer:         time.Millisecond * 2300,
				},
				Filters: &config.Filters{},
			},
			2,
			[]etw.EventTraceFlags{0x6018203, 0},
		},
		{"start kernel and security telemetry logger sessions",
			&config.Config{
				EventSource: config.EventSourceConfig{
					EnableThreadEvents:   true,
					EnableNetEvents:      true,
					EnableFileIOEvents:   true,
					EnableVAMapEvents:    true,
					EnableHandleEvents:   true,
					EnableRegistryEvents: true,
					BufferSize:           1024,
					FlushTimer:           time.Millisecond * 2300,
					EnableAuditAPIEvents: true,
				},
				Filters: &config.Filters{},
			},
			2,
			[]etw.EventTraceFlags{0x6038203, 0x80000040},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.cfg.EventSource.Init()
			evs := NewEventSource(psnap, hsnap, tt.cfg, nil)
			require.NoError(t, evs.Open(tt.cfg))
			defer evs.Close()
			assert.Equal(t, tt.wantSessions, len(evs.(*EventSource).traces))

			for _, trace := range evs.(*EventSource).traces {
				require.True(t, trace.Handle().IsValid())
				require.NoError(t, etw.ControlTrace(0, trace.Name, trace.GUID, etw.Query))
				if tt.wantFlags != nil && trace.IsKernelTrace() {
					flags, err := etw.GetTraceSystemFlags(trace.Handle())
					require.NoError(t, err)
					// check enabled system event flags
					require.Equal(t, tt.wantFlags[0], flags[0])
					require.Equal(t, tt.wantFlags[1], flags[4])
				}
			}
		})
	}
}

func TestEventSourceEnableFlagsDynamically(t *testing.T) {
	psnap := new(ps.SnapshotterMock)
	psnap.On("Write", mock.Anything).Return(nil)
	psnap.On("AddThread", mock.Anything).Return(nil)
	psnap.On("AddModule", mock.Anything).Return(nil)
	psnap.On("AddMmap", mock.Anything).Return(nil)
	psnap.On("RemoveMmap", mock.Anything, mock.Anything).Return(nil)
	psnap.On("RemoveThread", mock.Anything, mock.Anything).Return(nil)
	psnap.On("RemoveModule", mock.Anything, mock.Anything).Return(nil)
	psnap.On("FindModule", mock.Anything).Return(false, nil)
	psnap.On("FindAndPut", mock.Anything).Return(&pstypes.PS{})
	psnap.On("Find", mock.Anything).Return(true, &pstypes.PS{})
	psnap.On("Remove", mock.Anything).Return(nil)

	hsnap := new(handle.SnapshotterMock)
	hsnap.On("FindByObject", mock.Anything).Return(htypes.Handle{}, false)
	hsnap.On("FindHandles", mock.Anything).Return([]htypes.Handle{}, nil)

	r := &config.RulesCompileResult{
		HasProcEvents:     true,
		HasImageEvents:    true,
		HasRegistryEvents: true,
		HasNetworkEvents:  true,
		HasFileEvents:     true,
		HasThreadEvents:   false,
		HasVAMapEvents:    true,
		HasAuditAPIEvents: true,
		UsedEvents: []event.Type{
			event.CreateProcess,
			event.LoadImage,
			event.RegCreateKey,
			event.RegSetValue,
			event.CreateFile,
			event.RenameFile,
			event.MapViewFile,
			event.OpenProcess,
			event.ConnectTCPv4,
		},
	}
	cfg := &config.Config{
		EventSource: config.EventSourceConfig{
			EnableThreadEvents:   true,
			EnableRegistryEvents: true,
			EnableImageEvents:    true,
			EnableFileIOEvents:   true,
			EnableAuditAPIEvents: true,
		},
		Filters: &config.Filters{},
	}

	cfg.EventSource.Init()
	evs := NewEventSource(psnap, hsnap, cfg, r)
	require.NoError(t, evs.Open(cfg))
	defer evs.Close()

	require.Len(t, evs.(*EventSource).traces, 2)

	flags := evs.(*EventSource).traces[1].enableFlagsDynamically(cfg.EventSource)

	require.True(t, flags&etw.FileIO != 0)
	require.True(t, flags&etw.Process != 0)
	// rules compile result doesn't have the thread event
	// and thread events are enabled in the config
	require.True(t, flags&etw.Thread == 0)
	require.True(t, flags&etw.ImageLoad != 0)
	require.True(t, flags&etw.Registry != 0)
	// rules compile result has the network event
	// but network I/O is disabled in the config
	require.True(t, flags&etw.NetTCPIP == 0)
	require.True(t, flags&etw.FileIO != 0)
	// rules compile result has MapViewFile event
	// but VAMap is disabled in the config
	require.True(t, flags&etw.VaMap == 0)

	require.False(t, cfg.EventSource.TestDropMask(event.UnloadImage))
	require.True(t, cfg.EventSource.TestDropMask(event.WriteFile))
	require.True(t, cfg.EventSource.TestDropMask(event.UnmapViewFile))
	require.False(t, cfg.EventSource.TestDropMask(event.OpenProcess))
}

func TestEventSourceEnableFlagsDynamicallyWithYaraEnabled(t *testing.T) {
	psnap := new(ps.SnapshotterMock)
	psnap.On("Write", mock.Anything).Return(nil)
	psnap.On("AddThread", mock.Anything).Return(nil)
	psnap.On("AddModule", mock.Anything).Return(nil)
	psnap.On("AddMmap", mock.Anything).Return(nil)
	psnap.On("RemoveMmap", mock.Anything, mock.Anything).Return(nil)
	psnap.On("RemoveThread", mock.Anything, mock.Anything).Return(nil)
	psnap.On("RemoveModule", mock.Anything, mock.Anything).Return(nil)
	psnap.On("FindModule", mock.Anything).Return(false, nil)
	psnap.On("FindAndPut", mock.Anything).Return(&pstypes.PS{})
	psnap.On("Find", mock.Anything).Return(true, &pstypes.PS{})
	psnap.On("Remove", mock.Anything).Return(nil)

	hsnap := new(handle.SnapshotterMock)
	hsnap.On("FindByObject", mock.Anything).Return(htypes.Handle{}, false)
	hsnap.On("FindHandles", mock.Anything).Return([]htypes.Handle{}, nil)

	r := &config.RulesCompileResult{
		HasProcEvents:     true,
		HasImageEvents:    true,
		HasRegistryEvents: true,
		HasNetworkEvents:  true,
		HasFileEvents:     false,
		HasThreadEvents:   false,
		HasAuditAPIEvents: true,
		UsedEvents: []event.Type{
			event.CreateProcess,
			event.LoadImage,
			event.RegCreateKey,
			event.RegSetValue,
			event.RenameFile,
			event.OpenProcess,
			event.ConnectTCPv4,
		},
	}
	cfg := &config.Config{
		EventSource: config.EventSourceConfig{
			EnableThreadEvents:   true,
			EnableRegistryEvents: true,
			EnableImageEvents:    true,
			EnableFileIOEvents:   true,
			EnableAuditAPIEvents: true,
			EnableVAMapEvents:    false,
			EnableMemEvents:      true,
		},
		Filters: &config.Filters{},
		Yara: yara.Config{
			Enabled:    true,
			SkipFiles:  false,
			SkipMmaps:  true,
			SkipAllocs: false,
		},
	}

	cfg.EventSource.Init()
	evs := NewEventSource(psnap, hsnap, cfg, r)
	require.NoError(t, evs.Open(cfg))
	defer evs.Close()

	require.Len(t, evs.(*EventSource).traces, 2)

	flags := evs.(*EventSource).traces[1].enableFlagsDynamically(cfg.EventSource)

	// rules compile result doesn't have file events
	// but Yara file scanning is enabled
	require.True(t, flags&etw.FileIO != 0)
	// VAMap events are not in the ruleset and VaMap is disabled
	require.False(t, flags&etw.VaMap != 0)
	// VirtualAlloc is not present in the ruleset, but Yara
	// alloc scanning is enabled
	require.True(t, flags&etw.VirtualAlloc != 0)

	require.False(t, cfg.EventSource.TestDropMask(event.CreateFile))
	require.True(t, cfg.EventSource.TestDropMask(event.MapViewFile))
	require.False(t, cfg.EventSource.TestDropMask(event.VirtualAlloc))
}

func TestEventSourceRundownEvents(t *testing.T) {
	psnap := new(ps.SnapshotterMock)
	psnap.On("Write", mock.Anything).Return(nil)
	psnap.On("AddThread", mock.Anything).Return(nil)
	psnap.On("AddModule", mock.Anything).Return(nil)
	psnap.On("AddMmap", mock.Anything).Return(nil)
	psnap.On("RemoveMmap", mock.Anything, mock.Anything).Return(nil)
	psnap.On("RemoveThread", mock.Anything, mock.Anything).Return(nil)
	psnap.On("RemoveModule", mock.Anything, mock.Anything).Return(nil)
	psnap.On("FindModule", mock.Anything).Return(false, nil)
	psnap.On("FindAndPut", mock.Anything).Return(&pstypes.PS{})
	psnap.On("Find", mock.Anything).Return(true, &pstypes.PS{})
	psnap.On("Remove", mock.Anything).Return(nil)

	hsnap := new(handle.SnapshotterMock)
	hsnap.On("FindByObject", mock.Anything).Return(htypes.Handle{}, false)
	hsnap.On("FindHandles", mock.Anything).Return([]htypes.Handle{}, nil)

	evsConfig := config.EventSourceConfig{
		EnableThreadEvents:   true,
		EnableImageEvents:    true,
		EnableFileIOEvents:   true,
		EnableNetEvents:      true,
		EnableRegistryEvents: true,
	}
	cfg := &config.Config{
		EventSource: evsConfig,
		CapFile:     "fake.cap", // simulate capture to receive state/rundown events
		Filters:     &config.Filters{},
	}

	cfg.EventSource.Init()
	evs := NewEventSource(psnap, hsnap, cfg, nil)

	l := &MockListener{}
	evs.RegisterEventListener(l)
	require.NoError(t, evs.Open(cfg))
	defer evs.Close()

	rundownsByType := map[event.Type]bool{
		event.ProcessRundown: false,
		event.ThreadRundown:  false,
		event.ImageRundown:   false,
		event.FileRundown:    false,
		event.RegKCBRundown:  false,
	}
	rundownsByHash := make(map[uint64]uint8)
	timeout := time.After(time.Minute)

	for {
		select {
		case e := <-evs.Events():
			if !e.IsRundown() {
				continue
			}
			rundownsByType[e.Type] = true
			rundownsByHash[e.RundownKey()]++
		case err := <-evs.Errors():
			t.Fatalf("FAIL: %v", err)
		case <-timeout:
			t.Logf("got %d rundown events", len(rundownsByHash))
			for key, count := range rundownsByHash {
				if count > 1 {
					t.Fatalf("got more than 1 rundown event for key %d", key)
				}
			}
			for typ, got := range rundownsByType {
				if !got {
					t.Fatalf("no rundown events for %s", typ.String())
				}
			}
			return
		}
	}
}

func TestEventSourceAllEvents(t *testing.T) {
	event.DropCurrentProc = false
	var viewBase uintptr
	var freeAddress uintptr
	var dupHandleID windows.Handle

	var tests = []*struct {
		name      string
		gen       func() error
		want      func(e *event.Event) bool
		completed bool
	}{
		{
			"spawn new process",
			func() error {
				var si windows.StartupInfo
				var pi windows.ProcessInformation
				argv, err := windows.UTF16PtrFromString(filepath.Join(os.Getenv("windir"), "notepad.exe"))
				if err != nil {
					return err
				}
				err = windows.CreateProcess(
					nil,
					argv,
					nil,
					nil,
					true,
					0,
					nil,
					nil,
					&si,
					&pi)
				if err != nil {
					return err
				}
				defer windows.TerminateProcess(pi.Process, 0)
				return nil
			},
			func(e *event.Event) bool {
				return e.IsCreateProcess() && e.CurrentPid() &&
					strings.EqualFold(e.GetParamAsString(params.ProcessName), "notepad.exe")
			},
			false,
		},
		{
			"terminate process",
			nil,
			func(e *event.Event) bool {
				return e.IsTerminateProcess() && strings.EqualFold(e.GetParamAsString(params.ProcessName), "notepad.exe")
			},
			false,
		},
		{
			"load image",
			nil,
			func(e *event.Event) bool {
				img := filepath.Join(os.Getenv("windir"), "System32", "notepad.exe")
				return e.IsLoadImage() && strings.EqualFold(img, e.GetParamAsString(params.ImagePath))
			},
			false,
		},
		{
			"create new file",
			func() error {
				f, err := os.CreateTemp(os.TempDir(), "fibratus-test")
				if err != nil {
					return err
				}
				defer f.Close()
				return nil
			},
			func(e *event.Event) bool {
				return e.CurrentPid() && e.Type == event.CreateFile &&
					strings.HasPrefix(filepath.Base(e.GetParamAsString(params.FilePath)), "fibratus-test") &&
					!e.IsOpenDisposition()
			},
			false,
		},
		{
			"connect socket",
			func() error {
				go func() {
					srv := http.Server{
						Addr: ":18090",
					}
					mux := http.NewServeMux()
					mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {})
					time.AfterFunc(time.Second*2, func() {
						//nolint:noctx
						resp, _ := http.Get("http://localhost:18090")
						if resp != nil {
							defer func() {
								_ = resp.Body.Close()
							}()
						}
						_ = srv.Shutdown(context.TODO())
					})
					_ = srv.ListenAndServe()
				}()
				return nil
			},
			func(e *event.Event) bool {
				return e.CurrentPid() && (e.Type == event.ConnectTCPv4 || e.Type == event.ConnectTCPv6)
			},
			false,
		},
		{
			"map view section",
			func() error {
				const SecImage = 0x01000000
				const SectionRead = 0x4

				var sec windows.Handle
				var offset uintptr
				var baseViewAddr uintptr
				dll := "_fixtures/yara-test.dll"
				f, err := os.Open(dll)
				if err != nil {
					return err
				}
				defer f.Close()
				stat, err := f.Stat()
				if err != nil {
					return err
				}
				size := stat.Size()
				if err := sys.NtCreateSection(
					&sec,
					SectionRead,
					0,
					uintptr(unsafe.Pointer(&size)),
					windows.PAGE_READONLY,
					SecImage,
					windows.Handle(f.Fd()),
				); err != nil {
					return fmt.Errorf("NtCreateSection: %v", err)
				}
				defer windows.Close(sec)
				err = sys.NtMapViewOfSection(
					sec,
					windows.CurrentProcess(),
					uintptr(unsafe.Pointer(&baseViewAddr)),
					0,
					0,
					uintptr(unsafe.Pointer(&offset)),
					uintptr(unsafe.Pointer(&size)),
					windows.SUB_CONTAINERS_ONLY_INHERIT,
					0,
					windows.PAGE_READONLY)
				if err != nil {
					return fmt.Errorf("NtMapViewOfSection: %v", err)
				}
				return nil
			},
			func(e *event.Event) bool {
				return e.CurrentPid() && e.Type == event.MapViewFile &&
					e.GetParamAsString(params.MemProtect) == "EXECUTE_READWRITE|READONLY" &&
					e.GetParamAsString(params.FileViewSectionType) == "IMAGE" &&
					strings.Contains(e.GetParamAsString(params.FilePath), "_fixtures\\yara-test.dll")
			},
			false,
		},
		{
			"unmap view section",
			func() error {
				const SecCommit = 0x8000000
				const SectionWrite = 0x2
				const SectionRead = 0x4
				const SectionExecute = 0x8
				const SectionRWX = SectionRead | SectionWrite | SectionExecute

				var sec windows.Handle
				var size uint64 = 1024
				var offset uintptr
				if err := sys.NtCreateSection(
					&sec,
					SectionRWX,
					0,
					uintptr(unsafe.Pointer(&size)),
					windows.PAGE_READONLY,
					SecCommit,
					0,
				); err != nil {
					return fmt.Errorf("NtCreateSection: %v", err)
				}
				defer windows.Close(sec)
				err := sys.NtMapViewOfSection(
					sec,
					windows.CurrentProcess(),
					uintptr(unsafe.Pointer(&viewBase)),
					0,
					0,
					uintptr(unsafe.Pointer(&offset)),
					uintptr(unsafe.Pointer(&size)),
					windows.SUB_CONTAINERS_ONLY_INHERIT,
					0,
					windows.PAGE_READONLY)
				if err != nil {
					return fmt.Errorf("NtMapViewOfSection: %v", err)
				}
				return sys.NtUnmapViewOfSection(windows.CurrentProcess(), viewBase)
			},
			func(e *event.Event) bool {
				return e.CurrentPid() && e.Type == event.UnmapViewFile &&
					e.GetParamAsString(params.MemProtect) == "READONLY" &&
					e.Params.MustGetUint64(params.FileViewBase) == uint64(viewBase)
			},
			false,
		},
		{
			"virtual alloc",
			func() error {
				base, err := windows.VirtualAlloc(0, 1024, windows.MEM_COMMIT|windows.MEM_RESERVE, windows.PAGE_EXECUTE_READWRITE)
				if err != nil {
					return err
				}
				defer func() {
					_ = windows.VirtualFree(base, 1024, windows.MEM_RELEASE)
				}()
				return nil
			},
			func(e *event.Event) bool {
				return e.CurrentPid() && e.Type == event.VirtualAlloc &&
					e.GetParamAsString(params.MemAllocType) == "COMMIT|RESERVE" && e.GetParamAsString(params.MemProtectMask) == "RWX"
			},
			false,
		},
		{
			"virtual free",
			func() error {
				var err error
				freeAddress, err = windows.VirtualAlloc(0, 1024, windows.MEM_COMMIT|windows.MEM_RESERVE, windows.PAGE_EXECUTE_READWRITE)
				if err != nil {
					return err
				}
				return windows.VirtualFree(freeAddress, 1024, windows.MEM_DECOMMIT)
			},
			func(e *event.Event) bool {
				return e.CurrentPid() && e.Type == event.VirtualFree &&
					e.GetParamAsString(params.MemAllocType) == "DECOMMIT" && e.Params.MustGetUint64(params.MemBaseAddress) == uint64(freeAddress)
			},
			false,
		},
		{
			"duplicate handle",
			func() error {
				var si windows.StartupInfo
				var pi windows.ProcessInformation
				argv, err := windows.UTF16PtrFromString(filepath.Join(os.Getenv("windir"), "notepad.exe"))
				if err != nil {
					return err
				}
				err = windows.CreateProcess(
					nil,
					argv,
					nil,
					nil,
					true,
					0,
					nil,
					nil,
					&si,
					&pi)
				if err != nil {
					return err
				}
				time.Sleep(time.Second)
				defer windows.TerminateProcess(pi.Process, 0)
				hs := handle.NewSnapshotter(&config.Config{EnumerateHandles: true}, nil)
				handles, err := hs.FindHandles(pi.ProcessId)
				if err != nil {
					return err
				}
				for _, h := range handles {
					if h.Type == handle.Key {
						dupHandleID = h.Num
						break
					}
				}
				assert.False(t, dupHandleID == 0)
				dup, err := handle.Duplicate(dupHandleID, pi.ProcessId, windows.KEY_READ)
				if err != nil {
					return err
				}
				defer windows.Close(dup)
				return nil
			},
			func(e *event.Event) bool {
				return e.CurrentPid() && e.Type == event.DuplicateHandle &&
					e.GetParamAsString(params.HandleObjectTypeID) == handle.Key &&
					windows.Handle(e.Params.MustGetUint32(params.HandleSourceID)) == dupHandleID
			},
			false,
		},
		{
			"query dns",
			func() error {
				_, err := net.LookupHost("dns.google")
				return err
			},
			func(e *event.Event) bool {
				return e.CurrentPid() && e.Type == event.QueryDNS && e.IsDNS() &&
					e.Type.Subcategory() == event.DNS &&
					e.GetParamAsString(params.DNSName) == "dns.google" &&
					e.GetParamAsString(params.DNSRR) == "A"
			},
			false,
		},
		{
			"reply dns",
			func() error {
				_, err := net.LookupHost("dns.google")
				return err
			},
			func(e *event.Event) bool {
				return e.CurrentPid() && e.Type == event.ReplyDNS && e.IsDNS() &&
					e.Type.Subcategory() == event.DNS &&
					e.GetParamAsString(params.DNSName) == "dns.google" &&
					e.GetParamAsString(params.DNSRR) == "AAAA" &&
					e.GetParamAsString(params.DNSRcode) == "NOERROR" &&
					e.GetParamAsString(params.DNSAnswers) != ""
			},
			false,
		},
		{
			"set thread context",
			func() error {
				return nil
			},
			func(e *event.Event) bool {
				return e.CurrentPid() && e.Type == event.SetThreadContext && e.GetParamAsString(params.NTStatus) == "Success"
			},
			false,
		},
	}

	psnap := new(ps.SnapshotterMock)
	psnap.On("Write", mock.Anything).Return(nil)
	psnap.On("AddThread", mock.Anything).Return(nil)
	psnap.On("AddModule", mock.Anything).Return(nil)
	psnap.On("AddMmap", mock.Anything).Return(nil)
	psnap.On("RemoveThread", mock.Anything, mock.Anything).Return(nil)
	psnap.On("RemoveModule", mock.Anything, mock.Anything).Return(nil)
	psnap.On("FindModule", mock.Anything).Return(false, nil)
	psnap.On("RemoveMmap", mock.Anything, mock.Anything).Return(nil)
	psnap.On("FindAndPut", mock.Anything).Return(&pstypes.PS{})
	psnap.On("Find", mock.Anything).Return(true, &pstypes.PS{})
	psnap.On("Remove", mock.Anything).Return(nil)

	hsnap := new(handle.SnapshotterMock)
	hsnap.On("FindByObject", mock.Anything).Return(htypes.Handle{}, false)
	hsnap.On("FindHandles", mock.Anything).Return([]htypes.Handle{}, nil)
	hsnap.On("Write", mock.Anything).Return(nil)
	hsnap.On("Remove", mock.Anything).Return(nil)

	evsConfig := config.EventSourceConfig{
		EnableThreadEvents:   true,
		EnableImageEvents:    true,
		EnableFileIOEvents:   true,
		EnableVAMapEvents:    true,
		EnableNetEvents:      true,
		EnableRegistryEvents: true,
		EnableMemEvents:      true,
		EnableHandleEvents:   true,
		EnableDNSEvents:      true,
		EnableAuditAPIEvents: true,
		StackEnrichment:      false,
	}

	evsConfig.Init()
	cfg := &config.Config{EventSource: evsConfig, Filters: &config.Filters{}}
	evs := NewEventSource(psnap, hsnap, cfg, nil)

	l := &MockListener{}
	evs.RegisterEventListener(l)
	require.NoError(t, evs.Open(cfg))
	defer evs.Close()

	time.Sleep(time.Second * 2)

	for _, tt := range tests {
		gen := tt.gen
		if gen != nil {
			require.NoError(t, gen(), tt.name)
		}
	}

	ntests := len(tests)
	timeout := time.After(time.Duration(ntests) * time.Minute)

	for {
		select {
		case e := <-evs.Events():
			for _, tt := range tests {
				if tt.completed {
					continue
				}
				pred := tt.want
				if pred(e) {
					t.Logf("PASS: %s", tt.name)
					tt.completed = true
					ntests--
				}
				if ntests == 0 {
					assert.True(t, l.gotEvent)
					return
				}
			}
		case err := <-evs.Errors():
			t.Fatalf("FAIL: %v", err)
		case <-timeout:
			for _, tt := range tests {
				if !tt.completed {
					t.Logf("FAIL: %s", tt.name)
				}
			}
			t.Fatal("FAIL: TestConsumerEvents")
		}
	}
}

func callstackContainsTestExe(callstack string) bool {
	return strings.Contains(callstack, "etw.test.exe")
}

// NoopPsSnapshotter is the process noop snapshotter  used in tests.
// The main motivation for a noop snapshotter is to reduce the pressure
// on internal mock calls which lead to excessive memory usage when
// the snapshotter Find method is invoked for each incoming event. This
// may create flaky tests.
type NoopPsSnapshotter struct{}

var fakeProc = &pstypes.PS{PID: 111111, Name: "fake.exe"}

func (s *NoopPsSnapshotter) Write(evt *event.Event) error                       { return nil }
func (s *NoopPsSnapshotter) Remove(evt *event.Event) error                      { return nil }
func (s *NoopPsSnapshotter) Find(pid uint32) (bool, *pstypes.PS)                { return true, fakeProc }
func (s *NoopPsSnapshotter) FindAndPut(pid uint32) *pstypes.PS                  { return fakeProc }
func (s *NoopPsSnapshotter) Put(ps *pstypes.PS)                                 {}
func (s *NoopPsSnapshotter) Size() uint32                                       { return 1 }
func (s *NoopPsSnapshotter) Close() error                                       { return nil }
func (s *NoopPsSnapshotter) GetSnapshot() []*pstypes.PS                         { return nil }
func (s *NoopPsSnapshotter) AddThread(evt *event.Event) error                   { return nil }
func (s *NoopPsSnapshotter) AddModule(evt *event.Event) error                   { return nil }
func (s *NoopPsSnapshotter) FindModule(addr va.Address) (bool, *pstypes.Module) { return false, nil }
func (s *NoopPsSnapshotter) RemoveThread(pid uint32, tid uint32) error          { return nil }
func (s *NoopPsSnapshotter) RemoveModule(pid uint32, addr va.Address) error     { return nil }
func (s *NoopPsSnapshotter) WriteFromCapture(evt *event.Event) error            { return nil }
func (s *NoopPsSnapshotter) AddMmap(evt *event.Event) error                     { return nil }
func (s *NoopPsSnapshotter) RemoveMmap(pid uint32, addr va.Address) error       { return nil }

func TestCallstackEnrichment(t *testing.T) {
	hsnap := new(handle.SnapshotterMock)
	hsnap.On("FindByObject", mock.Anything).Return(htypes.Handle{}, false)
	hsnap.On("FindHandles", mock.Anything).Return([]htypes.Handle{}, nil)
	hsnap.On("Write", mock.Anything).Return(nil)
	hsnap.On("Remove", mock.Anything).Return(nil)

	// exercise callstack enrichment with a noop
	// process snapshotter. This will make the
	// symbolizer to always fall back to Debug Help
	// API when resolving symbolic information
	nopsnap := new(NoopPsSnapshotter)
	log.Info("test callstack enrichment with noop ps snapshotter")
	testCallstackEnrichment(t, hsnap, nopsnap)

	// now use a real process snapshotter to
	// enrich the callstacks. This way, we
	// should only resort to Debug Help API
	// when the symbol is not found in PE
	// export directory or the module doesn't
	// exist in process state
	cfg := &config.Config{}
	psnap := ps.NewSnapshotter(hsnap, cfg)
	log.Info("test callstack enrichment with real ps snapshotter")
	testCallstackEnrichment(t, hsnap, psnap)
}

func testCallstackEnrichment(t *testing.T, hsnap handle.Snapshotter, psnap ps.Snapshotter) {
	event.DropCurrentProc = false

	var procHandle windows.Handle

	var tests = []*struct {
		name      string
		gen       func() error
		want      func(e *event.Event) bool
		completed bool
	}{
		{
			"create process callstack",
			func() error {
				var si windows.StartupInfo
				var pi windows.ProcessInformation
				argv, err := windows.UTF16PtrFromString(filepath.Join(os.Getenv("windir"), "notepad.exe"))
				if err != nil {
					return err
				}
				err = windows.CreateProcess(
					nil,
					argv,
					nil,
					nil,
					true,
					0,
					nil,
					nil,
					&si,
					&pi)
				if err != nil {
					return err
				}
				procHandle = pi.Process
				return nil
			},
			func(e *event.Event) bool {
				if e.IsCreateProcess() && e.CurrentPid() &&
					strings.EqualFold(e.GetParamAsString(params.ProcessName), "notepad.exe") {
					callstack := e.Callstack.String()
					log.Infof("create process event %s: %s", e.String(), callstack)
					return callstackContainsTestExe(callstack) &&
						strings.Contains(strings.ToLower(callstack), strings.ToLower("\\WINDOWS\\System32\\KERNELBASE.dll!CreateProcessW"))
				}
				return false
			},
			false,
		},
		{
			"load image callstack",
			nil,
			func(e *event.Event) bool {
				if e.IsLoadImage() && filepath.Ext(e.GetParamAsString(params.FilePath)) == ".dll" {
					callstack := e.Callstack.String()
					return strings.Contains(strings.ToLower(callstack), strings.ToLower("\\WINDOWS\\System32\\KERNELBASE.dll!LoadLibraryExW")) &&
						strings.Contains(strings.ToLower(callstack), strings.ToLower("\\WINDOWS\\system32\\ntoskrnl.exe!NtMapViewOfSection"))
				}
				return false
			},
			false,
		},
		{
			"create thread callstack",
			nil,
			func(e *event.Event) bool {
				if e.IsCreateThread() {
					callstack := e.Callstack.String()
					log.Infof("create thread event %s: %s", e.String(), callstack)
					return strings.Contains(strings.ToLower(callstack), strings.ToLower("\\WINDOWS\\SYSTEM32\\ntdll.dll!ZwCreateThreadEx")) ||
						strings.Contains(strings.ToLower(callstack), strings.ToLower("\\WINDOWS\\System32\\KERNEL32.DLL!CreateThread"))
				}
				return false
			},
			false,
		},
		{
			"terminate thread callstack",
			nil,
			func(e *event.Event) bool {
				if e.IsTerminateThread() {
					callstack := e.Callstack.String()
					log.Infof("terminate thread event %s: %s", e.String(), callstack)
					return strings.Contains(strings.ToLower(callstack), strings.ToLower("\\WINDOWS\\SYSTEM32\\ntdll.dll!ZwTerminateThread")) ||
						strings.Contains(strings.ToLower(callstack), strings.ToLower("\\WINDOWS\\SYSTEM32\\ntdll.dll!NtTerminateThread"))
				}
				return false
			},
			false,
		},
		{
			"create registry key callstack",
			func() error {
				var h syscall.Handle
				var d uint32
				path := "Volatile Environment\\CallstackTest"
				err := regCreateKeyEx(syscall.Handle(registry.CURRENT_USER), windows.StringToUTF16Ptr(path),
					0, nil, 1, registry.ALL_ACCESS, nil, &h, &d)
				if err != nil {
					return err
				}
				defer registry.DeleteKey(registry.CURRENT_USER, path)
				return nil
			},
			func(e *event.Event) bool {
				if e.CurrentPid() && e.Type == event.RegCreateKey && e.GetParamAsString(params.RegPath) == "HKEY_CURRENT_USER\\Volatile Environment\\CallstackTest" {
					callstack := e.Callstack.String()
					log.Infof("create key event %s: %s", e.String(), callstack)
					return callstackContainsTestExe(callstack) &&
						(strings.Contains(strings.ToLower(callstack), strings.ToLower("\\WINDOWS\\SYSTEM32\\ntdll.dll!NtCreateKey")) ||
							strings.Contains(strings.ToLower(callstack), strings.ToLower("\\WINDOWS\\SYSTEM32\\ntdll.dll!ZwCreateKey"))) &&
						strings.Contains(strings.ToLower(callstack), strings.ToLower("\\WINDOWS\\System32\\KERNELBASE.dll!RegCreateKeyExW"))
				}
				return false
			},
			false,
		},
		{
			"delete registry key callstack",
			nil,
			func(e *event.Event) bool {
				if e.CurrentPid() && e.Type == event.RegDeleteKey {
					callstack := e.Callstack.String()
					log.Infof("delete key event %s: %s", e.String(), callstack)
					return callstackContainsTestExe(callstack) &&
						strings.Contains(strings.ToLower(callstack), strings.ToLower("\\WINDOWS\\System32\\advapi32.dll!RegDeleteKeyW"))
				}
				return false
			},
			false,
		},
		{
			"set registry value callstack",
			func() error {
				key, err := registry.OpenKey(registry.CURRENT_USER, "Volatile Environment", registry.SET_VALUE)
				if err != nil {
					return err
				}
				defer key.Close()
				defer key.DeleteValue("FibratusCallstack")
				return key.SetStringValue("FibratusCallstack", "Callstack")
			},
			func(e *event.Event) bool {
				if e.CurrentPid() && e.Type == event.RegSetValue && strings.HasSuffix(e.GetParamAsString(params.RegPath), "FibratusCallstack") {
					callstack := e.Callstack.String()
					log.Infof("set value event %s: %s", e.String(), callstack)
					return callstackContainsTestExe(callstack) &&
						strings.Contains(strings.ToLower(callstack), strings.ToLower("\\WINDOWS\\System32\\KERNELBASE.dll!RegSetValueExW")) &&
						(strings.Contains(strings.ToLower(callstack), strings.ToLower("\\WINDOWS\\SYSTEM32\\ntdll.dll!ZwSetValueKey")) ||
							strings.Contains(strings.ToLower(callstack), strings.ToLower("\\WINDOWS\\SYSTEM32\\ntdll.dll!NtSetValueKey")))
				}
				return false
			},
			false,
		},
		{
			"delete registry value callstack",
			nil,
			func(e *event.Event) bool {
				if e.CurrentPid() && e.Type == event.RegDeleteValue {
					callstack := e.Callstack.String()
					log.Infof("delete value event %s: %s", e.String(), callstack)
					return callstackContainsTestExe(callstack) &&
						strings.Contains(strings.ToLower(callstack), strings.ToLower("\\WINDOWS\\System32\\KERNELBASE.dll!RegDeleteValueW"))
				}
				return false
			},
			false,
		},
		{
			"set thread context callstack",
			nil,
			func(e *event.Event) bool {
				return e.Type == event.SetThreadContext &&
					callstackContainsTestExe(e.Callstack.String()) &&
					strings.Contains(strings.ToLower(e.Callstack.String()), strings.ToLower("\\WINDOWS\\System32\\KERNELBASE.dll!SetThreadContext"))
			},
			false,
		},
		{
			"create file callstack",
			func() error {
				f, err := os.CreateTemp(os.TempDir(), "fibratus-callstack")
				if err != nil {
					return err
				}
				defer f.Close()
				return nil
			},
			func(e *event.Event) bool {
				if e.CurrentPid() && e.Type == event.CreateFile &&
					strings.HasPrefix(filepath.Base(e.GetParamAsString(params.FilePath)), "fibratus-callstack") &&
					!e.IsOpenDisposition() {
					callstack := e.Callstack.String()
					log.Infof("create file event %s: %s", e.String(), callstack)
					return callstackContainsTestExe(callstack) &&
						strings.Contains(strings.ToLower(callstack), strings.ToLower("\\WINDOWS\\System32\\KERNELBASE.dll!CreateFileW"))
				}
				return false
			},
			false,
		},
		{
			"create file transacted callstack",
			func() error {
				n, _ := windows.UTF16PtrFromString(filepath.Join(os.TempDir(), "fibratus-file-transacted"))
				t, err := createTransaction()
				if err != nil {
					return err
				}
				defer windows.Close(t)
				h, err := createFileTransacted(n, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_WRITE, nil, 1, 0, 0, t, 0)
				if err != nil {
					return err
				}
				defer windows.Close(h)
				return nil
			},
			func(e *event.Event) bool {
				if e.CurrentPid() && e.Type == event.CreateFile &&
					strings.HasPrefix(filepath.Base(e.GetParamAsString(params.FilePath)), "fibratus-file-transacted") &&
					!e.IsOpenDisposition() {
					callstack := e.Callstack.String()
					log.Infof("create transacted file event %s: %s", e.String(), callstack)
					return callstackContainsTestExe(callstack) &&
						strings.Contains(strings.ToLower(callstack), strings.ToLower("\\WINDOWS\\System32\\KERNEL32.dll!CreateFileTransactedW"))
				}
				return false
			},
			false,
		},
		{
			"virtual alloc callstack",
			func() error {
				_, err := windows.VirtualAlloc(0, 1024, windows.MEM_COMMIT|windows.MEM_RESERVE, windows.PAGE_EXECUTE_READ)
				if err != nil {
					return err
				}
				return nil
			},
			func(e *event.Event) bool {
				if e.CurrentPid() && e.Type == event.VirtualAlloc &&
					e.GetParamAsString(params.MemAllocType) == "COMMIT|RESERVE" {
					callstack := e.Callstack.String()
					log.Infof("virtual alloc event %s: %s", e.String(), callstack)
					return callstackContainsTestExe(callstack) &&
						strings.Contains(strings.ToLower(callstack), strings.ToLower("\\Windows\\System32\\KernelBase.dll!VirtualAlloc"))
				}
				return false
			},
			false,
		},
		//{
		//	"copy file callstack",
		//	func() error {
		//		// TODO: Investigate CopyFile API call not working in Github CI
		//		f, err := os.CreateTemp(os.TempDir(), "fibratus-copy-file")
		//		if err != nil {
		//			return err
		//		}
		//		f.Close()
		//		from, _ := windows.UTF16PtrFromString(f.Name())
		//		to, _ := windows.UTF16PtrFromString(filepath.Join(os.TempDir(), "copied-file"))
		//		return copyFile(from, to)
		//	},
		//	func(e *event.Event) bool {
		//		if e.CurrentPid() && e.Type == event.CreateFile &&
		//			strings.HasPrefix(filepath.Base(e.GetParamAsString(params.FileName)), "copied-file") &&
		//			!e.IsOpenDisposition() {
		//			callstack := e.Callstack.String()
		//			log.Infof("copy file event %s: %s", e.String(), callstack)
		//			return callstackContainsTestExe(callstack) &&
		//				strings.Contains(strings.ToLower(callstack), strings.ToLower("\\WINDOWS\\System32\\KERNELBASE.dll!CopyFileExW"))
		//		}
		//		return false
		//	},
		//	false,
		//},
		{
			"delete file callstack",
			func() error {
				f, err := os.CreateTemp(os.TempDir(), "fibratus-delete")
				if err != nil {
					return err
				}
				f.Close()
				return os.Remove(f.Name())
			},
			func(e *event.Event) bool {
				if e.CurrentPid() && e.Type == event.DeleteFile &&
					strings.HasPrefix(filepath.Base(e.GetParamAsString(params.FilePath)), "fibratus-delete") {
					callstack := e.Callstack.String()
					log.Infof("delete file event %s: %s", e.String(), callstack)
					return callstackContainsTestExe(callstack) &&
						strings.Contains(strings.ToLower(callstack), strings.ToLower("\\WINDOWS\\System32\\KERNELBASE.dll!DeleteFileW"))
				}
				return false
			},
			false,
		},
		{
			"rename file callstack",
			func() error {
				f, err := os.CreateTemp(os.TempDir(), "fibratus-rename")
				if err != nil {
					return err
				}
				f.Close()
				if err := os.Rename(f.Name(), filepath.Join(os.TempDir(), "fibratus-ren")); err != nil {
					return err
				}
				return os.Remove(filepath.Join(os.TempDir(), "fibratus-ren"))
			},
			func(e *event.Event) bool {
				if e.CurrentPid() && e.Type == event.RenameFile &&
					strings.HasPrefix(filepath.Base(e.GetParamAsString(params.FilePath)), "fibratus-rename") {
					callstack := e.Callstack.String()
					log.Infof("rename file event %s: %s", e.String(), callstack)
					return callstackContainsTestExe(callstack) &&
						strings.Contains(strings.ToLower(callstack), strings.ToLower("\\WINDOWS\\System32\\KERNELBASE.dll!MoveFileExW"))
				}
				return false
			},
			false,
		},
		{
			"open process callstack",
			func() error {
				_, err := windows.OpenProcess(windows.PROCESS_VM_READ, false, uint32(os.Getpid()))
				return err
			},
			func(e *event.Event) bool {
				if e.CurrentPid() && e.Type == event.OpenProcess {
					callstack := e.Callstack.String()
					log.Infof("open process event %s: %s", e.String(), callstack)
					return callstackContainsTestExe(callstack) &&
						strings.Contains(strings.ToLower(callstack), strings.ToLower("\\WINDOWS\\System32\\KERNELBASE.dll!OpenProcess"))
				}
				return false
			},
			false,
		},
		{
			"open thread callstack",
			func() error {
				_, err := windows.OpenThread(windows.THREAD_IMPERSONATE, false, windows.GetCurrentThreadId())
				return err
			},
			func(e *event.Event) bool {
				if e.CurrentPid() && e.Type == event.OpenThread {
					callstack := e.Callstack.String()
					log.Infof("open thread event %s: %s", e.String(), callstack)
					return callstackContainsTestExe(callstack) &&
						strings.Contains(strings.ToLower(callstack), strings.ToLower("\\WINDOWS\\System32\\KERNELBASE.dll!OpenThread"))
				}
				return false
			},
			false,
		},
	}

	evsConfig := config.EventSourceConfig{
		EnableThreadEvents:   true,
		EnableImageEvents:    true,
		EnableFileIOEvents:   true,
		EnableRegistryEvents: true,
		EnableMemEvents:      true,
		EnableAuditAPIEvents: true,
		StackEnrichment:      true,
		BufferSize:           1024,
		MinBuffers:           uint32(runtime.NumCPU() * 2),
		MaxBuffers:           uint32((runtime.NumCPU() * 2) + 20),
		ExcludedImages:       []string{"System"},
		ExcludedEvents:       []string{"WriteFile", "ReadFile", "RegOpenKey", "RegCloseKey", "CloseFile"},
		FlushTimer:           1,
	}

	evsConfig.Init()

	cfg := &config.Config{
		EventSource:              evsConfig,
		Filters:                  &config.Filters{},
		SymbolizeKernelAddresses: true,
	}

	evs := NewEventSource(psnap, hsnap, cfg, nil)
	symbolizer := symbolize.NewSymbolizer(symbolize.NewDebugHelpResolver(cfg), psnap, cfg, true)
	defer symbolizer.Close()
	evs.RegisterEventListener(symbolizer)
	require.NoError(t, evs.Open(cfg))
	defer evs.Close()

	time.Sleep(time.Second * 5)

	log.Infof("current process id is [%d]", os.Getpid())

	for _, tt := range tests {
		gen := tt.gen
		log.Infof("executing [%s] test generator", tt.name)
		if gen != nil {
			require.NoError(t, gen(), tt.name)
		}
	}

	ntests := len(tests)
	timeout := time.After(time.Duration(ntests) * time.Minute)
	defer windows.TerminateProcess(procHandle, 0)

	for {
		select {
		case e := <-evs.Events():
			for _, tt := range tests {
				if tt.completed {
					continue
				}
				pred := tt.want
				if pred(e) {
					t.Logf("PASS: %s", tt.name)
					tt.completed = true
					ntests--
				}
				if ntests == 0 {
					return
				}
			}
		case err := <-evs.Errors():
			t.Fatalf("FAIL: %v", err)
		case <-timeout:
			for _, tt := range tests {
				if !tt.completed {
					t.Logf("FAIL: %s", tt.name)
				}
			}
			t.Fatal("FAIL: TestCallstackEnrichment")
		}
	}
}

var (
	modadvapi32 = windows.NewLazySystemDLL("advapi32.dll")
	kernel32    = windows.NewLazySystemDLL("kernel32.dll")
	ktmW32      = windows.NewLazySystemDLL("KtmW32.dll")

	procRegCreateKeyExW = modadvapi32.NewProc("RegCreateKeyExW")
	//procCopyFile             = kernel32.NewProc("CopyFileW")
	procCreateTransaction    = ktmW32.NewProc("CreateTransaction")
	procCreateFileTransacted = kernel32.NewProc("CreateFileTransactedW")
)

func regCreateKeyEx(key syscall.Handle, subkey *uint16, reserved uint32, class *uint16, options uint32, desired uint32, sa *syscall.SecurityAttributes, result *syscall.Handle, disposition *uint32) (regerrno error) {
	r0, _, _ := syscall.SyscallN(procRegCreateKeyExW.Addr(), uintptr(key), uintptr(unsafe.Pointer(subkey)), uintptr(reserved), uintptr(unsafe.Pointer(class)), uintptr(options), uintptr(desired), uintptr(unsafe.Pointer(sa)), uintptr(unsafe.Pointer(result)), uintptr(unsafe.Pointer(disposition)))
	if r0 != 0 {
		regerrno = syscall.Errno(r0)
	}
	return
}

func createFileTransacted(name *uint16, access uint32, mode uint32, sa *windows.SecurityAttributes, createmode uint32, attrs uint32, templatefile windows.Handle, trans windows.Handle, ver uint8) (handle windows.Handle, err error) {
	r0, _, e1 := syscall.SyscallN(procCreateFileTransacted.Addr(), uintptr(unsafe.Pointer(name)), uintptr(access), uintptr(mode), uintptr(unsafe.Pointer(sa)), uintptr(createmode), uintptr(attrs), uintptr(templatefile), uintptr(trans), uintptr(ver), 0, 0, 0)
	handle = windows.Handle(r0)
	if handle == windows.InvalidHandle {
		err = e1
	}
	return
}

//func copyFile(from *uint16, to *uint16) (regerrno error) {
//	r0, _, _ := procCopyFile.Call(uintptr(unsafe.Pointer(from)), uintptr(unsafe.Pointer(to)), uintptr(1))
//	if r0 != 0 {
//		regerrno = syscall.Errno(r0)
//	}
//	return
//}

func createTransaction() (handle windows.Handle, err error) {
	r0, _, e1 := syscall.SyscallN(procCreateTransaction.Addr(), 0, 0, 0, 0, 0, 0, 0, 0, 0)
	handle = windows.Handle(r0)
	if handle == windows.InvalidHandle {
		err = e1
	}
	return
}
