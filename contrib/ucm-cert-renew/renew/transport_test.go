package renew

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// sshServer answers a "shell" session like the signer's ForceCommand:
// it reads the CSR from stdin and writes answer (or fails with stderr).
func sshServer(t *testing.T, clientKey ssh.PublicKey, answer string, status uint32) (string, ssh.PublicKey) {
	t.Helper()

	addr, keys := sshServerKeys(t, clientKey, answer, status, serveNormal)

	return addr, keys[0]
}

// serveMode selects how the fake signer misbehaves.
type serveMode int

const (
	serveNormal serveMode = iota
	// serveHang never sends an exit status.
	serveHang
	// serveNoShell refuses the shell request.
	serveNoShell
)

// sshServerKeys also holds an RSA host key (returned second) and can
// misbehave as mode says.
func sshServerKeys(t *testing.T, clientKey ssh.PublicKey, answer string, status uint32, mode serveMode) (string, []ssh.PublicKey) {
	t.Helper()

	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)
	rsaPriv, _ := rsa.GenerateKey(rand.Reader, 2048)
	rsaSigner, _ := ssh.NewSignerFromKey(rsaPriv)

	cfg := &ssh.ServerConfig{PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if meta.User() == "enroll" && string(key.Marshal()) == string(clientKey.Marshal()) {
			return nil, nil
		}

		return nil, io.EOF
	}}
	cfg.AddHostKey(hostSigner)
	cfg.AddHostKey(rsaSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			go serveSigner(conn, cfg, answer, status, mode)
		}
	}()

	return ln.Addr().String(), []ssh.PublicKey{hostSigner.PublicKey(), rsaSigner.PublicKey()}
}

func serveSigner(conn net.Conn, cfg *ssh.ServerConfig, answer string, status uint32, mode serveMode) {
	_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}

	go ssh.DiscardRequests(reqs)

	for nc := range chans {
		ch, requests, err := nc.Accept()
		if err != nil {
			return
		}

		for req := range requests {
			if req.Type != "shell" || mode == serveNoShell {
				_ = req.Reply(false, nil)
				continue
			}

			_ = req.Reply(true, nil)
			csr, _ := io.ReadAll(ch)

			if mode == serveHang {
				time.Sleep(3 * time.Second)
				return
			}

			if strings.Contains(string(csr), "CERTIFICATE REQUEST") && status == 0 {
				_, _ = io.WriteString(ch, answer)
			} else {
				_, _ = io.WriteString(ch.Stderr(), "FAIL: CSR subject is outside the policy\n")
			}

			st := make([]byte, 4)
			binary.BigEndian.PutUint32(st, status)
			_, _ = ch.SendRequest("exit-status", false, st)
			_ = ch.Close()
		}
	}
}

func TestSSHSigner(t *testing.T) {
	_, clientPriv, _ := ed25519.GenerateKey(rand.Reader)
	clientSigner, _ := ssh.NewSignerFromKey(clientPriv)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	addr, hostKey := sshServer(t, clientSigner.PublicKey(), "-----BEGIN CERTIFICATE-----\n", 0)
	s := &SSHSigner{Addr: addr, User: "enroll", Key: clientSigner, HostKey: hostKey, DialTimeout: 5 * time.Second}

	out, err := s.Sign(ctx, []byte("-----BEGIN CERTIFICATE REQUEST-----\n"))
	if err != nil || !strings.HasPrefix(string(out), "-----BEGIN CERTIFICATE-----") {
		t.Fatalf("sign: %q %v", out, err)
	}

	// A different host key is refused before anything is sent.
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	otherSigner, _ := ssh.NewSignerFromKey(otherPriv)
	s.HostKey = otherSigner.PublicKey()

	if _, err := s.Sign(ctx, []byte("csr")); err == nil || !strings.Contains(err.Error(), "host key") {
		t.Fatalf("wrong host key: %v", err)
	}

	addr, hostKey = sshServer(t, clientSigner.PublicKey(), "", 1)
	s = &SSHSigner{Addr: addr, User: "enroll", Key: clientSigner, HostKey: hostKey, DialTimeout: 5 * time.Second}

	if _, err := s.Sign(ctx, []byte("-----BEGIN CERTIFICATE REQUEST-----\n")); err == nil || !strings.Contains(err.Error(), "outside the policy") {
		t.Fatalf("signer refusal: %v", err)
	}
}

