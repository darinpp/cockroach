// Copyright 2020 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package cliccl

import (
	"context"
	"net"
	"time"

	"github.com/cockroachdb/cockroach/pkg/ccl/sqlproxyccl"
	"github.com/cockroachdb/cockroach/pkg/cli"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

var options sqlproxyccl.ProxyOptions

func init() {
	startSQLProxyCmd := &cobra.Command{
		Use:   "start-proxy <basepath>",
		Short: "start-proxy host:port",
		Long: `Starts a SQL proxy.

This proxy accepts incoming connections and relays them to a backend server
determined by the arguments used.
`,
		RunE: cli.MaybeDecorateGRPCError(runStartSQLProxy),
		Args: cobra.NoArgs,
	}
	f := startSQLProxyCmd.Flags()
	f.StringVar(&options.Denylist, "denylist-file", "",
		"Denylist file to limit access to IP addresses and tenant ids.")
	f.StringVar(&options.ListenAddr, "listen-addr", "127.0.0.1:46257",
		"Listen address for incoming connections.")
	f.StringVar(&options.ListenCert, "listen-cert", "",
		"File containing PEM-encoded x509 certificate for listen address.")
	f.StringVar(&options.ListenKey, "listen-key", "",
		"File containing PEM-encoded x509 key for listen address.")
	f.StringVar(&options.MetricsAddress, "listen-metrics", "0.0.0.0:8080",
		"Listen address for incoming connections.")
	f.StringVar(&options.RoutingRule, "routing-rule", "",
		"Routing rule for incoming connections. Use '{{clusterName}}' for substitution.")
	f.BoolVar(&options.Verify, "verify", true,
		"If false, skip identity verification of the SQL pods. For testing only.")
	f.DurationVar(&options.RatelimitBaseDelay, "ratelimit-base-delay", 50*time.Millisecond,
		"Initial backoff after a failed login attempt. Set to 0 to disable rate limiting.")
	f.DurationVar(&options.ValidateAccessInterval, "validate-access-interval", 30*time.Second,
		"Time interval between validation that current connections are still valid.")
	f.DurationVar(&options.PollConfigInterval, "poll-config-interval", 30*time.Second,
		"Polling interval changes in config file.")
	cli.AddMTCommand(startSQLProxyCmd)
}

func runStartSQLProxy(*cobra.Command, []string) error {
	// TODO(chrisseto): Support graceful shutdown.
	// Our health check server is configured correctly but the proxy, itself,
	// is another story as it doesn't take any contexts to begin with.
	ctx := context.Background()

	proxyLn, err := net.Listen("tcp", options.ListenAddr)
	if err != nil {
		return err
	}
	defer func() { _ = proxyLn.Close() }()

	metricsLn, err := net.Listen("tcp", options.MetricsAddress)
	if err != nil {
		return err
	}
	defer func() { _ = metricsLn.Close() }()

	handler, err := sqlproxyccl.NewProxyHandler(ctx, options)
	if err != nil {
		return err
	}

	server := sqlproxyccl.NewServer(func(ctx context.Context, metrics *sqlproxyccl.Metrics, proxyConn *sqlproxyccl.Conn) error {
		return handler.Handle(ctx, metrics, proxyConn)
	})

	group, ctx := errgroup.WithContext(ctx)

	group.Go(func() error {
		log.Infof(ctx, "HTTP metrics server started at %s", metricsLn.Addr())
		return errors.Wrap(server.ServeHTTP(ctx, metricsLn), " HTTP server failed")
	})

	group.Go(func() error {
		log.Infof(ctx, "proxy server started at ", proxyLn.Addr())
		return errors.Wrap(server.Serve(ctx, proxyLn), "proxy server failed")
	})

	return group.Wait()

	return nil
}
