// Copyright © 2019 Circonus, Inc. <support@circonus.com>
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//

// Package cluster is the collection manager for a cluster
package cluster

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	cgm "github.com/circonus-labs/circonus-gometrics/v3"
	"github.com/circonus-labs/circonus-kubernetes-agent/internal/circonus"
	"github.com/circonus-labs/circonus-kubernetes-agent/internal/config"
	"github.com/circonus-labs/circonus-kubernetes-agent/internal/dns"
	"github.com/circonus-labs/circonus-kubernetes-agent/internal/events"
	"github.com/circonus-labs/circonus-kubernetes-agent/internal/ksm"
	"github.com/circonus-labs/circonus-kubernetes-agent/internal/ms"
	"github.com/circonus-labs/circonus-kubernetes-agent/internal/nodes"
	"github.com/circonus-labs/circonus-kubernetes-agent/internal/release"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
)

type Cluster struct {
	tlsConfig  *tls.Config
	cfg        config.Cluster
	check      *circonus.Check
	circCfg    config.Circonus
	logger     zerolog.Logger
	interval   time.Duration
	lastStart  *time.Time
	collectors []Collector
	running    bool
	sync.Mutex
}
type Collector interface {
	ID() string
	Collect(context.Context, *tls.Config, *time.Time)
}

func New(cfg config.Cluster, circCfg config.Circonus, parentLog zerolog.Logger) (*Cluster, error) {
	if cfg.Name == "" {
		return nil, errors.New("invalid cluster config (empty name)")
	}
	if cfg.BearerToken == "" && cfg.BearerTokenFile == "" {
		return nil, errors.New("invalid bearer credentials (empty)")
	}

	c := &Cluster{
		cfg:     cfg,
		circCfg: circCfg,
		logger:  parentLog.With().Str("pkg", "cluster").Str("cluster_name", cfg.Name).Logger(),
	}

	if c.cfg.BearerToken == "" && c.cfg.BearerTokenFile != "" {
		token, err := ioutil.ReadFile(c.cfg.BearerTokenFile)
		if err != nil {
			return nil, errors.Wrap(err, "bearer token file")
		}
		c.cfg.BearerToken = string(token)
	}
	c.logger.Debug().Str("token", c.cfg.BearerToken[0:8]+"...").Msg("using bearer token")

	if c.cfg.CAFile != "" {
		cert, err := ioutil.ReadFile(c.cfg.CAFile)
		if err != nil {
			return nil, errors.Wrap(err, "configuring k8s api tls")
		}
		cp := x509.NewCertPool()
		if !cp.AppendCertsFromPEM(cert) {
			return nil, errors.New("unable to add k8s api CA Certificate to x509 cert pool")
		}
		c.tlsConfig = &tls.Config{
			RootCAs: cp,
			// InsecureSkipVerify: true,
		}
		c.logger.Debug().Str("cert", c.cfg.CAFile).Msg("adding CA cert to TLS config")
	}

	d, err := time.ParseDuration(c.cfg.Interval)
	if err != nil {
		return nil, errors.Wrap(err, "invalid duration in cluster configuration")
	}
	c.interval = d
	c.logger.Debug().Str("interval", d.String()).Msg("using interval")

	// set check title if it has not been explicitly set by user
	if circCfg.Check.Title == "" {
		circCfg.Check.Title = fmt.Sprintf("%s /%s", cfg.Name, release.NAME)
	}

	if circCfg.Check.Target == "" {
		circCfg.Check.Target = strings.Replace(cfg.Name, " ", "_", -1)
	}
	check, err := circonus.NewCheck(c.logger, &circCfg)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to initialize circonus for cluster (%s)", cfg.Name)
	}
	c.check = check

	if c.cfg.EnableNodes {
		// node metrics, as well as, pod and container metrics (both optional)
		collector, err := nodes.New(&c.cfg, c.logger, c.check)
		if err != nil {
			return nil, errors.Wrap(err, "initializing node collector")
		}
		c.collectors = append(c.collectors, collector)
	}

	if c.cfg.EnableKubeStateMetrics {
		// TODO: does this allow "watching"?
		collector, err := ksm.New(&c.cfg, c.logger, c.check)
		if err != nil {
			return nil, errors.Wrap(err, "initializing kube-state-metrics collector")
		}
		c.collectors = append(c.collectors, collector)
	}

	if c.cfg.EnableMetricServer {
		// TODO: does this allow "watching"?
		collector, err := ms.New(&c.cfg, c.logger, c.check)
		if err != nil {
			return nil, errors.Wrap(err, "initializing kube-state-metrics collector")
		}
		c.collectors = append(c.collectors, collector)
	}

	if c.cfg.EnableKubeDNSMetrics {
		collector, err := dns.New(&c.cfg, c.logger, c.check)
		if err != nil {
			return nil, errors.Wrap(err, "initializing kube-dns metrics collector")
		}
		c.collectors = append(c.collectors, collector)
	}

	if len(c.collectors) == 0 {
		return nil, errors.Errorf("no collectors enabled for cluster %s", c.cfg.Name)
	}

	return c, nil
}

