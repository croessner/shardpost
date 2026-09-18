package renew

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// maxAnswer bounds the signer's stdout (a PEM certificate) and stderr.
const (
	maxAnswer = 64 << 10
	maxStderr = 4 << 10
)

// SSHSigner calls the constrained signer: the account's ForceCommand
// runs it, the CSR goes to stdin, the certificate comes back on stdout.
type SSHSigner struct {
	// Addr is host:port, User the enrollment account.
	Addr, User string
	// Key authenticates the account (not the certificate key).
	Key ssh.Signer
	// HostKey is the only accepted server host key.
	HostKey ssh.PublicKey
	// DialTimeout bounds the TCP connect and, when the context has no
	// deadline, the whole exchange.
	DialTimeout time.Duration
}

// Sign implements Signer.
func (s *SSHSigner) Sign(ctx context.Context, csrPEM []byte) ([]byte, error) {
	d := net.Dialer{Timeout: s.DialTimeout}

	conn, err := d.DialContext(ctx, "tcp", s.Addr)
	if err != nil {
		return nil, err
	}

	// The whole exchange ends at the context deadline, or after
	// DialTimeout when the context has none.
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(s.DialTimeout)
	}

	_ = conn.SetDeadline(deadline)

	cc, chans, reqs, err := ssh.NewClientConn(conn, s.Addr, &ssh.ClientConfig{
		User: s.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(s.Key)},
		HostKeyCallback: ssh.FixedHostKey(s.HostKey), HostKeyAlgorithms: hostKeyAlgorithms(s.HostKey),
	})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ssh: %w", err)
	}

	client := ssh.NewClient(cc, chans, reqs)
	defer func() { _ = client.Close() }()

	session, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer func() { _ = session.Close() }()

	stdout, stderr := &limited{limit: maxAnswer}, &limited{limit: maxStderr}
	session.Stdin, session.Stdout, session.Stderr = bytes.NewReader(csrPEM), stdout, stderr

	// No command: the server runs the account's ForceCommand (a remote
	// command would be refused by the wrapper).
	if err := session.Shell(); err != nil {
		return nil, err
	}

	if err := session.Wait(); err != nil {
		return nil, fmt.Errorf("signer: %v: %s", err, firstLine(stderr.buf.String()))
	}

	if stdout.over {
		return nil, errors.New("signer answer too large")
	}

	return stdout.buf.Bytes(), nil
}

// hostKeyAlgorithms offers only the pinned key's type, so the server
// presents that key and not another one it also holds.
func hostKeyAlgorithms(key ssh.PublicKey) []string {
	if key.Type() == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	}

	return []string{key.Type()}
}

// limited is a bounded buffer.
type limited struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (l *limited) Write(p []byte) (int, error) {
	if l.buf.Len()+len(p) > l.limit {
		l.over = true
		return len(p), nil
	}

	return l.buf.Write(p)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}
