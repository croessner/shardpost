package main

import (
	"shardpost/internal/config"
	"shardpost/internal/extprograms"
	"shardpost/internal/imapsieve"
	"shardpost/internal/metrics"
	"shardpost/internal/model"
)

// The processes that serve /metrics create the series of their event
// metrics when they start (metrics.Init) and say which parts their
// configuration switches on (imapceph_feature_enabled): a dashboard
// then tells "nothing happened" from "switched off" from "not
// measured". The short-lived commands have no listener and create
// nothing.

// workerFeatures are the parts of a process with or without workers
// (a server with workers.run, the runner), as the configuration names
// them. queue: the submission queue exists.
func workerFeatures(settings *config.Config, run, queue bool) metrics.Features {
	source := model.OrphanSource(settings.Scrub.OrphanSource)
	submission := settings.JMAP.Submission

	return metrics.Features{
		Workers: run,
		Jobs:    settings.Jobs.Enabled && run,
		// As startSubmissionQueue starts the worker: with workers, a
		// relay and jmap.submission.delayed.worker.
		DelayedSubmission: queue && run && submission.Enabled() && submission.Delayed.Worker,
		ScrubShadow:       source == model.OrphanSourceShadow,
		ScrubListing:      source == model.OrphanSourceShadow || source == model.OrphanSourceListing,
		PurgeListing:      settings.Purge.Listing,
	}
}

// learningEnabled reports whether IMAPSieve learning is configured.
func learningEnabled(settings *config.Config) bool {
	return settings.IMAPSieve.Enabled && settings.IMAPSieve.Learning.Enabled
}

// publishFeatures creates the series of the parts f names and sets the
// feature gauge for them and for the parts beside metrics.Features.
func publishFeatures(settings *config.Config, f metrics.Features, programs *extprograms.Runner, more map[string]bool) {
	metrics.Init(f)

	learning := learningEnabled(settings)
	enabled := map[string]bool{
		metrics.FeatureJobs:              f.Jobs,
		metrics.FeatureLearning:          learning,
		metrics.FeatureDelayedSubmission: f.DelayedSubmission,
		metrics.FeatureScrubShadow:       f.ScrubShadow,
		metrics.FeatureScrubListing:      f.ScrubListing,
		metrics.FeaturePurgeListing:      f.PurgeListing,
		metrics.FeatureExtPrograms:       programs.Enabled(),
	}

	for name, on := range more {
		enabled[name] = on
	}

	metrics.SetFeatures(enabled)
	imapsieve.InitMetrics(f.Server && settings.IMAPSieve.Enabled, learning, f.Workers)
	programs.InitMetrics()
}

// serveMetrics is publishFeatures for a server.
func serveMetrics(settings *config.Config, bg *background, programs *extprograms.Runner, authCache, quotaEvents bool) {
	f := workerFeatures(settings, settings.Workers.Run, bg.sendq != nil)
	f.Server = true
	f.DelayedOffered = bg.sendq != nil && settings.JMAP.Enabled() && settings.JMAP.Submission.Delayed.Enabled()
	f.LMTP = settings.Server.LmtpAddr != ""
	f.AuthCache = authCache

	publishFeatures(settings, f, programs, map[string]bool{
		metrics.FeatureLMTP:           f.LMTP,
		metrics.FeatureAuthCache:      authCache,
		metrics.FeatureJMAP:           settings.JMAP.Enabled(),
		metrics.FeatureJMAPPerIPLimit: settings.JMAP.Enabled() && settings.JMAP.MaxConcurrentPerIP > 0,
		metrics.FeatureQuotaEvents:    quotaEvents,
	})
}

// runnerMetrics is publishFeatures for the job runner.
func runnerMetrics(settings *config.Config, bg *background, programs *extprograms.Runner) {
	publishFeatures(settings, workerFeatures(settings, true, bg.sendq != nil), programs, nil)
}
