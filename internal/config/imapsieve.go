package config

import (
	"fmt"
	"net/url"
	"strings"
)

// IMAPSieve configures RFC 6785 IMAPSieve: user scripts triggered by
// mailbox events and the operator learning rules.
type IMAPSieve struct {
	Enabled        bool     `mapstructure:"enabled" env:"IMAPSIEVE_ENABLED"`
	UserScripts    bool     `mapstructure:"user_scripts" env:"IMAPSIEVE_USER_SCRIPTS"`
	ManageSieveURL string   `mapstructure:"managesieve_url" env:"IMAPSIEVE_MANAGESIEVE_URL"`
	TimeoutMS      int64    `mapstructure:"timeout_ms" env:"IMAPSIEVE_TIMEOUT_MS" validate:"gt=0,lte=10000"`
	MaxParallel    int64    `mapstructure:"max_parallel" env:"IMAPSIEVE_MAX_PARALLEL" validate:"gt=0,lte=64"`
	Learning       Learning `mapstructure:"learning"`
}

// Learning are the operator rules that hand copied or moved messages to
// an external program (spam/ham training). The server knows no
// classifier: each rule names a program from extprograms.programs, which
// gets the message on stdin and reports with its exit code (0 done,
// 75 try again later, anything else failed for good).
type Learning struct {
	Enabled         bool           `mapstructure:"enabled" env:"LEARNING_ENABLED"`
	MaxAttempts     int64          `mapstructure:"max_attempts" env:"LEARNING_MAX_ATTEMPTS" validate:"gt=0,lte=20"`
	MaxMessageBytes int64          `mapstructure:"max_message_bytes" env:"LEARNING_MAX_MESSAGE_BYTES" validate:"gt=0,lte=67108864"`
	Rules           []LearningRule `mapstructure:"rules" validate:"dive"`
}

// LearningRule selects COPY/MOVE events by special-use flags and names the
// program that learns from the message.
type LearningRule struct {
	Name                     string   `mapstructure:"name" validate:"required"`
	Operations               []string `mapstructure:"operations" validate:"required,min=1,dive,oneof=COPY MOVE"`
	SourceSpecialUse         string   `mapstructure:"source_special_use"`
	SourceNotSpecialUse      string   `mapstructure:"source_not_special_use"`
	DestinationSpecialUse    string   `mapstructure:"destination_special_use"`
	DestinationNotSpecialUse []string `mapstructure:"destination_not_special_use"`
	// Program is the extprograms entry to run for the message.
	Program string `mapstructure:"program" validate:"required"`
}

// Validate checks the cross-field rules the struct tags cannot express.
func (c IMAPSieve) Validate() error {
	if !c.Enabled && c.Learning.Enabled {
		return fmt.Errorf("learning requires imapsieve.enabled")
	}

	if c.Enabled && c.UserScripts && !validManageSieveURL(c.ManageSieveURL) {
		return fmt.Errorf("imapsieve.managesieve_url must be a sieve server URL")
	}

	if c.Learning.Enabled && len(c.Learning.Rules) == 0 {
		return fmt.Errorf("learning requires explicit rules")
	}

	names := map[string]bool{}
	for _, r := range c.Learning.Rules {
		if names[r.Name] {
			return fmt.Errorf("duplicate learning rule name")
		}

		names[r.Name] = true
		if err := r.validateSpecialUse(); err != nil {
			return err
		}
	}

	return nil
}

// validManageSieveURL reports whether s is a bare sieve://host[:port] URL.
func validManageSieveURL(s string) bool {
	u, e := url.Parse(s)
	if e != nil || u.Scheme != "sieve" || u.Host == "" || u.User != nil {
		return false
	}

	return u.RawQuery == "" && u.Fragment == "" && u.Path == ""
}

// validateSpecialUse checks that all special-use values begin with a
// backslash.
func (r LearningRule) validateSpecialUse() error {
	for _, v := range append([]string{r.SourceSpecialUse, r.SourceNotSpecialUse, r.DestinationSpecialUse}, r.DestinationNotSpecialUse...) {
		if v != "" && !strings.HasPrefix(v, `\`) {
			return fmt.Errorf("learning special-use values must begin with a backslash")
		}
	}

	return nil
}
