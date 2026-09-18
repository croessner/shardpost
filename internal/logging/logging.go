// Package logging defines the structured operational logging contract.
package logging

import (
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/google/uuid"
)

// New returns a JSON logger writing to w at the given level (debug, info,
// warn or error); unknown levels fall back to info.
func New(w io.Writer, level string) *slog.Logger {
	var l slog.Level
	// Configuration validation rejects unsupported levels before this call.
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}

	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: l}))
}

// Session uses the protocol's effective address (after trusted PROXY parsing).
// Never attach request structs, command arguments, SASL data or message bodies.
func Session(log *slog.Logger, c net.Conn) *slog.Logger {
	if log == nil {
		log = slog.Default()
	}

	return log.With("session_id", uuid.NewString(), "remote", c.RemoteAddr().String(), "local", c.LocalAddr().String())
}

// Begin logs the opening of a connection and returns a function that logs
// its close together with the connection duration.
func Begin(log *slog.Logger, tls bool) func() {
	start := time.Now()

	log.Info("connection opened", "event", "connection_open", "tls", tls)

	return func() {
		log.Info("connection closed", "event", "connection_close", "duration_ms", time.Since(start).Milliseconds())
	}
}
