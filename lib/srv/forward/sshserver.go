package forward

import (
	//"crypto/subtle"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	//"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/events"
	rsession "github.com/gravitational/teleport/lib/session"
	psrv "github.com/gravitational/teleport/lib/srv"
	"github.com/gravitational/teleport/lib/utils"

	log "github.com/sirupsen/logrus"
)

type fakeServer struct {
	hostCAChecker    ssh.PublicKey
	userCAChecker    ssh.PublicKey
	remoteHostSigner ssh.Signer
	remoteHostPort   string

	alog          events.IAuditLog
	authService   auth.AccessPoint
	reg           *psrv.SessionRegistry
	sessionServer rsession.Service
}

func New(authClient auth.ClientI) (*fakeServer, error) {
	s := &fakeServer{
		alog:          authClient,
		authService:   authClient,
		sessionServer: authClient,
	}
	s.reg = psrv.NewSessionRegistry(s)
	return s, nil
}

func (f *fakeServer) ID() string {
	return "0"
}

func (f *fakeServer) GetNamespace() string {
	return "default"
}

func (f *fakeServer) LogFields(fields map[string]interface{}) log.Fields {
	return log.Fields{
		teleport.Component:       "forwarder",
		teleport.ComponentFields: fields,
	}
}

func (f *fakeServer) EmitAuditEvent(eventType string, fields events.EventFields) {
	log.Debugf("server.EmitAuditEvent(%v)", eventType)
	alog := f.GetAuditLog()
	if alog != nil {
		if err := alog.EmitAuditEvent(eventType, fields); err != nil {
			log.Error(err)
		}
	} else {
		log.Warn("SSH server has no audit log")
	}
}

// PermitUserEnvironment is always false because it's up the the remote host
// to decide if the user environment is ready or not.
func (f *fakeServer) PermitUserEnvironment() bool {
	return false
}

func (f *fakeServer) GetAuditLog() events.IAuditLog {
	return f.alog
}

func (f *fakeServer) GetAuthService() auth.AccessPoint {
	return f.authService
}

func (f *fakeServer) GetSessionServer() rsession.Service {
	return f.sessionServer
}

func (f *fakeServer) Dial(conn net.Conn) error {
	//userChecker := &ssh.CertChecker{
	//	IsUserAuthority: func(p ssh.PublicKey) bool {
	//		return subtle.ConstantTimeCompare(f.userCAChecker.Marshal(), p.Marshal()) == 1
	//	},
	//}

	config := &ssh.ServerConfig{
		//PublicKeyCallback: userChecker.Authenticate,
		NoClientAuth: true,
	}
	nodeSigner, err := readSigner("/Users/rjones/Development/go/src/github.com/gravitational/rusty/teleport/local/one/data/node")
	if err != nil {
		log.Errorf("readsigner: err: %v", err)
		return err
	}
	config.AddHostKey(nodeSigner)

	log.Errorf("trying to make new server conn")

	sconn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		log.Errorf("newserverconn: err: %v", err)
		return err
	}

	sconn.Permissions = &ssh.Permissions{
		Extensions: map[string]string{utils.CertTeleportUser: "rjones"},
	}

	log.Errorf("new server conn: %v", sconn)

	// global requests
	go func() {
		for newRequest := range reqs {
			go f.handleGlobalRequest(sconn, newRequest)
		}
	}()

	// go handle global channel requests
	go func() {
		for newChannel := range chans {
			go f.handleChannel(sconn, newChannel)
		}
	}()

	log.Errorf("Dial done!")

	return nil
}

func (f *fakeServer) handleGlobalRequest(sconn *ssh.ServerConn, r *ssh.Request) {
	fmt.Printf("handleGlobalRequest: %#v\n", r)

	switch r.Type {
	case "keepalive@openssh.com":
		if r.WantReply {
			_, _, err := sconn.SendRequest("keepalive@openssh.com", true, nil)
			if err != nil {
				fmt.Printf("handleGlobalRequest: SendRequest: err: %v\n", err)
			}
			r.Reply(false, nil)
		}
	default:
		fmt.Printf("Unknown type: %v\n", r.Type)
		r.Reply(false, nil)
	}
}

