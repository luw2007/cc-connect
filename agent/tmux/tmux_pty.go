//go:build !windows

package tmux

import (
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/creack/pty"
)

// tmuxPTY wraps a pty file for a `tmux attach-session` process.
type tmuxPTY struct {
	ptmx *os.File
	cmd  *exec.Cmd
}

func (p *tmuxPTY) Read(b []byte) (int, error)  { return p.ptmx.Read(b) }
func (p *tmuxPTY) Write(b []byte) (int, error) { return p.ptmx.Write(b) }
func (p *tmuxPTY) Close() error {
	err := p.ptmx.Close()
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
	return err
}

// newTmuxPipe starts `tmux attach-session -t <target>` in a pty and returns
// the pty fd as an io.ReadWriteCloser.
func newTmuxPipe(target string) (io.ReadWriteCloser, error) {
	cmd := exec.Command("tmux", "attach-session", "-t", target)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("tmux pty: %w", err)
	}
	return &tmuxPTY{ptmx: ptmx, cmd: cmd}, nil
}

// NewTmuxPipe is the exported wrapper around newTmuxPipe.
func NewTmuxPipe(target string) (io.ReadWriteCloser, error) {
	return newTmuxPipe(target)
}
