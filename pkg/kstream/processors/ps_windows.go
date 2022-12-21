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

package processors

import (
	"github.com/rabbitstack/fibratus/pkg/util/cmdline"
	"time"

	"github.com/rabbitstack/fibratus/pkg/kevent"
	"github.com/rabbitstack/fibratus/pkg/kevent/kparams"
	"github.com/rabbitstack/fibratus/pkg/kevent/ktypes"
	"github.com/rabbitstack/fibratus/pkg/ps"
	"github.com/rabbitstack/fibratus/pkg/syscall/process"
	"github.com/rabbitstack/fibratus/pkg/yara"
)

type psProcessor struct {
	snap ps.Snapshotter
	yara yara.Scanner
}

// newPsProcessor creates a new event processor for process events.
func newPsProcessor(snap ps.Snapshotter, yara yara.Scanner) Processor {
	return psProcessor{snap: snap, yara: yara}
}

func (p psProcessor) ProcessEvent(e *kevent.Kevent) (*kevent.Kevent, bool, error) {
	switch e.Type {
	case ktypes.CreateProcess, ktypes.TerminateProcess, ktypes.ProcessRundown:
		if err := p.processEvent(e); err != nil {
			return e, false, err
		}
		if e.IsTerminateProcess() {
			return e, false, p.snap.Remove(e)
		}
		return e, false, p.snap.Write(e)
	case ktypes.CreateThread, ktypes.TerminateThread, ktypes.ThreadRundown:
		if !e.IsTerminateThread() {
			return e, false, p.snap.Write(e)
		}
		return e, false, p.snap.Remove(e)
	case ktypes.OpenProcess, ktypes.OpenThread:
		pid, err := e.Kparams.GetPid()
		if err != nil {
			return e, false, err
		}
		proc := p.snap.Find(pid)
		if proc != nil {
			e.AppendParam(kparams.Exe, kparams.FilePath, proc.Exe)
			e.AppendParam(kparams.ProcessName, kparams.AnsiString, proc.Name)
		}
		return e, false, nil
	}
	return e, true, nil
}

func (p psProcessor) processEvent(e *kevent.Kevent) error {
	cmndline := cmdline.New(e.GetParamAsString(kparams.Cmdline)).
		// get rid of leading/trailing quotes in the executable path
		CleanExe().
		// expand all variations of the SystemRoot environment variable
		ExpandSystemRoot().
		// some system processes are reported without the path in the command line,
		// but we can expand the path from the SystemRoot environment variable
		CompleteSysProc(e.GetParamAsString(kparams.ProcessName))

	// append executable path parameter
	e.AppendParam(kparams.Exe, kparams.FilePath, cmndline.Exeline())

	// set normalized command line
	_ = e.Kparams.SetValue(kparams.Cmdline, cmndline.String())

	if e.IsTerminateProcess() {
		return nil
	}

	// query process start time
	pid := e.Kparams.MustGetPid()
	started, err := getStartTime(pid)
	if err != nil {
		started = e.Timestamp
	}
	e.AppendParam(kparams.StartTime, kparams.Time, started)

	return nil
}

func (psProcessor) Name() ProcessorType { return Ps }
func (p psProcessor) Close()            {}

func getStartTime(pid uint32) (time.Time, error) {
	handle, err := process.Open(process.QueryLimitedInformation, false, pid)
	if err != nil {
		return time.Now(), err
	}
	defer handle.Close()
	started, err := process.GetStartTime(handle)
	if err != nil {
		return time.Now(), err
	}
	return started, nil
}
