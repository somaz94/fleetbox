package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const (
	defaultDockerPath = "docker"
	dialStdioArgs     = " system dial-stdio"
	sshTimeout        = 15 * time.Second
)

// The remote shell parses the command, so a configured path is limited to
// characters that need no quoting.
var dockerPathPattern = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

// sshTransport reaches dockerd by running `docker system dial-stdio` over SSH,
// one session per HTTP connection on a shared client. It uses
// golang.org/x/crypto/ssh rather than an ssh binary, which the distroless
// image does not have, and refuses a host whose key is not pinned.
type sshTransport struct {
	addr    string
	command string
	config  *ssh.ClientConfig

	mu     sync.Mutex
	client *ssh.Client
}

func newSSHTransport(c Config) (*sshTransport, error) {
	if c.Host == "" || c.User == "" || c.KeyFile == "" || c.KnownHostsFile == "" {
		return nil, errors.New("ssh transport needs host, user, keyFile and knownHostsFile")
	}
	dockerPath := c.DockerPath
	if dockerPath == "" {
		dockerPath = defaultDockerPath
	} else if !dockerPathPattern.MatchString(dockerPath) {
		return nil, fmt.Errorf("dockerPath %q must be an absolute path of letters, digits and ._/-", dockerPath)
	}
	key, err := os.ReadFile(c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("read key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("parse key %s: %w", c.KeyFile, err)
	}
	hostKeys, err := knownhosts.New(c.KnownHostsFile)
	if err != nil {
		return nil, fmt.Errorf("known hosts: %w", err)
	}
	port := c.Port
	if port == 0 {
		port = 22
	}
	return &sshTransport{
		addr:    net.JoinHostPort(c.Host, strconv.Itoa(port)),
		command: dockerPath + dialStdioArgs,
		config: &ssh.ClientConfig{
			User:            c.User,
			Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
			HostKeyCallback: hostKeys,
			Timeout:         sshTimeout,
		},
	}, nil
}

func (t *sshTransport) DialContext(ctx context.Context) (net.Conn, error) {
	client, err := t.connect(ctx)
	if err != nil {
		return nil, err
	}
	conn, err := openDialStdio(client, t.command)
	if err != nil {
		// The shared connection may have died; reconnect once.
		t.drop(client)
		if client, err = t.connect(ctx); err != nil {
			return nil, err
		}
		return openDialStdio(client, t.command)
	}
	return conn, nil
}

func (t *sshTransport) connect(ctx context.Context) (*ssh.Client, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.client != nil {
		return t.client, nil
	}
	d := net.Dialer{Timeout: sshTimeout}
	nc, err := d.DialContext(ctx, "tcp", t.addr)
	if err != nil {
		return nil, err
	}
	c, chans, reqs, err := ssh.NewClientConn(nc, t.addr, t.config)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("ssh %s: %w", t.addr, err)
	}
	t.client = ssh.NewClient(c, chans, reqs)
	return t.client, nil
}

func (t *sshTransport) drop(c *ssh.Client) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.client == c {
		t.client.Close()
		t.client = nil
	}
}

// Close ends the shared SSH connection.
func (t *sshTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.client == nil {
		return nil
	}
	err := t.client.Close()
	t.client = nil
	return err
}

func openDialStdio(c *ssh.Client, command string) (net.Conn, error) {
	sess, err := c.NewSession()
	if err != nil {
		return nil, err
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	if err := sess.Start(command); err != nil {
		sess.Close()
		return nil, err
	}
	return &sessionConn{sess: sess, stdin: stdin, stdout: stdout, remote: c.RemoteAddr(), local: c.LocalAddr()}, nil
}

// sessionConn presents one SSH session's stdio as a net.Conn. Deadlines are
// not supported; callers bound requests with contexts instead.
type sessionConn struct {
	sess          *ssh.Session
	stdin         io.WriteCloser
	stdout        io.Reader
	local, remote net.Addr
	once          sync.Once
}

func (c *sessionConn) Read(b []byte) (int, error)  { return c.stdout.Read(b) }
func (c *sessionConn) Write(b []byte) (int, error) { return c.stdin.Write(b) }
func (c *sessionConn) LocalAddr() net.Addr         { return c.local }
func (c *sessionConn) RemoteAddr() net.Addr        { return c.remote }
func (c *sessionConn) SetDeadline(time.Time) error { return nil }
func (c *sessionConn) SetReadDeadline(time.Time) error {
	return nil
}
func (c *sessionConn) SetWriteDeadline(time.Time) error { return nil }

func (c *sessionConn) Close() error {
	c.once.Do(func() {
		c.stdin.Close()
		c.sess.Close()
	})
	return nil
}
