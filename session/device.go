// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode"
)

// What the trigger that started a session fires on (spec 022), as
// TriggerFiresOn records it.
const (
	TriggerSchedule = "schedule"
	TriggerEvents   = "events"
)

// MaxDeviceWorkdir bounds a device machine's folder, in bytes.
const MaxDeviceWorkdir = 4096

// environmentName is a Cella environment's name: a DNS label, which is
// what Cella names an environment by.
var environmentName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ErrInvalidDevice is a device machine that names no Cella environment by
// its name, or no absolute clean folder.
var ErrInvalidDevice = errors.New("session: invalid device machine")

// CheckDevice checks a device machine's environment and folder (spec
// 080): environment the name of the Cella environment the computer
// serves, a DNS label, and workdir an absolute, clean path of at most
// MaxDeviceWorkdir bytes with no control character. The computer decides
// whether the folder is one its owner granted; this checks only that it
// names a folder at all.
func CheckDevice(environment, workdir string) error {
	switch {
	case !environmentName.MatchString(environment):
		return fmt.Errorf("%w: environment %q is not a Cella environment's name, a DNS label of at most 63 lowercase letters, digits and hyphens", ErrInvalidDevice, environment)
	case workdir == "":
		return fmt.Errorf("%w: workdir is required, the folder on the computer the session works in", ErrInvalidDevice)
	case len(workdir) > MaxDeviceWorkdir:
		return fmt.Errorf("%w: workdir is %d bytes, at most %d", ErrInvalidDevice, len(workdir), MaxDeviceWorkdir)
	case strings.ContainsFunc(workdir, unicode.IsControl):
		return fmt.Errorf("%w: workdir holds a control character", ErrInvalidDevice)
	case !path.IsAbs(workdir) || path.Clean(workdir) != workdir:
		return fmt.Errorf("%w: workdir %q is not an absolute, clean path", ErrInvalidDevice, workdir)
	case workdir == "/":
		return fmt.Errorf("%w: workdir is the root directory, which no computer grants", ErrInvalidDevice)
	}
	return nil
}
