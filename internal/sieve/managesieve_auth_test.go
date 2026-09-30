package sieve

import (
	"bufio"
	"encoding/base64"
	"net"
	"strings"
	"testing"
	"time"

	"shardpost/internal/auth"
	"shardpost/internal/memstore"
)

// sieveClient is a line-based ManageSieve test client.
type sieveClient struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
}

func dialSieveServer(t *testing.T) *sieveClient {
	t.Helper()

	srv := New(Options{Store: memstore.New(), Auth: auth.Static{"alice": "secret"}, InsecureAuth: true})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go func() { _ = srv.Serve(ln) }()

	t.Cleanup(func() { _ = ln.Close() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	c := &sieveClient{t: t, conn: conn, br: bufio.NewReader(conn)}
	c.until("OK") // greeting with the capabilities

	return c
}

// send writes a line and returns the response lines up to the next one
// that starts with OK, NO or BYE, or the first challenge line (a quoted
// string) when stopAtChallenge is set.
func (c *sieveClient) send(line string, stopAtChallenge bool) []string {
	c.t.Helper()

	if _, err := c.conn.Write([]byte(line + "\r\n")); err != nil {
		c.t.Fatal(err)
	}

	if stopAtChallenge {
		return []string{c.line()}
	}

	return c.until("OK", "NO", "BYE")
}

func (c *sieveClient) line() string {
	c.t.Helper()

	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	l, err := c.br.ReadString('\n')
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}

	return strings.TrimRight(l, "\r\n")
}

func (c *sieveClient) until(prefixes ...string) []string {
	c.t.Helper()

	var out []string

	for {
		l := c.line()
		out = append(out, l)

		for _, p := range prefixes {
			if strings.HasPrefix(l, p) {
				return out
			}
		}
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// TestManageSieveOAuthMechanisms: ManageSieve takes a bearer token with
// XOAUTH2 and OAUTHBEARER, as IMAP does — a webmail that logged in with
// OIDC, and the director that authenticates the same way to the backend.
// A failed token login gets NO at once.
func TestManageSieveOAuthMechanisms(t *testing.T) {
	c := dialSieveServer(t)

	caps := strings.Join(c.send("CAPABILITY", false), "\n")
	if !strings.Contains(caps, `"SASL" "PLAIN XOAUTH2 OAUTHBEARER"`) {
		t.Fatalf("capabilities: %s", caps)
	}

	// A wrong token: NO at once, no error challenge a client would have
	// to answer; a broken response too.
	bad := b64("n,a=alice,\x01auth=Bearer wrong\x01\x01")
	if l := c.send(`AUTHENTICATE "OAUTHBEARER" "`+bad+`"`, false); len(l) != 1 || !strings.HasPrefix(l[0], "NO") {
		t.Fatalf("failed login: %v", l)
	}

	if l := c.send(`AUTHENTICATE "XOAUTH2" "not base64"`, false); len(l) != 1 || !strings.HasPrefix(l[0], "NO") {
		t.Fatalf("broken response: %v", l)
	}

	// Cancelled by the client.
	if l := c.send(`AUTHENTICATE "XOAUTH2"`, true)[0]; l != `""` {
		t.Fatalf("no empty challenge: %s", l)
	}

	if l := c.send(`"*"`, false); !strings.HasPrefix(l[len(l)-1], "NO") {
		t.Fatalf("cancel: %v", l)
	}

	good := b64("user=alice\x01auth=Bearer secret\x01\x01")
	if l := c.send(`AUTHENTICATE "XOAUTH2" "`+good+`"`, false); !strings.HasPrefix(l[len(l)-1], "OK") {
		t.Fatalf("XOAUTH2 login: %v", l)
	}

	if l := c.send("LISTSCRIPTS", false); !strings.HasPrefix(l[len(l)-1], "OK") {
		t.Fatalf("after login: %v", l)
	}
}

// TestManageSieveOAuthBearerLogin: a good OAUTHBEARER token opens the
// account named in the token response.
func TestManageSieveOAuthBearerLogin(t *testing.T) {
	c := dialSieveServer(t)

	good := b64("n,a=alice,\x01auth=Bearer secret\x01\x01")
	if l := c.send(`AUTHENTICATE "OAUTHBEARER" "`+good+`"`, false); !strings.HasPrefix(l[len(l)-1], "OK") {
		t.Fatalf("OAUTHBEARER login: %v", l)
	}
}