func (f *fakeServer) handleChannel(sconn *ssh.ServerConn, c ssh.NewChannel) {
	log.Errorf("handleChannel")
	channel, requests, err := c.Accept()
	if err != nil {
		fmt.Printf("handleChannel: err: %v\n", err)
		return
	}

	ctx := psrv.NewServerContext(f, sconn)
	err = ctx.JoinOrCreateSession(f.reg)
	if err != nil {
		log.Errorf("JoinOrCreateSession problem: %v", err)
		return
	}

	for req := range requests {
		go f.handleRequest(ctx, channel, req)
	}
}

func (f *fakeServer) handleRequest(ctx *psrv.ServerContext, channel ssh.Channel, req *ssh.Request) error {
	var err error

	switch req.Type {
	case "env":
		err = f.handleEnv(channel, req)
	case "exec":
		err = f.handleExec(ctx, channel, req)
	case "pty-req":
		err = f.handlePtyReq(ctx, channel, req)
	case "subsystem":
		err = f.handleSubsystem(ctx, channel, req)
	case "shell":
		err = f.handleShell(ctx, channel, req)
	case "window-change":
		err = f.handleWindowChange(ctx, channel, req)
	case "auth-agent-req@openssh.com":
		err = f.handleAgentForward(ctx, channel, req)
	default:
		message := fmt.Sprintf("unsupported request type: %q\n", req.Type)
		replyError(message, channel, req)
		return fmt.Errorf(message)
	}

	if err != nil {
		replyError(err.Error(), channel, req)
		return err
	}

	replySuccess(req)
	return nil
}

func (f *fakeServer) handleEnv(channel ssh.Channel, req *ssh.Request) error {
	fmt.Printf("fakeServer: handleEnv\n")
	return nil
}

func (f *fakeServer) handleSubsystem(ctx *psrv.ServerContext, channel ssh.Channel, req *ssh.Request) error {
	type subsystemRequest struct {
		Name string
	}
	var s subsystemRequest
	err := ssh.Unmarshal(req.Payload, &s)
	if err != nil {
		replyError(err.Error(), channel, req)
		return err
	}

	fmt.Printf("fakeServer: handleSubsystem: %v\n", s.Name)

	session, err := f.remoteSession(ctx)
	if err != nil {
		replyError(err.Error(), channel, req)
		return err
	}

	err = session.RequestSubsystem(s.Name)
	if err != nil {
		replyError(err.Error(), channel, req)
		return err
	}

	done := make(chan bool)

	sout, err := session.StdoutPipe()
	if err != nil {
		replyError(err.Error(), channel, req)
		return err
	}
	sin, err := session.StdinPipe()
	if err != nil {
		replyError(err.Error(), channel, req)
		return err
	}

	go func() {
		io.Copy(sin, channel)
		close(done)
	}()
	go func() {
		io.Copy(channel, sout)
	}()

	<-done
	channel.Close()

	fmt.Printf("fakeServer: handleSubsystem: %v: complete\n", s.Name)

	return nil
}

func (f *fakeServer) handleExec(ctx *psrv.ServerContext, channel ssh.Channel, req *ssh.Request) error {
	type execRequest struct {
		Command string
	}

	var e execRequest
	if err := ssh.Unmarshal(req.Payload, &e); err != nil {
		return err
	}

	fmt.Printf("fakeServer: handleExec: %v\n", e.Command)

	// open a session to the remote host
	session, err := f.remoteSession(ctx)
	if err != nil {
		fmt.Printf("unable to open remote %v session: %v\n", f.remoteHostPort, err)
		return err
	}
	defer session.Close()

	done := make(chan bool)

	sout, err := session.StdoutPipe()
	if err != nil {
		replyError(err.Error(), channel, req)
		return err
	}
	sin, err := session.StdinPipe()
	if err != nil {
		replyError(err.Error(), channel, req)
		return err
	}

	go func() {
		io.Copy(sin, channel)
		close(done)
	}()
	go func() {
		io.Copy(channel, sout)
	}()

	_, err = session.SendRequest(req.Type, req.WantReply, req.Payload)
	if err != nil {
		replyError(err.Error(), channel, req)
		return err
	}

	<-done
	channel.Close()

	fmt.Printf("fakeServer: handleExec: %v: complete\n", e.Command)

	return nil
}

