/*
Copyright 2015 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package srv

import (
	"io"
	//"net"
	"crypto/subtle"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"golang.org/x/crypto/ssh"
	//"golang.org/x/crypto/ssh/agent"

	"github.com/gravitational/teleport/lib/services"
	rsession "github.com/gravitational/teleport/lib/session"
	"github.com/gravitational/teleport/lib/sshutils"

	"github.com/gravitational/trace"
	"github.com/kr/pty"
	"github.com/moby/moby/pkg/term"
	log "github.com/sirupsen/logrus"
)

// Terminal defines an interface of handy functions for managing a (local or
// remote) PTY, such as resizing windows, executing commands with a PTY, and
// cleaning up.
type Terminal interface {
	// AddParty adds another participant to this terminal. We will keep the
	// Terminal open until all participants have left.
	AddParty(delta int)

	Run() error
	Wait() (*ExecResult, error)
	Kill() error

	PTY() io.ReadWriter
	TTY() *os.File

	Close() error

	GetWinSize() (*term.Winsize, error)
	SetWinSize(params rsession.TerminalParams) error
	GetTerminalParams() rsession.TerminalParams
}

// terminal is a local PTY created by Teleport nodes.
type terminal struct {
	wg sync.WaitGroup
	mu sync.Mutex

	cmd *exec.Cmd
	ctx *ServerContext

	pty *os.File
	tty *os.File
	//err    error
	//done   bool
	params rsession.TerminalParams
}

// NewLocalTerminal creates and returns a local PTY.
func NewLocalTerminal(ctx *ServerContext) (*terminal, error) {
	pty, tty, err := pty.Open()
	if err != nil {
		log.Warnf("could not start pty (%s)", err)
		return nil, err
	}
	return &terminal{
		ctx: ctx,
		pty: pty,
		tty: tty,
		//err: err,
	}, nil
}

func (t *terminal) AddParty(delta int) {
	t.wg.Add(delta)
}

func (t *terminal) Run() error {
	defer t.closeTTY()

	cmd, err := prepInteractiveCommand(t.ctx)
	if err != nil {
		return trace.Wrap(err)
	}
	t.cmd = cmd

	cmd.Stdout = t.tty
	cmd.Stdin = t.tty
	cmd.Stderr = t.tty
	cmd.SysProcAttr.Setctty = true
	cmd.SysProcAttr.Setsid = true

	err = cmd.Start()
	if err != nil {
		return trace.Wrap(err)
	}

	return nil
}

func (t *terminal) Wait() (*ExecResult, error) {
	err := t.cmd.Wait()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			status := exitErr.Sys().(syscall.WaitStatus)
			return &ExecResult{Code: status.ExitStatus(), Command: t.cmd.Path}, nil
		}
		return nil, err
	}

	status, ok := t.cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok {
		return nil, trace.Errorf("unknown exit status: %T(%v)", t.cmd.ProcessState.Sys(), t.cmd.ProcessState.Sys())
	}

	return &ExecResult{
		Code:    status.ExitStatus(),
		Command: t.cmd.Path,
	}, nil
}

func (t *terminal) Kill() error {
	if t.cmd.Process != nil {
		if err := t.cmd.Process.Kill(); err != nil {
			if err.Error() != "os: process already finished" {
				//log.Error(trace.DebugReport(err))
				return trace.Wrap(err)
			}
		}
	}

	return nil
}

func (t *terminal) PTY() io.ReadWriter {
	return t.pty
}

func (t *terminal) TTY() *os.File {
	return t.tty
}

func (t *terminal) GetWinSize() (*term.Winsize, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pty == nil {
		return nil, trace.NotFound("no pty")
	}
	ws, err := term.GetWinsize(t.pty.Fd())
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return ws, nil
}

func (t *terminal) SetWinSize(params rsession.TerminalParams) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pty == nil {
		return trace.NotFound("no pty")
	}
	if err := term.SetWinsize(t.pty.Fd(), params.Winsize()); err != nil {
		return trace.Wrap(err)
	}
	t.params = params
	return nil
}

// getTerminalParams is a fast call to get cached terminal parameters
// and avoid extra system call
func (t *terminal) GetTerminalParams() rsession.TerminalParams {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.params
}

func (t *terminal) closeTTY() {
	if err := t.tty.Close(); err != nil {
		log.Warnf("failed to close TTY: %v", err)
	}
	t.tty = nil
}

func (t *terminal) Close() error {
	var err error
	// note, pty is closed in the copying goroutine,
	// not here to avoid data races
	if t.tty != nil {
		if e := t.tty.Close(); e != nil {
			err = e
		}
	}
	go t.closePTY()
	return trace.Wrap(err)
}

func (t *terminal) closePTY() {
	t.mu.Lock()
	defer t.mu.Unlock()
	defer log.Debugf("PTY is closed")

	// wait until all copying is over (all participants have left)
	t.wg.Wait()

	t.pty.Close()
	t.pty = nil
}

func ParsePTYReq(req *ssh.Request) (*sshutils.PTYReqParams, error) {
	var r sshutils.PTYReqParams
	if err := ssh.Unmarshal(req.Payload, &r); err != nil {
		log.Warnf("failed to parse PTY request: %v", err)
		return nil, err
	}

	// if the caller asked for an invalid sized pty (like ansible
	// which asks for a 0x0 size) update the request with defaults
	if err := r.CheckAndSetDefaults(); err != nil {
		return nil, err
	}

	return &r, nil
}

type remoteTerminal struct {
	wg sync.WaitGroup
	mu sync.Mutex

	ctx *ServerContext

	session     *ssh.Session
	params      rsession.TerminalParams
	sessionDone chan bool
	ptyBuffer   *ptyBuffer
}

func NewRemoteTerminal(ctx *ServerContext) (*remoteTerminal, error) {
	checker := &ssh.CertChecker{
		IsHostAuthority: func(p ssh.PublicKey, addr string) bool {
			caid := services.CertAuthID{DomainName: ctx.ClusterName, Type: services.HostCA}
			ca, err := ctx.srv.GetAuthService().GetCertAuthority(caid, false)
			if err != nil {
				return false
			}
			checkers, err := ca.Checkers()
			if err != nil {
				return false
			}

			for _, checker := range checkers {
				addrMatch := subtle.ConstantTimeCompare([]byte(ctx.srv.AdvertiseAddr()), []byte(addr)) == 1
				caMatch := subtle.ConstantTimeCompare(checker.Marshal(), p.Marshal()) == 1

				if addrMatch && caMatch {
					return true
				}
			}
			return false
		},
	}

	// TODO(russjones): Wait for the agent to be ready or a timeout.
	select {
	case <-ctx.agentReady:
	}
	authMethod := ssh.PublicKeysCallback(ctx.agent.Signers)

	clientConfig := &ssh.ClientConfig{
		User: ctx.Login,
		Auth: []ssh.AuthMethod{
			authMethod,
		},
		HostKeyCallback: checker.CheckHostKey,
	}

	// TODO(russjones): Add a timeout here.
	client, err := ssh.Dial("tcp", ctx.srv.AdvertiseAddr(), clientConfig)
	if err != nil {
		return nil, err
	}

	session, err := client.NewSession()
	if err != nil {
		return nil, err
	}

	t := &remoteTerminal{
		session:     session,
		sessionDone: make(chan bool),
		ptyBuffer:   &ptyBuffer{},
	}

	return t, nil
}

type ptyBuffer struct {
	r io.Reader
	w io.Writer
}

func (b *ptyBuffer) Read(p []byte) (n int, err error) {
	return b.r.Read(p)
}

func (b *ptyBuffer) Write(p []byte) (n int, err error) {
	return b.w.Write(p)
}

func (t *remoteTerminal) Run() error {
	// combine stdout and stderr
	stdout, err := t.session.StdoutPipe()
	if err != nil {
		return trace.Wrap(err)
	}
	t.session.Stderr = t.session.Stdout
	stdin, err := t.session.StdinPipe()
	if err != nil {
		return trace.Wrap(err)
	}

	t.ptyBuffer = &ptyBuffer{
		r: stdout,
		w: stdin,
	}

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}

	if err := t.session.RequestPty("xterm", t.params.W, t.params.H, modes); err != nil {
		return trace.Wrap(err)
	}

	if err := t.session.Shell(); err != nil {
		return trace.Wrap(err)
	}

	return nil
}

func (t *remoteTerminal) Wait() error {
	t.wg.Wait()
	return nil
}

func (t *remoteTerminal) PTY() io.ReadWriter {
	return t.ptyBuffer
}

func (t *remoteTerminal) Close() error {
	// wait until all participants are done copying
	t.wg.Wait()

	err := t.session.Close()
	if err != nil {
		return trace.Wrap(err)
	}

	return nil
}

func (t *remoteTerminal) GetWinSize() (*term.Winsize, error) {
	return t.params.Winsize(), nil
}

func (t *remoteTerminal) SetWinSize(params rsession.TerminalParams) error {
	// TODO(russjones): Revendor the SSH library so so we can update the window
	// size here.
	//err = session.WindowChange(params.H, params.W)
	//if err != nil {
	//    return nil, nil, err
	//}
	t.params = params
	return nil
}

func (t *remoteTerminal) GetTerminalParams() rsession.TerminalParams {
	return t.params
}
