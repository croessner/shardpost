// Command ucm-cert-renew keeps an LDAP client certificate of the UCM
// private PKI in a Kubernetes Secret current (see package renew). It
// is meant to run as a CronJob; everything is configured by environment:
//
//	UCM_RENEW_SUBJECT        ordered subject, e.g. C=DE,DC=de,DC=roessner-net,ST=Hessen,L=Alsfeld,O=R.N.S.,OU=Mail,CN=x,emailAddress=y
//	UCM_RENEW_SECRET         Secret to maintain (must exist; kubernetes.io/tls)
//	UCM_RENEW_NAMESPACE      its namespace (default: the pod's)
//	UCM_RENEW_BEFORE         renew when less validity remains (default 1080h = 45 days)
//	UCM_RENEW_ROOT_FILE      pinned root certificate (PEM)
//	UCM_RENEW_ISSUER_FILE    pinned client-authentication issuing CA (PEM)
//	UCM_RENEW_CA_KEY         also store the root under this Secret key (optional)
//	UCM_RENEW_SSH_ADDR       signer host:port
//	UCM_RENEW_SSH_USER       enrollment account
//	UCM_RENEW_SSH_KEY_FILE   SSH private key of the account
//	UCM_RENEW_SSH_HOST_KEY_FILE  the signer's host key (authorized_keys format)
//	UCM_RENEW_TIMEOUT        whole run (default 2m)
//
// Exit 0: certificate fresh or renewed; 1: failure (the CronJob retries).
package main

import (
	"context"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"time"

	"golang.org/x/crypto/ssh"

	"ucm-cert-renew/renew"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(log); err != nil {
		log.Error("client certificate renewal failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, timeout, err := configure(log)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	res, err := cfg.Run(ctx)
	if err != nil {
		return err
	}

	log.Info("client certificate checked", "cn", cfg.Subject.CommonName(), "renewed", res.Renewed,
		"reason", res.Reason, "not_after", res.NotAfter.UTC().Format(time.RFC3339))

	return nil
}

// configure builds the renewal from the environment.
func configure(log *slog.Logger) (*renew.Config, time.Duration, error) {
	e := env{}

	subject, err := renew.ParseSubject(e.need("UCM_RENEW_SUBJECT"))
	if err != nil {
		return nil, 0, err
	}

	root, issuer := e.cert("UCM_RENEW_ROOT_FILE"), e.cert("UCM_RENEW_ISSUER_FILE")
	before, timeout := e.duration("UCM_RENEW_BEFORE", 1080*time.Hour), e.duration("UCM_RENEW_TIMEOUT", 2*time.Minute)
	signer := &renew.SSHSigner{Addr: e.need("UCM_RENEW_SSH_ADDR"), User: e.need("UCM_RENEW_SSH_USER"),
		Key: e.sshKey("UCM_RENEW_SSH_KEY_FILE"), HostKey: e.hostKey("UCM_RENEW_SSH_HOST_KEY_FILE"), DialTimeout: 15 * time.Second}

	if e.err != nil {
		return nil, 0, e.err
	}

	store, err := renew.NewInClusterStore(os.Getenv("UCM_RENEW_NAMESPACE"), e.need("UCM_RENEW_SECRET"))
	if err != nil || e.err != nil {
		return nil, 0, fmt.Errorf("secret store: %w", firstErr(err, e.err))
	}

	return &renew.Config{Subject: subject, Root: root, Issuer: issuer, RenewBefore: before,
		CAKey: os.Getenv("UCM_RENEW_CA_KEY"), Signer: signer, Store: store, Log: log}, timeout, nil
}

// env reads settings and keeps the first error.
type env struct{ err error }

func (e *env) fail(format string, a ...any) {
	if e.err == nil {
		e.err = fmt.Errorf(format, a...)
	}
}

func (e *env) need(name string) string {
	v := os.Getenv(name)
	if v == "" {
		e.fail("%s is not set", name)
	}

	return v
}

func (e *env) file(name string) []byte {
	path := e.need(name)
	if path == "" {
		return nil
	}

	b, err := os.ReadFile(path)
	if err != nil {
		e.fail("%s: %v", name, err)
	}

	return b
}

func (e *env) cert(name string) *x509.Certificate {
	c, err := renew.LoadCertificate(e.file(name))
	if err != nil {
		e.fail("%s: %v", name, err)
	}

	return c
}

func (e *env) duration(name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}

	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		e.fail("%s must be a positive duration", name)
	}

	return d
}

func (e *env) sshKey(name string) ssh.Signer {
	k, err := ssh.ParsePrivateKey(e.file(name))
	if err != nil {
		e.fail("%s: %v", name, err)
	}

	return k
}

func (e *env) hostKey(name string) ssh.PublicKey {
	k, _, _, _, err := ssh.ParseAuthorizedKey(e.file(name))
	if err != nil {
		e.fail("%s: %v", name, err)
	}

	return k
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}

	return nil
}
