//go:build windows

package claudecode

import "errors"

const tmuxSidecarPrefix = "cc-connect-"

var errTmuxNotSupported = errors.New("tmux sidecar not supported on windows")

func tmuxAvailable() bool { return false }

func createSidecarPane(_ string) (string, error) { return "", errTmuxNotSupported }

func destroySidecarPane(_ string) error { return errTmuxNotSupported }

func captureSidecarPane(_ string) (string, error) { return "", errTmuxNotSupported }

func reapStaleSidecars() {}
