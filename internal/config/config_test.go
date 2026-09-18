package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func configFile(t *testing.T, body string) string {
	t.Helper()

	p := filepath.Join(t.TempDir(), "server.yaml")
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}

	return p
}
func TestFileEnvDefaults(t *testing.T) {
	p := configFile(t, "server:\n  imap_addr: ':2143'\n  insecure_auth: false\nceph:\n  user: other\n")

	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}

	if c.Server.ImapAddr != ":2143" || c.Server.InsecureAuth || c.Server.Pop3Addr != ":1110" || c.Rook.ClientSecret != "rook-ceph-client-other" {
		t.Fatalf("wrong configuration merge")
	}

	t.Setenv("IMAP_ADDR", ":3143")

	c, err = Load(p)
	if err != nil || c.Server.ImapAddr != ":3143" {
		t.Fatalf("env precedence: %v", err)
	}
}
func TestRejectInvalid(t *testing.T) {
	for _, body := range []string{"server:\n  typo: true", "server:\n  max_conns_per_ip: -1", "server:\n  imap_addr: 'x'", "tls:\n  cert: 'cert.pem'", "telemetry:\n  sample_ratio: 2"} {
		t.Run(body, func(t *testing.T) {
			if _, err := Load(configFile(t, body)); err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
}
func TestInvalidEnv(t *testing.T) {
	for key, value := range map[string]string{"MAX_CONNS_PER_IP": "wrong", "INSECURE_AUTH": "yes", "MBOX_SHARD_SIZE": "4294967296"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, value)

			if _, err := Load(""); err == nil {
				t.Fatal("accepted invalid env")
			}
		})
	}
}
func TestSecretsFailClosed(t *testing.T) {
	t.Setenv("MASTER_PASSWORD", "do-not-leak")
	t.Setenv("MASTER_PASSWORD_FILE", "/definitely/missing")

	_, err := Load("")
	if err == nil || strings.Contains(err.Error(), "do-not-leak") {
		t.Fatal("secret file fallback or disclosure")
	}

	p := configFile(t, " file-secret \n")
	t.Setenv("MASTER_PASSWORD_FILE", p)

	c, err := Load("")
	if err != nil || c.Server.MasterPassword != "file-secret" {
		t.Fatalf("secret file: %v", err)
	}
}
func TestStrictYAMLTypes(t *testing.T) {
	for _, body := range []string{"server:\n  quota_msgs: 0.5", "server:\n  insecure_auth: 1"} {
		if _, err := Load(configFile(t, body)); err == nil {
			t.Fatal("weak YAML conversion")
		}
	}
}
func TestDecimalEnv(t *testing.T) {
	t.Setenv("QUOTA_MSGS", "010")

	c, err := Load("")
	if err != nil || c.Server.QuotaMsgs != 10 {
		t.Fatalf("decimal parsing: %v", err)
	}
}

func TestExternalProgramYAML(t *testing.T) {
	body := `imapsieve:
  enabled: true
  user_scripts: false
  learning:
    enabled: true
    rules:
      - name: junk
        operations: [MOVE]
        program: spam
extprograms:
  bin_dir: /usr/local/lib/programs
  programs:
    spam:
      command: learn.py
      kinds: [learning]
      env: {LEARN_BACKEND: rspamd, LEARN_TOKEN_FILE: /run/secrets/token}
`

	c, err := Load(configFile(t, body))
	if err != nil {
		t.Fatal(err)
	}

	if c.ExtPrograms.Programs["spam"].Env["LEARN_BACKEND"] != "rspamd" {
		t.Fatal("Viper lost environment names")
	}

	if _, err = Load(configFile(t, strings.Replace(body, "kinds: [learning]", "kinds: [execute]", 1))); err == nil {
		t.Fatal("unusable learning program accepted")
	}
}

func TestLoggingLevels(t *testing.T) {
	c, err := Load("")
	if err != nil || c.Logging.Level != "info" {
		t.Fatalf("default: %v", err)
	}

	p := configFile(t, "logging:\n  level: debug\n")

	c, err = Load(p)
	if err != nil || c.Logging.Level != "debug" {
		t.Fatalf("yaml: %v", err)
	}

	for _, level := range []string{"debug", "info", "warn", "error"} {
		t.Setenv("LOG_LEVEL", level)

		c, err = Load(p)
		if err != nil || c.Logging.Level != level {
			t.Fatalf("env %s: %v", level, err)
		}
	}

	t.Setenv("LOG_LEVEL", "password-sentinel")

	if _, err = Load(p); err == nil || strings.Contains(err.Error(), "password-sentinel") {
		t.Fatal("invalid level not safely rejected")
	}
}

func TestTelemetryServiceIdentity(t *testing.T) {
	c, err := Load("")
	if err != nil || c.Telemetry.ServiceName != "shardpost" {
		t.Fatalf("new default: %v", err)
	}

	p := configFile(t, "telemetry:\n  service_name: legacy-service\n")

	c, err = Load(p)
	if err != nil || c.Telemetry.ServiceName != "legacy-service" {
		t.Fatalf("legacy contract: %v", err)
	}

	t.Setenv("OTEL_SERVICE_NAME", "custom-service")

	c, err = Load(p)
	if err != nil || c.Telemetry.ServiceName != "custom-service" {
		t.Fatalf("override: %v", err)
	}
}
