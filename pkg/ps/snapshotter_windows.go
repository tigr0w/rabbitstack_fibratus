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

package ps

import (
	"expvar"
	"github.com/rabbitstack/fibratus/pkg/zsyscall"
	"golang.org/x/sys/windows"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/rabbitstack/fibratus/pkg/config"
	"github.com/rabbitstack/fibratus/pkg/handle"
	htypes "github.com/rabbitstack/fibratus/pkg/handle/types"
	"github.com/rabbitstack/fibratus/pkg/kevent"
	"github.com/rabbitstack/fibratus/pkg/kevent/kparams"
	"github.com/rabbitstack/fibratus/pkg/kevent/ktypes"
	"github.com/rabbitstack/fibratus/pkg/pe"
	pstypes "github.com/rabbitstack/fibratus/pkg/ps/types"
	log "github.com/sirupsen/logrus"
)

var (
	// reapPeriod specifies the interval for triggering the housekeeping of dead processes
	reapPeriod = time.Minute * 2

	processLookupFailureCount = expvar.NewMap("process.lookup.failure.count")
	reapedProcesses           = expvar.NewInt("process.reaped")
	processCount              = expvar.NewInt("process.count")
	threadCount               = expvar.NewInt("process.thread.count")
	moduleCount               = expvar.NewInt("process.module.count")
	pebReadErrors             = expvar.NewInt("process.peb.read.errors")
)

type snapshotter struct {
	mu      sync.RWMutex
	procs   map[uint32]*pstypes.PS
	quit    chan struct{}
	config  *config.Config
	hsnap   handle.Snapshotter
	pe      pe.Reader
	capture bool
}