func (f *fakeServer) handlePtyReq(ctx *psrv.ServerContext, channel ssh.Channel, req *ssh.Request) error {
	var err error

	type ptyRequest struct {
		Env   string
		W     uint32
		H     uint32
		Wpx   uint32
		Hpx   uint32
		Modes string
	}

	var p ptyRequest
	if err := ssh.Unmarshal(req.Payload, &p); err != nil {
		return err
	}
	fmt.Printf("fakeServer: handlePtyReq: %v, w=%v, h=%v\n", p.Env, p.W, p.H)

	term := ctx.GetTerm()
	if term == nil {
		//term, _, err = psrv.NewRemoteTerminal(req)
		term, err = psrv.NewLocalTerminal()
		if err != nil {
			return err
		}
		ctx.SetTerm(term)
	}

	return nil
}

func (f *fakeServer) handleShell(ctx *psrv.ServerContext, channel ssh.Channel, req *ssh.Request) error {
	fmt.Printf("fakeServer: handleShell\n")

	if err := f.reg.OpenSession(channel, req, ctx); err != nil {
		log.Error(err)
		return err
	}

	//session, err := f.remoteSession(ctx)
	//if err != nil {
	//	fmt.Printf("unable to open remote %v session: %v\n", f.remoteHostPort, err)
	//	return err
	//}
	//defer session.Close()

	//done := make(chan bool)

	//sout, err := session.StdoutPipe()
	//if err != nil {
	//	log.Errorf("problem! sout: %v\n", err)
	//}
	//serr, err := session.StderrPipe()
	//if err != nil {
	//	log.Errorf("problem! serr: %v\n", err)
	//}
	//sin, _ := session.StdinPipe()

	//w1 := io.MultiWriter(channel, os.Stdout)
	////w2 := io.MultiWriter(sin, os.Stdout)

	//go func() {
	//	_, err := io.Copy(sin, channel)
	//	log.Errorf("sin copy err: %v", err)
	//}()
	//go func() {
	//	//io.Copy(channel, sout)
	//	_, err := io.Copy(w1, sout)
	//	log.Errorf("sout copy err: %v", err)
	//}()
	//go func() {
	//	//io.Copy(channel, serr)
	//	_, err := io.Copy(w1, serr)
	//	log.Errorf("serr copy err: %v", err)
	//	close(done)
	//}()

	//// Set up terminal modes
	//modes := ssh.TerminalModes{
	//	ssh.ECHO:          1,     // disable echoing
	//	ssh.TTY_OP_ISPEED: 14400, // input speed = 14.4kbaud
	//	ssh.TTY_OP_OSPEED: 14400, // output speed = 14.4kbaud
	//}

	//// Request pseudo terminal
	//if err := session.RequestPty("xterm", 80, 40, modes); err != nil {
	//	return err
	//}

	//// Start remote shell
	//if err := session.Shell(); err != nil {
	//	return err
	//}

	//log.Errorf("fakeServer: waiting for done")
	//<-done
	//channel.Close()
	////ctx.agentChan.Close()

	//fmt.Printf("fakeServer: handleShell: complete\n")

	return nil
}

