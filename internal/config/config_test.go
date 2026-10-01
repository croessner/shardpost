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

	// Authentication without TLS is opt-in.
	if c, err = Load(configFile(t, "server:\n  imap_addr: ':2143'\n")); err != nil || c.Server.InsecureAuth {
		t.Fatalf("insecure_auth must default to false: %v", err)
	}
}
// TestUserAttributesOptIn: personal span attributes are off unless the
// operator switches them on.
func TestUserAttributesOptIn(t *testing.T) {
	p := configFile(t, "server:\n  imap_addr: ':2143'\n")

	c, err := Load(p)
	if err != nil || c.Telemetry.UserAttributes {
		t.Fatalf("telemetry.user_attributes must default to false: %v", err)
	}

	t.Setenv("TELEMETRY_USER_ATTRIBUTES", "true")

	if c, err = Load(p); err != nil || !c.Telemetry.UserAttributes {
		t.Fatalf("TELEMETRY_USER_ATTRIBUTES: %v", err)
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

// TestDeliveryHeaderSettings: Return-Path and Delivered-To are on by
// default, Delivered-To takes final or none, and anything else fails.
func TestDeliveryHeaderSettings(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}

	if !c.Server.LmtpReturnPath || c.Server.LmtpDeliveredTo != "final" {
		t.Fatalf("defaults: return path %v, delivered-to %q", c.Server.LmtpReturnPath, c.Server.LmtpDeliveredTo)
	}

	c, err = Load(configFile(t, "server:\n  lmtp_return_path: false\n  lmtp_delivered_to: none\n"))
	if err != nil || c.Server.LmtpReturnPath || c.Server.LmtpDeliveredTo != "none" {
		t.Fatalf("explicit values: %v", err)
	}

	for _, body := range []string{"server:\n  lmtp_delivered_to: original\n", "server:\n  lmtp_delivered_to: bogus\n", "server:\n  lmtp_return_path: maybe\n"} {
		if _, err := Load(configFile(t, body)); err == nil {
			t.Errorf("accepted %q", body)
		}
	}

	t.Setenv("LMTP_DELIVERED_TO", "bogus")

	if _, err := Load(""); err == nil {
		t.Error("accepted LMTP_DELIVERED_TO=bogus")
	}
}

// TestIntrospectionNeedsItsClient: a bearer mode that introspects needs
// the endpoint, and an introspection client needs its secret; either gap
// would otherwise surface as every bearer login failing at runtime.
func TestIntrospectionNeedsItsClient(t *testing.T) {
	for name, body := range map[string]string{
		"introspect without url": "nauthilus:\n  bearer_mode: introspect",
		"client without secret":  "nauthilus:\n  introspect_url: https://idp.example.org/oidc/introspect\n  introspect_client_id: mail",
		"no client at all":       "nauthilus:\n  introspect_url: https://idp.example.org/oidc/introspect",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(configFile(t, body)); err == nil {
				t.Fatal("accepted incomplete introspection settings")
			}
		})
	}

	for name, body := range map[string]string{
		"own client":     "nauthilus:\n  bearer_mode: introspect\n  introspect_url: https://idp.example.org/oidc/introspect\n  introspect_client_id: mail\n  introspect_client_secret: s",
		"service client": "nauthilus:\n  introspect_url: https://idp.example.org/oidc/introspect\n  oidc_client_id: svc\n  oidc_client_secret: s",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(configFile(t, body)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestJMAPShardSetTogether: a shard gate needs both the attribute and the
// value; one without the other would serve nobody or everybody silently.
func TestJMAPShardSetTogether(t *testing.T) {
	for _, body := range []string{"jmap:\n  shard: shardpost", "jmap:\n  shard_attribute: mailShard"} {
		if _, err := Load(configFile(t, body)); err == nil {
			t.Fatalf("accepted %q", body)
		}
	}

	c, err := Load(configFile(t, "jmap:\n  shard_attribute: mailShard\n  shard: shardpost"))
	if err != nil || c.JMAP.Shard != "shardpost" || c.TLS.ReloadSeconds != 60 {
		t.Fatalf("shard gate config: %v %+v", err, c)
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

// TestNamespacePrefixes: two namespaces must not name the same place,
// and neither may lie below INBOX, or one of them would be unreachable.
func TestNamespacePrefixes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		shared, pub   string
		wantRejection bool
	}{
		{"the defaults", "Shared/", "Public/", false},
		{"sharing off", "", "Public/", false},
		{"both off", "", "", false},
		{"the same name", "Shared/", "Shared/", true},
		{"one inside the other", "Shared/", "Shared/Public/", true},
		{"below INBOX", "INBOX/Shared/", "Public/", true},
		{"below inbox, lower case", "inbox/shared/", "Public/", true},
		{"a quote in the prefix", `Sh"ared/`, "Public/", true},
		{"a wildcard in the prefix", "Sh*ared/", "Public/", true},
		{"not ASCII", "Geteilt ü/", "Public/", true},
		{"the delimiter alone", "/", "Public/", true},
		{"an empty level", "Shared//", "Public/", true},
		{"a space is fine", "Other Users/", "Public/", false},
		{"more than one level", "Shared/Other/", "Public/", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{}
			c.Server.SharedPrefix, c.Server.PublicPrefix = tc.shared, tc.pub

			if err := c.checkNamespaces(); (err != nil) != tc.wantRejection {
				t.Errorf("shared %q public %q: %v", tc.shared, tc.pub, err)
			}
		})
	}
}

// TestAutocreateDefaultsAndChecks: without a configuration an account
// gets the five special-use mailboxes, and the validator refuses a list
// nobody could use.
func TestAutocreateDefaultsAndChecks(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}

	if len(c.Server.Autocreate) != 5 {
		t.Fatalf("default autocreate: %+v", c.Server.Autocreate)
	}

	for _, f := range c.Server.Autocreate {
		if f.SpecialUse == "" || !f.Subscribed() {
			t.Errorf("default %q: special-use %q, subscribed %v", f.Name, f.SpecialUse, f.Subscribed())
		}
	}

	no := false

	for _, tc := range []struct {
		name          string
		folders       []AutocreateFolder
		wantRejection bool
	}{
		{"the defaults", nil, false},
		{"a plain mailbox", []AutocreateFolder{{Name: "Projekte"}}, false},
		{"created but not subscribed", []AutocreateFolder{{Name: "Ablage", Subscribe: &no}}, false},
		{"no name", []AutocreateFolder{{Name: ""}}, true},
		{"twice", []AutocreateFolder{{Name: "Ablage"}, {Name: "Ablage"}}, true},
		{"below a namespace", []AutocreateFolder{{Name: "Shared/alice/Box"}}, true},
		{"a trailing separator", []AutocreateFolder{{Name: "Ablage/"}}, true},
		{"no special-use attribute", []AutocreateFolder{{Name: "Ablage", SpecialUse: "Archive"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{}
			cfg.Server.SharedPrefix, cfg.Server.PublicPrefix = "Shared/", "Public/"
			cfg.Server.Autocreate = tc.folders

			if err := cfg.checkAutocreate(); (err != nil) != tc.wantRejection {
				t.Errorf("%+v: %v", tc.folders, err)
			}
		})
	}
}

// TestAutocreateFromYAML: the list is read from the file as written,
// including an attribute and a mailbox that is created but not
// subscribed.
func TestAutocreateFromYAML(t *testing.T) {
	p := configFile(t, `
server:
  autocreate:
    - name: Entwürfe
      special_use: "\\Drafts"
    - name: Projekte
      subscribe: false
`)

	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}

	if len(c.Server.Autocreate) != 2 {
		t.Fatalf("read %+v", c.Server.Autocreate)
	}

	first, second := c.Server.Autocreate[0], c.Server.Autocreate[1]
	if first.Name != "Entwürfe" || first.SpecialUse != `\Drafts` || !first.Subscribed() {
		t.Errorf("first entry: %+v, subscribed %v", first, first.Subscribed())
	}

	if second.Name != "Projekte" || second.SpecialUse != "" || second.Subscribed() {
		t.Errorf("second entry: %+v, subscribed %v", second, second.Subscribed())
	}
}
