package migrate

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// sieveScript is one script of the source account.
type sieveScript struct {
	Name   string
	Text   string
	Active bool
}

// sieveClient is a minimal ManageSieve (RFC 5804) client.
type sieveClient struct {
	ep   Endpoint
	conn net.Conn
	lr   *lineReader
}

// dialSieve connects, reads the greeting and upgrades with STARTTLS if
// configured.
func dialSieve(ctx context.Context, ep Endpoint) (*sieveClient, error) {
	conn, err := ep.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", ep.Addr, err)
	}

	c := &sieveClient{ep: ep, conn: conn, lr: &lineReader{br: bufio.NewReader(conn), litLimit: 16 << 20}}

	if _, err := c.response(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("managesieve greeting from %s: %w", ep.Addr, err)
	}

	if ep.TLS != TLSStartTLS {
		return c, nil
	}

	if _, err := c.cmd("STARTTLS"); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("starttls: %w", err)
	}

	cfg, err := ep.tlsConfig()
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	tc := tls.Client(conn, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("starttls: %w", err)
	}

	c.conn, c.lr = tc, &lineReader{br: bufio.NewReader(tc), litLimit: 16 << 20}

	// RFC 5804 2.2: the server repeats its capabilities after STARTTLS.
	if _, err := c.response(); err != nil {
		_ = c.conn.Close()
		return nil, fmt.Errorf("starttls: %w", err)
	}

	return c, nil
}

// Close logs out and closes the connection.
func (c *sieveClient) Close() {
	_, _ = c.cmd("LOGOUT")
	_ = c.conn.Close()
}

// cmd sends a command and returns the lines before its OK.
func (c *sieveClient) cmd(command string) ([]*response, error) {
	_ = c.conn.SetDeadline(time.Now().Add(c.ep.Timeout))

	if _, err := c.conn.Write([]byte(command + "\r\n")); err != nil {
		return nil, err
	}

	return c.response()
}

// response reads lines up to OK (returned) or NO/BYE (error).
func (c *sieveClient) response() ([]*response, error) {
	var out []*response

	for {
		_ = c.conn.SetDeadline(time.Now().Add(c.ep.Timeout))

		r, err := c.lr.readResponse()
		if err != nil {
			return nil, err
		}

		word, rest, _ := strings.Cut(r.text, " ")

		switch strings.ToUpper(word) {
		case "OK":
			return out, nil
		case statusNO, "BYE":
			return nil, fmt.Errorf("managesieve: %s %s", word, rest)
		default:
			out = append(out, r)
		}
	}
}

// login authenticates with SASL PLAIN (a master login is "user*master"
// as the authentication identity).
func (c *sieveClient) login(user, password string) error {
	ir := base64.StdEncoding.EncodeToString([]byte("\x00" + user + "\x00" + password))
	if _, err := c.cmd(`AUTHENTICATE "PLAIN" "` + ir + `"`); err != nil {
		return fmt.Errorf("sieve login: %w", err)
	}

	return nil
}

// scripts reads all scripts with their active flag.
func (c *sieveClient) scripts() ([]sieveScript, error) {
	rs, err := c.cmd("LISTSCRIPTS")
	if err != nil {
		return nil, err
	}

	var out []sieveScript

	for _, r := range rs {
		vs, err := newParser(r, 0).values()
		if err != nil || len(vs) == 0 {
			return nil, errors.New("malformed LISTSCRIPTS response")
		}

		name, ok := str(vs[0])
		if !ok {
			return nil, errors.New("malformed LISTSCRIPTS response")
		}

		active := false

		if len(vs) > 1 {
			flag, _ := str(vs[1])
			active = strings.EqualFold(flag, "ACTIVE")
		}

		out = append(out, sieveScript{Name: name, Active: active})
	}

	for i := range out {
		rs, err := c.cmd("GETSCRIPT " + quote(out[i].Name))
		if err != nil {
			return nil, fmt.Errorf("getscript %q: %w", out[i].Name, err)
		}

		if len(rs) != 1 {
			return nil, fmt.Errorf("getscript %q: malformed response", out[i].Name)
		}

		vs, err := newParser(rs[0], 0).values()
		if err != nil || len(vs) != 1 {
			return nil, fmt.Errorf("getscript %q: malformed response", out[i].Name)
		}

		out[i].Text, _ = str(vs[0])
	}

	return out, nil
}
