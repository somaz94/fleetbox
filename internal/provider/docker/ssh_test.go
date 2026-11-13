package docker

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"gopkg.in/yaml.v3"

	"github.com/somaz94/fleetbox/internal/config"
	"github.com/somaz94/fleetbox/internal/provider"
)

type sshFixture struct {
	port           int
	addr           string
	keyFile        string
	knownHostsFile string
	hostKey        ssh.PublicKey
	execs          chan string
}

func newSigner(t *testing.T) (ssh.Signer, ed25519.PrivateKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s, priv
}

// startSSH runs an SSH server that accepts one user and key and answers
// `docker system dial-stdio` by proxying to backend.
func startSSH(t *testing.T, backend string) *sshFixture {
	t.Helper()
	dir := t.TempDir()
	hostSigner, _ := newSigner(t)
	clientSigner, clientPriv := newSigner(t)

	block, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	fx := &sshFixture{keyFile: filepath.Join(dir, "id_ed25519"), knownHostsFile: filepath.Join(dir, "known_hosts"),
		hostKey: hostSigner.PublicKey(), execs: make(chan string, 16)}
	if err := os.WriteFile(fx.keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &ssh.ServerConfig{PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if c.User() == "fleetbox" && bytes.Equal(k.Marshal(), clientSigner.PublicKey().Marshal()) {
			return nil, nil
		}
		return nil, errors.New("denied")
	}}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	fx.addr = ln.Addr().String()
	fx.port = ln.Addr().(*net.TCPAddr).Port
	if err := os.WriteFile(fx.knownHostsFile, []byte(knownhosts.Line([]string{fx.addr}, fx.hostKey)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSSHConn(nc, cfg, backend, fx.execs)
		}
	}()
	return fx
}

func serveSSHConn(nc net.Conn, cfg *ssh.ServerConfig, backend string, execs chan<- string) {
	_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		nc.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		if nch.ChannelType() != "session" {
			nch.Reject(ssh.UnknownChannelType, "session only")
			continue
		}
		ch, chReqs, err := nch.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range chReqs {
				if req.Type != "exec" {
					req.Reply(false, nil)
					continue
				}
				var payload struct{ Command string }
				ssh.Unmarshal(req.Payload, &payload)
				select {
				case execs <- payload.Command:
				default:
				}
				if !strings.HasSuffix(payload.Command, dialStdioArgs) {
					req.Reply(false, nil)
					ch.Close()
					continue
				}
				req.Reply(true, nil)
				be, err := net.Dial("tcp", backend)
				if err != nil {
					ch.Close()
					continue
				}
				go func() {
					io.Copy(be, ch)
					be.(*net.TCPConn).CloseWrite()
				}()
				go func() {
					io.Copy(ch, be)
					ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
					ch.Close()
					be.Close()
				}()
			}
		}()
	}
}

func sshConfig(fx *sshFixture) Config {
	return Config{Host: "127.0.0.1", Port: fx.port, User: "fleetbox", KeyFile: fx.keyFile, KnownHostsFile: fx.knownHostsFile}
}

func TestSSHTransportEndToEnd(t *testing.T) {
	srv := httptest.NewServer(newDaemon().handler())
	defer srv.Close()
	fx := startSSH(t, strings.TrimPrefix(srv.URL, "http://"))

	tr, err := newSSHTransport(sshConfig(fx))
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	inv, err := NewWithDialer("host1", tr).Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Services) != 4 {
		t.Fatalf("services = %d", len(inv.Services))
	}
	if cmd := <-fx.execs; cmd != "docker system dial-stdio" {
		t.Fatalf("exec = %q", cmd)
	}

	// A dead shared connection is replaced on the next dial.
	tr.mu.Lock()
	tr.client.Close()
	tr.mu.Unlock()
	conn, err := tr.DialContext(context.Background())
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if conn.LocalAddr() == nil || conn.RemoteAddr() == nil || conn.SetDeadline(time.Time{}) != nil ||
		conn.SetReadDeadline(time.Time{}) != nil || conn.SetWriteDeadline(time.Time{}) != nil {
		t.Fatal("conn methods misbehave")
	}
	conn.Close()
	conn.Close() // idempotent

	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// A NAS keeps the CLI off the non-interactive PATH; the configured path is