func TestSSHSignerEdges(t *testing.T) {
	_, clientPriv, _ := ed25519.GenerateKey(rand.Reader)
	clientSigner, _ := ssh.NewSignerFromKey(clientPriv)

	// The server has ed25519 and RSA host keys; RSA is pinned.
	addr, keys := sshServerKeys(t, clientSigner.PublicKey(), "-----BEGIN CERTIFICATE-----\n", 0, serveNormal)
	s := &SSHSigner{Addr: addr, User: "enroll", Key: clientSigner, HostKey: keys[1], DialTimeout: 5 * time.Second}

	if _, err := s.Sign(context.Background(), []byte("-----BEGIN CERTIFICATE REQUEST-----\n")); err != nil {
		t.Fatalf("pinned RSA host key: %v", err)
	}

	// Same server, ed25519 pinned (the cluster's normal case).
	s.HostKey = keys[0]
	if _, err := s.Sign(context.Background(), []byte("-----BEGIN CERTIFICATE REQUEST-----\n")); err != nil {
		t.Fatalf("pinned ed25519 host key: %v", err)
	}

	// A refused shell request is an error, not an empty answer.
	addr, keys = sshServerKeys(t, clientSigner.PublicKey(), "", 0, serveNoShell)
	s = &SSHSigner{Addr: addr, User: "enroll", Key: clientSigner, HostKey: keys[0], DialTimeout: 5 * time.Second}

	if out, err := s.Sign(context.Background(), []byte("-----BEGIN CERTIFICATE REQUEST-----\n")); err == nil {
		t.Fatalf("refused shell accepted: %q", out)
	}

	// An oversized answer is refused.
	addr, keys = sshServerKeys(t, clientSigner.PublicKey(), strings.Repeat("x", maxAnswer+1), 0, serveNormal)
	s = &SSHSigner{Addr: addr, User: "enroll", Key: clientSigner, HostKey: keys[0], DialTimeout: 5 * time.Second}

	if _, err := s.Sign(context.Background(), []byte("-----BEGIN CERTIFICATE REQUEST-----\n")); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized answer: %v", err)
	}

	// A signer that never finishes is cut off at the deadline.
	addr, keys = sshServerKeys(t, clientSigner.PublicKey(), "", 0, serveHang)
	s = &SSHSigner{Addr: addr, User: "enroll", Key: clientSigner, HostKey: keys[0], DialTimeout: time.Second}

	start := time.Now()
	if _, err := s.Sign(context.Background(), []byte("-----BEGIN CERTIFICATE REQUEST-----\n")); err == nil || time.Since(start) > 2500*time.Millisecond {
		t.Fatalf("hanging signer: %v after %v", err, time.Since(start))
	}
}

// fakeAPI is a Kubernetes API with one Secret at resourceVersion 42; it
// records the replaced object.
func fakeAPI(t *testing.T) (*SecretStore, *map[string]any) {
	t.Helper()

	put := map[string]any{}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" || r.URL.Path != "/api/v1/namespaces/auth/secrets/mx" {
			w.WriteHeader(http.StatusForbidden)
			return
		}

		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"kind":"Secret","type":"kubernetes.io/tls","metadata":{"name":"mx","resourceVersion":"42","annotations":{"a":"b","kubectl.kubernetes.io/last-applied-configuration":"{\"stringData\":{\"tls.key\":\"old\"}}"}},"data":{"tls.crt":"`+
				base64.StdEncoding.EncodeToString([]byte("old"))+`"}}`)

			return
		}

		_ = json.NewDecoder(r.Body).Decode(&put)
		if put["metadata"].(map[string]any)["resourceVersion"] != "42" {
			w.WriteHeader(http.StatusConflict)
		}
	}))
	t.Cleanup(srv.Close)

	return &SecretStore{Server: srv.URL, Client: srv.Client(), Namespace: "auth", Name: "mx",
		Token: func() (string, error) { return "token", nil }}, &put
}

func TestSecretStore(t *testing.T) {
	s, put := fakeAPI(t)

	data, version, err := s.Load(context.Background())
	if err != nil || string(data["tls.crt"]) != "old" {
		t.Fatalf("load: %v %v", data, err)
	}

	data["tls.crt"] = []byte("new")
	if err := s.Save(context.Background(), data, version); err != nil {
		t.Fatal(err)
	}

	obj := *put
	annotations := obj["metadata"].(map[string]any)["annotations"].(map[string]any)

	if _, kept := annotations[lastAppliedAnnotation]; kept {
		t.Fatal("last-applied annotation with old key material kept")
	}

	if obj["type"] != "kubernetes.io/tls" || annotations["a"] != "b" || obj["data"].(map[string]any)["tls.crt"] != base64.StdEncoding.EncodeToString([]byte("new")) {
		t.Fatalf("replaced object: %v", obj)
	}

	stale := strings.Replace(version, `"42"`, `"41"`, 1)
	if err := s.Save(context.Background(), data, stale); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("stale version: %v", err)
	}

	s.Token = func() (string, error) { return "wrong", nil }
	if _, _, err := s.Load(context.Background()); err == nil {
		t.Fatal("forbidden answer accepted")
	}
}
