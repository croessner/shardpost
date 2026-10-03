package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"shardpost/internal/cephstore"
	"shardpost/internal/config"
	"shardpost/internal/extprograms"
	"shardpost/internal/model"
	"shardpost/internal/version"
)

// runRunner runs "shardpost runner" (stage B18b) until ctx ends: the
// background workers of a server without its listeners. It runs the
// job runner with the expiry and fts_due workers, the upload expiry,
// the delayed submission worker and the learning worker, registers
// with the role runner, and serves the health and metrics endpoints.
// It opens no protocol listener and no admin listener: the admin API
// (the jobs API, the directory repair, the import reshard) is a
// server's, and diagnosis runs through a server pod. It reads the
// servers' configmap and leaves the TLS, JMAP, Sieve listener and
// authentication settings unread.
func runRunner(ctx context.Context, log *slog.Logger, settings *config.Config, store *cephstore.Store, programs *extprograms.Runner) error {
	if settings.Admin.Socket != "" || settings.Admin.Addr != "" {
		log.Info("runner: the admin listeners of the configuration are not opened; the admin API is a server's",
			"admin_socket", settings.Admin.Socket, "admin_addr", settings.Admin.Addr)
	}

	health := newHTTPServer(settings, store)

	go func() {
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("runner: http listener", "err", err)
		}
	}()

	defer func() { _ = health.Close() }()

	_, stopRegistration, err := startRegistration(ctx, log, store, model.RoleRunner, nil, nil)
	if err != nil {
		return err
	}
	defer stopRegistration()

	w, err := startRunnerWorkers(ctx, log, settings, store, programs)
	if err != nil {
		return err
	}
	defer w.stop()

	log.Info("runner running", "http", settings.Server.HTTPAddr, "jobs", settings.Jobs.Enabled,
		"learning", settings.IMAPSieve.Enabled && settings.IMAPSieve.Learning.Enabled, "version", version.Version)

	<-ctx.Done()

	log.Info("runner shutting down")

	return nil
}

// startRunnerWorkers starts every worker of a runner; stop ends them,
// the last started first.
func startRunnerWorkers(ctx context.Context, log *slog.Logger, settings *config.Config, store *cephstore.Store,
	programs *extprograms.Runner,
) (*background, error) {
	b := &background{}

	_, stopLearner, err := startLearning(ctx, log, settings, store, programs, true)
	if err != nil {
		return nil, err
	}

	b.stops = append(b.stops, stopLearner, startUploadExpiry(ctx, store))

	sendq, stopSendq, err := startSubmissionQueue(ctx, log, settings, store, true)
	if err != nil {
		b.stop()
		return nil, err
	}

	b.sendq, b.stops = sendq, append(b.stops, stopSendq)

	if err := b.startJobs(ctx, log, settings, store, true); err != nil {
		b.stop()
		return nil, err
	}

	return b, nil
}
