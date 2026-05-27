//go:build windows

package tmux

import (
	"fmt"
	"io"
)

func newTmuxPipe(target string) (io.ReadWriteCloser, error) {
	return nil, fmt.Errorf("tmux: web terminal not supported on Windows")
}

func NewTmuxPipe(target string) (io.ReadWriteCloser, error) {
	return newTmuxPipe(target)
}