// NewSnapshotter returns a new instance of the process snapshotter.
func NewSnapshotter(hsnap handle.Snapshotter, config *config.Config) Snapshotter {
	s := &snapshotter{
		procs:  make(map[uint32]*pstypes.PS),
		quit:   make(chan struct{}),
		config: config,
		hsnap:  hsnap,
		pe:     pe.NewReader(config.PE),
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.hsnap.RegisterCreateCallback(s.onHandleCreated)
	s.hsnap.RegisterDestroyCallback(s.onHandleDestroyed)

	go s.gcDeadProcesses()

	return s
}

// NewSnapshotterFromKcap restores the snapshotter state from the kcap file.
func NewSnapshotterFromKcap(hsnap handle.Snapshotter, config *config.Config) Snapshotter {
	s := &snapshotter{
		procs:   make(map[uint32]*pstypes.PS),
		quit:    make(chan struct{}),
		config:  config,
		hsnap:   hsnap,
		pe:      pe.NewReader(config.PE),
		capture: true,
	}

	s.hsnap.RegisterCreateCallback(s.onHandleCreated)
	s.hsnap.RegisterDestroyCallback(s.onHandleDestroyed)

	return s
}

func (s *snapshotter) WriteFromKcap(e *kevent.Kevent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch e.Type {
	case ktypes.CreateProcess, ktypes.ProcessRundown:
		proc := e.PS
		if proc == nil {
			return nil
		}
		pid, err := e.Kparams.GetPid()
		if err != nil {
			return err
		}
		ppid, err := e.Kparams.GetPpid()
		if err != nil {
			return err
		}
		if e.Type == ktypes.ProcessRundown {
			// invalid process
			if proc.PID == proc.Ppid {
				return nil
			}
			s.procs[pid] = proc
		} else {
			ps := pstypes.NewProc(
				pid,
				ppid,
				e.GetParamAsString(kparams.ProcessName),
				e.GetParamAsString(kparams.Cmdline),
				e.GetParamAsString(kparams.Exe),
				e.GetParamAsString(kparams.UserSID),
				uint8(e.Kparams.MustGetUint32(kparams.SessionID)),
			)
			s.procs[pid] = ps
		}
		proc.Parent = s.procs[ppid]
	case ktypes.CreateThread, ktypes.ThreadRundown:
		pid, err := e.Kparams.GetPid()
		if err != nil {
			return err
		}
		threadCount.Add(1)
		if ps, ok := s.procs[pid]; ok {
			thread := pstypes.Thread{}
			thread.Tid, _ = e.Kparams.GetTid()
			thread.UstackBase, _ = e.Kparams.GetHex(kparams.UstackBase)
			thread.UstackLimit, _ = e.Kparams.GetHex(kparams.UstackLimit)
			thread.KstackBase, _ = e.Kparams.GetHex(kparams.KstackBase)
			thread.KstackLimit, _ = e.Kparams.GetHex(kparams.KstackLimit)
			thread.IOPrio, _ = e.Kparams.GetUint8(kparams.IOPrio)
			thread.BasePrio, _ = e.Kparams.GetUint8(kparams.BasePrio)
			thread.PagePrio, _ = e.Kparams.GetUint8(kparams.PagePrio)
			thread.Entrypoint, _ = e.Kparams.GetHex(kparams.StartAddr)
			ps.AddThread(thread)
		}
	case ktypes.LoadImage, ktypes.ImageRundown:
		pid, err := e.Kparams.GetPid()
		if err != nil {
			return err
		}
		moduleCount.Add(1)
		ps, ok := s.procs[pid]
		if !ok {
			return nil
		}
		module := pstypes.Module{}
		module.Size, _ = e.Kparams.GetUint32(kparams.ImageSize)
		module.Checksum, _ = e.Kparams.GetUint32(kparams.ImageCheckSum)
		module.Name, _ = e.Kparams.GetString(kparams.ImageFilename)
		module.BaseAddress, _ = e.Kparams.GetHex(kparams.ImageBase)
		module.DefaultBaseAddress, _ = e.Kparams.GetHex(kparams.ImageDefaultBase)
		ps.AddModule(module)
	}
	return nil
}

func (s *snapshotter) Write(e *kevent.Kevent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	processCount.Add(1)
	pid, err := e.Kparams.GetPid()
	if err != nil {
		return err
	}
	ppid, err := e.Kparams.GetPpid()
	if err != nil {
		return err
	}
	proc, err := s.initProc(pid, ppid, e)
	s.procs[pid] = proc
	proc.Parent = s.procs[ppid]
	// adjust the process which is generating the event. For
	// `CreateProcess` events the process context is scoped
	// to the parent/creator process. Otherwise, it is a regular
	// rundown event that doesn't require consulting the
	// process in the snapshot state
	if e.IsCreateProcess() {
		e.PS = s.procs[e.PID]
	} else {
		e.PS = proc
	}
	if err != nil {
		return err
	}
	return nil
}

func (s *snapshotter) AddThread(e *kevent.Kevent) error {
	pid, err := e.Kparams.GetPid()
	if err != nil {
		return err
	}
	threadCount.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	proc, ok := s.procs[pid]
	if !ok {
		return nil
	}
	thread := pstypes.Thread{}
	thread.Tid, _ = e.Kparams.GetTid()
	thread.UstackBase, _ = e.Kparams.GetHex(kparams.UstackBase)
	thread.UstackLimit, _ = e.Kparams.GetHex(kparams.UstackLimit)
	thread.KstackBase, _ = e.Kparams.GetHex(kparams.KstackBase)
	thread.KstackLimit, _ = e.Kparams.GetHex(kparams.KstackLimit)
	thread.IOPrio, _ = e.Kparams.GetUint8(kparams.IOPrio)
	thread.BasePrio, _ = e.Kparams.GetUint8(kparams.BasePrio)
	thread.PagePrio, _ = e.Kparams.GetUint8(kparams.PagePrio)
	thread.Entrypoint, _ = e.Kparams.GetHex(kparams.StartAddr)
	proc.AddThread(thread)
	return nil
}

func (s *snapshotter) AddModule(e *kevent.Kevent) error {
	pid, err := e.Kparams.GetPid()
	if err != nil {
		return err
	}
	moduleCount.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	proc, ok := s.procs[pid]
	if !ok {
		return nil
	}
	module := pstypes.Module{}
	module.Size, _ = e.Kparams.GetUint32(kparams.ImageSize)
	module.Checksum, _ = e.Kparams.GetUint32(kparams.ImageCheckSum)
	module.Name, _ = e.Kparams.GetString(kparams.ImageFilename)
	module.BaseAddress, _ = e.Kparams.GetHex(kparams.ImageBase)
	module.DefaultBaseAddress, _ = e.Kparams.GetHex(kparams.ImageDefaultBase)
	proc.AddModule(module)
	return nil
}

func (s *snapshotter) RemoveThread(pid uint32, tid uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	proc, ok := s.procs[pid]
	if !ok {
		return nil
	}
	proc.RemoveThread(tid)
	threadCount.Add(-1)
	return nil
}

func (s *snapshotter) RemoveModule(pid uint32, module string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	proc, ok := s.procs[pid]
	if !ok {
		return nil
	}
	proc.RemoveModule(module)
	moduleCount.Add(-1)
	return nil
}

func (s *snapshotter) Close() error {
	s.quit <- struct{}{}
	return nil
}

func (s *snapshotter) initProc(pid, ppid uint32, e *kevent.Kevent) (*pstypes.PS, error) {
	proc := pstypes.NewProc(
		pid,
		ppid,
		e.GetParamAsString(kparams.ProcessName),
		e.GetParamAsString(kparams.Cmdline),
		e.GetParamAsString(kparams.Exe),
		e.GetParamAsString(kparams.UserSID),
		uint8(e.Kparams.MustGetUint32(kparams.SessionID)),
	)
	// retrieve Portable Executable data
	var err error
	proc.PE, err = s.pe.Read(proc.Exe)
	if err != nil {
		return proc, err
	}

	// retrieve process handles
	proc.Handles, err = s.hsnap.FindHandles(pid)
	if err != nil {
		return proc, err
	}

	// try to read the PEB (Process Environment Block)
	// to access environment variables and the process
	// current working directory
	access := uint32(windows.PROCESS_QUERY_INFORMATION | windows.PROCESS_VM_READ)
	process, err := windows.OpenProcess(access, false, pid)
	if err != nil {
		return proc, nil
	}
	defer windows.CloseHandle(process)
	peb, err := ReadPEB(process)
	if err != nil {
		pebReadErrors.Add(1)
		return proc, err
	}
	proc.Envs = peb.GetEnvs()
	proc.Cwd = peb.GetCurrentWorkingDirectory()
	return proc, nil
}

// gcDeadProcesses periodically scans the map of the snapshot's processes and removes
// any terminated processes from it. This guarantees that any leftovers are cleaned-up
// in case we miss process' terminate events.
func (s *snapshotter) gcDeadProcesses() {
	tick := time.NewTicker(reapPeriod)
	for {
		select {
		case <-tick.C:
			s.mu.Lock()
			ss := len(s.procs)
			log.Debugf("scanning for dead processes on the snapshot of %d items", ss)

			for pid := range s.procs {
				proc, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
				if err != nil {
					continue
				}
				if !zsyscall.IsProcessRunning(proc) {
					delete(s.procs, pid)
				}
				_ = windows.CloseHandle(proc)
			}

			if ss > len(s.procs) {
				reaped := ss - len(s.procs)
				reapedProcesses.Add(int64(reaped))
				log.Debugf("%d dead process(es) reaped", reaped)
			}
			s.mu.Unlock()
		case <-s.quit:
			tick.Stop()
		}
	}
}

func (s *snapshotter) onHandleCreated(pid uint32, handle htypes.Handle) {
	s.mu.RLock()
	ps, ok := s.procs[pid]
	s.mu.RUnlock()
	if ok {
		s.mu.Lock()
		defer s.mu.Unlock()
		ps.AddHandle(handle)
		s.procs[pid] = ps
	}
}

func (s *snapshotter) onHandleDestroyed(pid uint32, rawHandle windows.Handle) {
	s.mu.RLock()
	ps, ok := s.procs[pid]
	s.mu.RUnlock()
	if ok {
		s.mu.Lock()
		defer s.mu.Unlock()
		ps.RemoveHandle(rawHandle)
		s.procs[pid] = ps
	}
}

func (s *snapshotter) Remove(e *kevent.Kevent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	pid, err := e.Kparams.GetPid()
	if err != nil {
		return err
	}
	delete(s.procs, pid)
	processCount.Add(-1)
	// reset parent if it died after spawning a process
	for procID, proc := range s.procs {
		if proc.Ppid == pid {
			s.procs[procID].Parent = nil
		}
	}
	return nil
}

func (s *snapshotter) Find(pid uint32) *pstypes.PS {
	s.mu.RLock()
	ps, ok := s.procs[pid]
	s.mu.RUnlock()
	if ok {
		return ps
	}
	if s.capture {
		return nil
	}
	processLookupFailureCount.Add(strconv.Itoa(int(pid)), 1)

	proc := &pstypes.PS{PID: pid, Ppid: zsyscall.InvalidProcessPid}
	access := uint32(windows.PROCESS_QUERY_INFORMATION | windows.PROCESS_VM_READ)
	process, err := windows.OpenProcess(access, false, pid)
	if err != nil {
		// the access to protected / system process can't be achieved
		// through `PROCESS_VM_READ` or `PROCESS_QUERY_INFORMATION` flags.
		// Try to acquire the process handle again but with restricted access
		// rights to obtain the full process's image name
		process, err = windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
		if err != nil {
			return proc
		}
		var size uint32 = windows.MAX_PATH
		n := make([]uint16, size)
		err := windows.QueryFullProcessImageName(process, 0, &n[0], &size)
		if err != nil {
			return proc
		}
		image := windows.UTF16ToString(n)
		proc.Exe = image
		proc.Name = filepath.Base(image)
	}
	defer windows.CloseHandle(process)

	// retrieve Portable Executable data
	proc.PE, err = s.pe.Read(proc.Exe)
	if err != nil {
		return proc
	}

	// consult process parent id
	info, err := zsyscall.QueryInformationProcess[windows.PROCESS_BASIC_INFORMATION](process, windows.ProcessBasicInformation)
	if err != nil {
		return proc
	}
	proc.Ppid = uint32(info.InheritedFromUniqueProcessId)

	// retrieve process handles
	proc.Handles, err = s.hsnap.FindHandles(pid)
	if err != nil {
		return proc
	}

	// read PEB
	peb, err := ReadPEB(process)
	if err != nil {
		pebReadErrors.Add(1)
		return proc
	}
	proc.Envs = peb.GetEnvs()
	proc.Cmdline = peb.GetCommandLine()
	proc.Cwd = peb.GetCurrentWorkingDirectory()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.procs[pid] = proc
	return proc
}

func (s *snapshotter) Size() uint32 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return uint32(len(s.procs))
}