func (c *Cluster) Start(ctx context.Context) error {
	// create a errgroup context based on ctx
	// if events enabled, create event watcher and add to errgroup
	// if >0 collectors, start collector goroutine and add to errgroup
	// errgroup wait

	var eventWatcher *events.Events
	if c.cfg.EnableEvents {
		// TODO: events needs to be a separate thing started
		//       in cluster.Start. It will not return if event
		//       'watching' works as a stream collector. Which
		//       means it does not need to be fired every
		//       cluster.Interval like everything else.
		ew, err := events.New(&c.cfg, c.logger, c.check)
		if err != nil {
			return errors.Wrap(err, "initializing events collector")
		}
		eventWatcher = ew
	}

	if len(c.collectors) == 0 && eventWatcher == nil {
		return errors.New("invalid cluster (zero collectors)")
	}

	if eventWatcher != nil {
		go eventWatcher.Start(ctx, c.tlsConfig)
	}

	if !c.check.ConcurrentSubmissions() {
		go c.check.Submitter(ctx)
	}

	c.logger.Info().Str("collection_interval", c.interval.String()).Time("next_collection", time.Now().Add(c.interval)).Msg("client started")

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			c.Lock()
			if c.lastStart != nil {
				elapsed := time.Since(*c.lastStart)
				if c.interval.Round(time.Second)-elapsed.Round(time.Second) > 2 {
					c.Unlock()
					c.logger.Warn().
						Str("last_start", c.lastStart.String()).
						Dur("elapsed", elapsed).
						Dur("interval", c.interval).
						Msg("interval not reached")
					continue
				}
			}
			if c.running {
				c.Unlock()
				c.logger.Warn().
					Str("started", c.lastStart.String()).
					Str("elapsed", time.Since(*c.lastStart).String()).
					Msg("collection in progress, not starting another")
				continue
			}

			start := time.Now()
			c.lastStart = &start
			c.running = true
			c.Unlock()

			// reset submit retries metric
			c.check.SetCounter("collect_submit_retries", cgm.Tags{cgm.Tag{Category: "source", Value: release.NAME}}, 0)

			go func() {
				var wg sync.WaitGroup
				wg.Add(len(c.collectors))
				for _, collector := range c.collectors {
					if collector.ID() == "events" {
						continue
					}
					go func(collector Collector) {
						collector.Collect(ctx, c.tlsConfig, &start)
						wg.Done()
					}(collector)
				}
				wg.Wait()

				cstats := c.check.SubmitStats()
				c.check.ResetSubmitStats()
				dur := time.Since(start)

				baseStreamTags := cgm.Tags{
					cgm.Tag{Category: "cluster", Value: c.cfg.Name},
					cgm.Tag{Category: "source", Value: release.NAME},
				}
				c.check.AddText("collect_agent", baseStreamTags, release.NAME+"_"+release.VERSION)
				c.check.AddGauge("collect_metrics", baseStreamTags, cstats.Metrics)
				c.check.AddGauge("collect_ngr", baseStreamTags, uint64(runtime.NumGoroutine()))

				{
					var streamTags cgm.Tags
					streamTags = append(streamTags, baseStreamTags...)
					streamTags = append(streamTags, cgm.Tag{Category: "units", Value: "bytes"})
					c.check.AddGauge("collect_sent", streamTags, cstats.SentBytes)

					var ms runtime.MemStats
					runtime.ReadMemStats(&ms)
					c.check.AddGauge("collect_heap_alloc", streamTags, ms.HeapAlloc)
					c.check.AddGauge("collect_heap_released", streamTags, ms.HeapReleased)
					c.check.AddGauge("collect_stack_sys", streamTags, ms.StackSys)
					c.check.AddGauge("collect_other_sys", streamTags, ms.OtherSys)
					var mem syscall.Rusage
					if err := syscall.Getrusage(syscall.RUSAGE_SELF, &mem); err == nil {
						c.check.AddGauge("collect_max_rss", streamTags, uint64(mem.Maxrss*1024))
					} else {
						c.logger.Warn().Err(err).Msg("collecting rss from system")
					}
				}
				{
					var streamTags cgm.Tags
					streamTags = append(streamTags, baseStreamTags...)
					streamTags = append(streamTags, cgm.Tag{Category: "units", Value: "milliseconds"})
					c.check.AddGauge("collect_duration", streamTags, uint64(dur.Milliseconds()))
					c.check.AddGauge("collect_interval", streamTags, uint64(c.interval.Milliseconds()))
				}

				c.check.FlushCGM(ctx, &start)

				c.logger.Info().
					Interface("metrics_sent", cstats).
					Str("duration", dur.String()).
					Msg("collection complete")
				c.Lock()
				c.running = false
				c.Unlock()
			}()
		}
	}
}
