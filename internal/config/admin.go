package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Admin configures the internal admin API (roadmap stage 8). Both
// listeners are off unless configured. The Unix socket admits only
// processes running as the server's own user (peer credentials); mutual
// TLS admits clients whose certificate identity is listed in Clients.
type Admin struct {
	// Socket is the path of the Unix socket ("" disables it).
	Socket string `mapstructure:"socket" env:"ADMIN_SOCKET"`
	// SocketRoles are the roles of local callers on the socket.
	SocketRoles []string `mapstructure:"socket_roles" validate:"dive,oneof=diagnose deliver deliver:noenforce"`
	// Addr is the mutual-TLS listener ("" disables it).
	Addr string `mapstructure:"addr" env:"ADMIN_ADDR"`
	// TLSCert, TLSKey and ClientCA are files; they are re-read when they
	// change (certificate rotation without a restart).
	TLSCert  string `mapstructure:"tls_cert" env:"ADMIN_TLS_CERT"`
	TLSKey   string `mapstructure:"tls_key" env:"ADMIN_TLS_KEY"`
	ClientCA string `mapstructure:"client_ca" env:"ADMIN_CLIENT_CA"`
	// Clients map certificate identities to roles.
	Clients []AdminClient `mapstructure:"clients" validate:"dive"`
}

// AdminClient grants roles to the client certificate whose subject
// common name or one of whose DNS or URI SANs equals Identity.
type AdminClient struct {
	Identity string   `mapstructure:"identity" validate:"required"`
	Roles    []string `mapstructure:"roles" validate:"required,min=1,dive,oneof=diagnose deliver deliver:noenforce"`
}

// Validate checks the listener settings and refuses settings without
// effect (a listener nobody may use, TLS material without a listener).
func (c Admin) Validate() error {
	if err := c.validateSocket(); err != nil {
		return err
	}

	if c.Addr == "" {
		if c.TLSCert != "" || c.TLSKey != "" || c.ClientCA != "" || len(c.Clients) > 0 {
			return fmt.Errorf("admin TLS settings and clients require admin.addr")
		}

		return nil
	}

	return c.validateTLS()
}

// validateSocket checks the Unix socket settings.
func (c Admin) validateSocket() error {
	if c.Socket != "" && !filepath.IsAbs(c.Socket) {
		return fmt.Errorf("admin.socket must be an absolute path")
	}

	if (c.Socket == "") != (len(c.SocketRoles) == 0) {
		return fmt.Errorf("admin.socket and admin.socket_roles must be set together")
	}

	return nil
}

// validateTLS checks the mutual-TLS listener settings.
func (c Admin) validateTLS() error {
	if err := checkListener(c.Addr); err != nil {
		return fmt.Errorf("admin.addr: %w", err)
	}

	if c.TLSCert == "" || c.TLSKey == "" || c.ClientCA == "" || len(c.Clients) == 0 {
		return fmt.Errorf("admin.addr requires tls_cert, tls_key, client_ca and clients")
	}

	seen := map[string]bool{}

	for _, cl := range c.Clients {
		id := strings.ToLower(cl.Identity)
		if seen[id] {
			return fmt.Errorf("duplicate admin client identity")
		}

		seen[id] = true
	}

	return nil
}