func (f *fakeServer) handleAgentForward(ctx *psrv.ServerContext, channel ssh.Channel, req *ssh.Request) error {
	fmt.Printf("fakeServer: forwarding agent\n")

	authChannel, _, err := ctx.Conn.OpenChannel("auth-agent@openssh.com", nil)
	if err != nil {
		return err
	}
	ctx.SetAgent(agent.NewClient(authChannel), channel)

	//close(ctx.agentReady)

	return nil
}

func (f *fakeServer) handleWindowChange(ctx *psrv.ServerContext, channel ssh.Channel, req *ssh.Request) error {
	type windowChangeRequest struct {
		W   uint32
		H   uint32
		Wpx uint32
		Hpx uint32
	}
	var w windowChangeRequest
	if err := ssh.Unmarshal(req.Payload, &w); err != nil {
		return err
	}

	fmt.Printf("fakeServer: window change: H=%v, W=%v\n", w.H, w.W)

	//err := ctx.session.WindowChange(int(w.H), int(w.W))
	//if err != nil {
	//	replyError(err.Error(), channel, req)
	//	return err
	//}

	return nil
}

func (f *fakeServer) remoteSession(ctx *psrv.ServerContext) (*ssh.Session, error) {
	//checker := &ssh.CertChecker{
	//	IsHostAuthority: func(p ssh.PublicKey, addr string) bool {
	//		addrMatch := subtle.ConstantTimeCompare([]byte("node.example.com:22"), []byte(addr)) == 1
	//		caMatch := subtle.ConstantTimeCompare(f.hostCAChecker.Marshal(), p.Marshal()) == 1

	//		return addrMatch && caMatch
	//	},
	//}

	// wait for agent to be ready before building the remote session
	//<-ctx.agentReady
	//authMethod := ssh.PublicKeysCallback(ctx.agent.Signers)

	// hack until we create a wrapper to allow agent forwarding for scp and sftp
	systemAgent, err := net.Dial("unix", os.Getenv("SSH_AUTH_SOCK"))
	if err != nil {
		return nil, err
	}
	authMethod := ssh.PublicKeysCallback(agent.NewClient(systemAgent).Signers)

	clientConfig := &ssh.ClientConfig{
		User: "rjones",
		Auth: []ssh.AuthMethod{
			authMethod,
		},
		//HostKeyCallback: checker.CheckHostKey,
	}

	client, err := ssh.Dial("tcp", "localhost:22", clientConfig)
	if err != nil {
		return nil, err
	}

	session, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	//ctx.session = session

	//err = agent.RequestAgentForwarding(session)
	//if err != nil {
	//	return nil, err
	//}

	//err = agent.ForwardToAgent(client, ctx.agent)
	//if err != nil {
	//	return nil, err
	//}

	return session, nil
}

func replySuccess(req *ssh.Request) {
	if req.WantReply {
		req.Reply(true, nil)
	}
}

func replyError(message string, ch ssh.Channel, req *ssh.Request) {
	ch.Stderr().Write([]byte(message))
	if req.WantReply {
		req.Reply(false, []byte(message))
	}
}

func readSigner(path string) (ssh.Signer, error) {
	privateKey, err := readPrivateKey(path + ".key")
	if err != nil {
		return nil, err
	}

	cert, err := readCertificate(path + ".cert")
	if err != nil {
		return nil, err
	}

	s, err := ssh.NewCertSigner(cert, privateKey)
	if err != nil {
		return nil, err
	}

	return s, nil
}

func readPrivateKey(path string) (ssh.Signer, error) {
	privateBytes, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}

	private, err := ssh.ParsePrivateKey(privateBytes)
	if err != nil {
		return nil, err
	}

	return private, nil
}

func readCertificate(path string) (*ssh.Certificate, error) {
	publicBytes, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}

	key, _, _, _, err := ssh.ParseAuthorizedKey(publicBytes)
	if err != nil {
		return nil, err
	}

	sshCert, ok := key.(*ssh.Certificate)
	if !ok {
		return nil, fmt.Errorf("not cert")
	}

	return sshCert, nil
}