// what runs on the host.
func TestSSHTransportRunsTheConfiguredDockerPath(t *testing.T) {
	srv := httptest.NewServer(newDaemon().handler())
	defer srv.Close()
	fx := startSSH(t, strings.TrimPrefix(srv.URL, "http://"))

	c := sshConfig(fx)
	c.DockerPath = "/usr/local/bin/docker"
	tr, err := newSSHTransport(c)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	if _, err := NewWithDialer("host-a", tr).Inventory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cmd := <-fx.execs; cmd != "/usr/local/bin/docker system dial-stdio" {
		t.Fatalf("exec = %q", cmd)
	}
}

func TestSSHTransportRefusesUnpinnedHost(t *testing.T) {
	fx := startSSH(t, "127.0.0.1:1")
	other, _ := newSigner(t)
	if err := os.WriteFile(fx.knownHostsFile, []byte(knownhosts.Line([]string{fx.addr}, other.PublicKey())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr, err := newSSHTransport(sshConfig(fx))
	if err != nil {
		t.Fatal(err)
	}
	_, err = tr.DialContext(context.Background())
	var keyErr *knownhosts.KeyError
	if !errors.As(err, &keyErr) {
		t.Fatalf("err = %v, want a knownhosts.KeyError", err)
	}
	// A host missing from known_hosts is refused the same way, never trusted on first use.
	if err := os.WriteFile(fx.knownHostsFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tr, _ = newSSHTransport(sshConfig(fx))
	if _, err := tr.DialContext(context.Background()); !errors.As(err, &keyErr) {
		t.Fatalf("err = %v, want a knownhosts.KeyError", err)
	}
}

func TestSSHTransportDialErrors(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	fx := startSSH(t, "127.0.0.1:1")
	cfg := sshConfig(fx)
	cfg.Port = port
	tr, err := newSSHTransport(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.DialContext(context.Background()); err == nil {
		t.Fatal("dial to a closed port succeeded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tr.DialContext(ctx); err == nil {
		t.Fatal("dial with a cancelled context succeeded")
	}
}

func TestNewSSHTransportErrors(t *testing.T) {
	fx := startSSH(t, "127.0.0.1:1")
	bad := filepath.Join(t.TempDir(), "bad")
	os.WriteFile(bad, []byte("not a key"), 0o600)

	tests := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{name: "missing fields", edit: func(c *Config) { c.User = "" }, want: "needs host, user"},
		{name: "missing key file", edit: func(c *Config) { c.KeyFile = bad + "-missing" }, want: "read key"},
		{name: "bad key", edit: func(c *Config) { c.KeyFile = bad }, want: "parse key"},
		{name: "missing known_hosts", edit: func(c *Config) { c.KnownHostsFile = bad + "-missing" }, want: "known hosts"},
		{name: "relative dockerPath", edit: func(c *Config) { c.DockerPath = "bin/docker" }, want: "must be an absolute path"},
		{name: "dockerPath with shell syntax", edit: func(c *Config) { c.DockerPath = "/usr/bin/docker;id" }, want: "must be an absolute path"},
		{name: "dockerPath with space", edit: func(c *Config) { c.DockerPath = "/usr/bin/sudo docker" }, want: "must be an absolute path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := sshConfig(fx)
			tt.edit(&c)
			if _, err := newSSHTransport(c); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
	c := sshConfig(fx)
	c.Port = 0
	tr, err := newSSHTransport(c)
	if err != nil || !strings.HasSuffix(tr.addr, ":22") {
		t.Fatalf("default port: %v, %v", tr, err)
	}
}

func TestNew(t *testing.T) {
	fx := startSSH(t, "127.0.0.1:1")
	target := func(doc string) config.Target {
		var n yaml.Node
		if err := yaml.Unmarshal([]byte(doc), &n); err != nil {
			t.Fatal(err)
		}
		return config.Target{Name: "host1", Provider: Kind, Spec: *n.Content[0]}
	}
	ok := "host: 127.0.0.1\nport: " + strconv.Itoa(fx.port) + "\nuser: fleetbox\nkeyFile: " + fx.keyFile + "\nknownHostsFile: " + fx.knownHostsFile
	p, err := provider.New(target(ok))
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind() != Kind || !p.Capabilities().FollowLogs || p.Capabilities().Scale {
		t.Fatalf("capabilities = %+v", p.Capabilities())
	}
	if _, ok := p.(provider.Watcher); !ok {
		t.Fatal("docker provider must implement Watcher")
	}
	for doc, want := range map[string]string{
		ok + "\ntransport: agent":  `unknown transport "agent"`,
		ok + "\nhots: x":           "hots",
		"host: 127.0.0.1\nuser: x": "needs host, user",
	} {
		if _, err := New(target(doc)); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want %q", err, want)
		}
	}
}
