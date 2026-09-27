// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// shell is the shell a command runs under: Windows has no /bin/sh, so
// it is the POSIX shell findShell finds on this process's PATH.
func shell() (string, error) {
	return findShell(exec.LookPath, func(p string) bool {
		fi, err := os.Stat(p)
		return err == nil && fi.Mode().IsRegular()
	})
}

// group is the Job Object a command runs in. Windows has no process
// group a signal ends: the job holds the shell and every process the
// shell starts, and terminating the job ends them all.
type group struct{ job windows.Handle }

// prepare creates the command's job and makes cmd start suspended, so
// the shell starts no process before it is in the job, and in a
// process group of its own, so the console's Ctrl+C reaches topos and
// not the command, as a process group of its own does on Unix. A
// missing start directory is reported as fs.ErrNotExist, as a failed
// chdir is on Unix, where CreateProcess would answer ERROR_DIRECTORY.
func prepare(cmd *exec.Cmd) (*group, error) {
	if cmd.Dir != "" {
		if _, err := os.Stat(cmd.Dir); err != nil {
			return nil, fmt.Errorf("machine: the start directory: %w", err)
		}
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("machine: create the command's job object: %w", err)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP}
	return &group{job: job}, nil
}

// started puts the suspended shell in the job and resumes it. On an
// error the shell may still be suspended outside the job; the caller
// kills and reaps it.
func (g *group) started(cmd *exec.Cmd) error {
	pid := uint32(cmd.Process.Pid)
	p, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return fmt.Errorf("machine: open the command's process: %w", err)
	}
	aerr := windows.AssignProcessToJobObject(g.job, p)
	cerr := windows.CloseHandle(p)
	if aerr != nil {
		return errors.Join(fmt.Errorf("machine: put the command in its job object: %w", aerr), cerr)
	}
	if cerr != nil {
		return fmt.Errorf("machine: close the command's process: %w", cerr)
	}
	return resume(pid)
}

// resume resumes the threads of the suspended process pid, which
// CreateProcess started with one.
func resume(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("machine: list the command's threads: %w", err)
	}
	var errs []error
	resumed := 0
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err = windows.Thread32First(snap, &entry); err == nil; err = windows.Thread32Next(snap, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}
		t, oerr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if oerr != nil {
			errs = append(errs, fmt.Errorf("machine: open the command's thread %d: %w", entry.ThreadID, oerr))
			continue
		}
		if _, rerr := windows.ResumeThread(t); rerr != nil {
			errs = append(errs, fmt.Errorf("machine: resume the command's thread %d: %w", entry.ThreadID, rerr))
		} else {
			resumed++
		}
		if cerr := windows.CloseHandle(t); cerr != nil {
			errs = append(errs, fmt.Errorf("machine: close the command's thread %d: %w", entry.ThreadID, cerr))
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		errs = append(errs, fmt.Errorf("machine: list the command's threads: %w", err))
	}
	if cerr := windows.CloseHandle(snap); cerr != nil {
		errs = append(errs, fmt.Errorf("machine: close the thread list: %w", cerr))
	}
	if resumed == 0 && len(errs) == 0 {
		errs = append(errs, fmt.Errorf("machine: the command's process %d has no thread to resume", pid))
	}
	return errors.Join(errs...)
}

// terminate ends every process in the job. Windows has no signal that
// a job's processes could handle before they end, so a cancel ends
// them at once and KillGrace applies on Unix alone.
func (g *group) terminate() error { return g.kill() }

// kill ends every process in the job; a job whose processes have all
// exited is not an error.
func (g *group) kill() error {
	if err := windows.TerminateJobObject(g.job, 1); err != nil {
		return fmt.Errorf("machine: terminate the command's job object: %w", err)
	}
	return nil
}

// close closes the job's handle. A process still in the job keeps
// running, as a process group's members do on Unix once nobody signals
// them.
func (g *group) close() error {
	if err := windows.CloseHandle(g.job); err != nil {
		return fmt.Errorf("machine: close the command's job object: %w", err)
	}
	return nil
}
