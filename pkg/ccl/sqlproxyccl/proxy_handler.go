// Copyright 2021 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package sqlproxyccl

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"math/big"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cockroachdb/cockroach/pkg/ccl/sqlproxyccl/admitter"
	"github.com/cockroachdb/cockroach/pkg/ccl/sqlproxyccl/cache"
	"github.com/cockroachdb/cockroach/pkg/ccl/sqlproxyccl/denylist"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/logtags"
	"github.com/jackc/pgproto3/v2"
	"github.com/spf13/viper"
)

var (
	// This assumes that whitespaces are used to separate command line args.
	// Unlike the original spec, this does not handle escaping rules.
	//
	// See "options" in https://www.postgresql.org/docs/current/libpq-connect.html#LIBPQ-PARAMKEYWORDS.
	clusterNameLongOptionRE = regexp.MustCompile(`(?:-c\s*|--)cluster=([\S]*)`)

	// ClusterNameRegex restricts cluster names to have between 6 and 20
	// alphanumeric characters, with dashes allowed within the name (but not as a
	// starting or ending character).
	ClusterNameRegex = regexp.MustCompile("^[a-z0-9][a-z0-9-]{4,18}[a-z0-9]$")
)

const (
	// Cluster identifier is in the form "clustername-<tenant_id>. Tenant id is
	// always in the end but the cluster name can also contain '-' or  digits.
	// For example:
	// "foo-7-10" -> cluster name is "foo-7" and tenant id is 10.
	clusterTenantSep = "-"
	// TODO(spaskob): add ballpark estimate.
	maxKnownConnCacheSize = 5e6 // 5 million.
)

type ProxyOptions struct {
	Denylist       string
	ListenAddr     string
	ListenCert     string
	ListenKey      string
	MetricsAddress string
	// Verify if set will skip the identity verification of the
	// backend. This is for testing only.
	Verify bool
	// Routing rule for constructing the backend address for each incoming
	// connection. Optionally use '{{clusterName}}'
	// which will be substituted with the cluster name.
	RoutingRule            string
	RatelimitBaseDelay     time.Duration
	ValidateAccessInterval time.Duration
	PollConfigInterval     time.Duration
}

type ProxyHandler struct {
	ProxyOptions

	// IncomingTLSConfig is the TLS configuration of the proxy endpoint to
	// which clients connect.
	IncomingTLSConfig *tls.Config

	// DenyListService provides access control
	DenyListService denylist.Service

	// AdmitterService will do throttling of incoming connection requests.
	AdmitterService admitter.Service

	//ConnCache is used to keep track of all current connections.
	ConnCache cache.ConnCache

	// Called after successfully connecting to OutgoingAddr.
	OnConnectionSuccess func()

	// KeepAliveLoop if provided controls the lifetime of the proxy connection.
	// It will be run in its own goroutine when the connection is successfully
	// opened. Returning from `KeepAliveLoop` will close the proxy connection.
	// Note that non-nil error return values will be forwarded to the user and
	// hence should explain the reason for terminating the connection.
	// Most common use of KeepAliveLoop will be as an infinite loop that
	// periodically checks if the connection should still be kept alive. Hence it
	// may block indefinitely so it's prudent to use the provided context and
	// return on context cancellation.
	// See `TestProxyKeepAlive` for example usage.
	KeepAliveLoop func(context.Context) error
}

// NewProxyHandler will create a new proxy handler with configuration based on
// the provided options.
func NewProxyHandler(ctx context.Context, options ProxyOptions) (*ProxyHandler, error) {
	handler := ProxyHandler{ProxyOptions: options}

	var err error
	handler.IncomingTLSConfig, err = ListenTLSConfig(options.ListenCert, options.ListenKey)
	if err != nil {
		return nil, err
	}

	vprCfg := viper.New()
	if options.Denylist != "" {
		if vprCfg, err = denylist.NewViperCfgFromFile(options.Denylist); err != nil {
			return nil, err
		}
	}
	log.Infof(ctx, "current denied keys: %+v", vprCfg.AllKeys())
	handler.DenyListService = denylist.NewViperDenyList(
		ctx, vprCfg, denylist.WithPollInterval(options.PollConfigInterval))

	handler.AdmitterService = admitter.NewLocalService(admitter.WithBaseDelay(options.RatelimitBaseDelay))
	handler.ConnCache = cache.NewCappedConnCache(maxKnownConnCacheSize)

	return &handler, nil
}

func (handler *ProxyHandler) Handle(ctx context.Context, metrics *Metrics, proxyConn *Conn) error {
	conn, msg, err := FrontendAdmit(proxyConn, handler.IncomingTLSConfig)
	if err != nil {
		SendErrToClient(conn, err)
		return err
	}

	// This currently only happens for CancelRequest type of startup messages
	// that we don't support
	if conn == nil {
		return nil
	}

	defer func() { _ = conn.Close() }()

	//backendConfig, clientErr := backendConfigFromParams(
	//	handler.AdmitterService,
	//	handler.ConnCache,
	//	handler.DenyListService,
	//	options.routingRule,
	//	!options.verify, /* insecure skip verify */
	//	msg.Parameters,
	//	proxyConn,
	//)

	// Note that the errors returned from this function are user-facing errors so
	// we should be careful with the details that we want to expose.
	backendStartupMsg, clusterName, tenID, err := ClusterNameAndTenantFromParams(msg)
	if err != nil {
		clientErr := NewErrorf(CodeParamsRoutingFailed, err.Error())
		log.Errorf(ctx, clientErr.Error())
		SendErrToClient(conn, clientErr)
		return clientErr
	}
	// This forwards the remote addr to the backend.
	backendStartupMsg.Parameters["crdb:remote_addr"] = conn.RemoteAddr().String()

	ctx = logtags.AddTag(ctx, "cluster", clusterName)
	ctx = logtags.AddTag(ctx, "tenant", tenID)

	ipAddr, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		clientErr := NewErrorf(CodeParamsRoutingFailed, err.Error())
		log.Errorf(ctx, "could not parse address: %v", clientErr.Error())
		SendErrToClient(conn, clientErr)
		return clientErr
	}

	if err = handler.validateAccessAndAdmitConnection(ctx, tenID, ipAddr); err != nil {
		SendErrToClient(conn, err)
		return err
	}

	OutgoingAddress := strings.ReplaceAll(handler.RoutingRule, "{{clusterName}}", clusterName)
	TLSConf := &tls.Config{InsecureSkipVerify: handler.Verify}

	crdbConn, err := BackendDial(msg, OutgoingAddress, TLSConf)
	if err != nil {
		UpdateMetricsForError(metrics, err)
		SendErrToClient(conn, err)
		return err
	}

	defer func() { _ = crdbConn.Close() }()

	if err := Authenticate(conn, crdbConn); err != nil {
		UpdateMetricsForError(metrics, err)
		SendErrToClient(conn, err)
		return errors.AssertionFailedf("unrecognized auth failure")
	}

	metrics.SuccessfulConnCount.Inc(1)

	handler.ConnCache.Insert(&cache.ConnKey{IpAddress: ipAddr, TenantID: tenID})

	errConnectionCopy := make(chan error, 1)
	errExpired := make(chan error, 1)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		errExpired <- func(ctx context.Context) error {
			t := timeutil.NewTimer()
			defer t.Stop()
			t.Reset(0)
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-t.C:
					t.Read = true
					if err := handler.validateAccess(ctx, tenID, ipAddr); err != nil {
						return err
					}
				}
				t.Reset(util.Jitter(handler.ValidateAccessInterval, 0.15))
			}
		}(ctx)
	}()

	go func() {
		err := ConnectionCopy(crdbConn, conn)
		errConnectionCopy <- err
	}()

	select {
	case err := <-errConnectionCopy:
		UpdateMetricsForError(metrics, err)
		SendErrToClient(conn, err)
		return err
	case err := <-errExpired:
		if err != nil {
			// The client connection expired.
			codeErr := NewErrorf(
				CodeExpiredClientConnection, "expired client conn: %v", err,
			)
			UpdateMetricsForError(metrics, codeErr)
			SendErrToClient(conn, codeErr)
			return codeErr
		}
		return nil
	}
}

func (handler *ProxyHandler) validateAccessAndAdmitConnection(
	ctx context.Context, tenID uint64, ipAddr string,
) error {
	if err := handler.validateAccess(ctx, tenID, ipAddr); err != nil {
		return err
	}

	// Admit the connection
	connKey := cache.ConnKey{IpAddress: ipAddr, TenantID: tenID}
	if !handler.ConnCache.Exists(&connKey) {
		// Unknown previous successful connections from this IP and tenant.
		// Hence we need to rate limit.
		if err := handler.AdmitterService.LoginCheck(ipAddr, timeutil.Now()); err != nil {
			log.Errorf(ctx, "admitter refused connection: %v", err.Error())
			return NewErrorf(CodeProxyRefusedConnection, "Connection attempt throttled")
		}
	}

	return nil
}

func (handler *ProxyHandler) validateAccess(
	ctx context.Context, tenID uint64, ipAddr string,
) error {
	// First validate against the deny list service
	list := handler.DenyListService
	if entry, err := list.Denied(fmt.Sprint(tenID)); err != nil {
		// Log error but don't return since this could be transient.
		log.Errorf(ctx, "could not consult denied list for tenant: %s", tenID)
	} else if entry != nil {
		log.Errorf(ctx, "access denied for tenant: %s, reason: %s", tenID, entry.Reason)
		return NewErrorf(CodeProxyRefusedConnection, "tenant %s %s", tenID, entry.Reason)
	}

	if entry, err := list.Denied(ipAddr); err != nil {
		// Log error but don't return since this could be transient.
		log.Errorf(ctx, "could not consult denied list for IP address: %s", ipAddr)
	} else if entry != nil {
		log.Errorf(ctx, "access denied for IP address: %s, reason: %s", ipAddr, entry.Reason)
		return NewErrorf(CodeProxyRefusedConnection, "IP address %s %s", ipAddr, entry.Reason)
	}

	return nil
}

// ClusterNameAndTenantFromParams extracts the cluster name from the connection
// parameters, and rewrites the database param, if necessary. We currently
// support embedding the cluster name in two ways:
// - Within the database param (e.g. "happy-koala.defaultdb")
//
// - Within the options param (e.g. "... --cluster=happy-koala ...").
//   PostgreSQL supports three different ways to set a run-time parameter
//   through its command-line options, i.e. "-c NAME=VALUE", "-cNAME=VALUE", and
//   "--NAME=VALUE".
func ClusterNameAndTenantFromParams(
	msg *pgproto3.StartupMessage,
) (*pgproto3.StartupMessage, string, uint64, error) {
	clusterNameFromDB, databaseName, err := parseDatabaseParam(msg.Parameters["database"])
	if err != nil {
		return msg, "", 0, err
	}

	clusterNameFromOpt, err := parseOptionsParam(msg.Parameters["options"])
	if err != nil {
		return msg, "", 0, err
	}

	if clusterNameFromDB == "" && clusterNameFromOpt == "" {
		return msg, "", 0, errors.New("missing cluster name in connection string")
	}

	if clusterNameFromDB != "" && clusterNameFromOpt != "" {
		return msg, "", 0, errors.New("multiple cluster names provided")
	}

	if clusterNameFromDB == "" {
		clusterNameFromDB = clusterNameFromOpt
	}

	sepIdx := strings.LastIndex(clusterNameFromDB, clusterTenantSep)
	// Cluster name provided without a tenant ID in the end.
	if sepIdx == -1 || sepIdx == len(clusterNameFromDB)-1 {
		return msg, "", 0, errors.Errorf("invalid cluster name %s", clusterNameFromDB)
	}
	clusterNameSansTenant, tenantIDStr := clusterNameFromDB[:sepIdx], clusterNameFromDB[sepIdx+1:]

	if !ClusterNameRegex.MatchString(clusterNameSansTenant) {
		return msg, "", 0, errors.Errorf("invalid cluster name '%s'", clusterNameSansTenant)
	}

	tenID, err := strconv.ParseUint(tenantIDStr, 10, 64)
	if err != nil {
		return msg, "", 0, errors.Wrapf(err, "cannot parse %s as uint64", tenantIDStr)
	}

	// Make ane return a copy of the startup msg so the original is not modified.
	paramsOut := map[string]string{}
	for key, value := range msg.Parameters {
		if key == "database" {
			paramsOut[key] = databaseName
		} else if key != "options" {
			paramsOut[key] = value
		}
	}
	outMsg := &pgproto3.StartupMessage{
		ProtocolVersion: msg.ProtocolVersion,
		Parameters:      paramsOut,
	}

	return outMsg, clusterNameFromDB, tenID, nil
}

// parseDatabaseParam parses the database parameter from the PG connection
// string, and tries to extract the cluster name if present. The cluster
// name should be embedded in the database parameter using the dot (".")
// delimiter in the form of "<cluster name>.<database name>". This approach
// is safe because dots are not allowed in the database names themselves.
func parseDatabaseParam(databaseParam string) (clusterName, databaseName string, err error) {
	// Database param is not provided.
	if databaseParam == "" {
		return "", "", nil
	}

	parts := strings.Split(databaseParam, ".")

	// Database param provided without cluster name.
	if len(parts) <= 1 {
		return "", databaseParam, nil
	}

	clusterName, databaseName = parts[0], parts[1]

	// Ensure that the param is in the right format if the delimiter is provided.
	if len(parts) > 2 || clusterName == "" || databaseName == "" {
		return "", "", errors.New("invalid database param")
	}

	return clusterName, databaseName, nil
}

// parseOptionsParam parses the options parameter from the PG connection string,
// and tries to return the cluster name if present. Just like PostgreSQL, the
// sqlproxy supports three different ways to set a run-time parameter through
// its command-line options:
//     -c NAME=VALUE (commonly used throughout documentation around PGOPTIONS)
//     -cNAME=VALUE
//     --NAME=VALUE
//
// CockroachDB currently does not support the options parameter, so the parsing
// logic is built on that assumption. If we do start supporting options in
// CockroachDB itself, then we should revisit this.
//
// Note that this parsing approach is not perfect as it allows a negative case
// like options="-c --cluster=happy-koala -c -c -c" to go through. To properly
// parse this, we need to traverse the string from left to right, and look at
// every single argument, but that involves quite a bit of work, so we'll punt
// for now.
func parseOptionsParam(optionsParam string) (string, error) {
	// Only search up to 2 in case of large inputs.
	matches := clusterNameLongOptionRE.FindAllStringSubmatch(optionsParam, 2 /* n */)
	if len(matches) == 0 {
		return "", nil
	}

	if len(matches) > 1 {
		// Technically we could still allow requests to go through if all
		// cluster names match, but we don't want to parse the entire string, so
		// we will just error out if at least two cluster flags are provided.
		return "", errors.New("multiple cluster flags provided")
	}

	// Length of each match should always be 2 with the given regex, one for
	// the full string, and the other for the cluster name.
	if len(matches[0]) != 2 {
		// We don't want to panic here.
		return "", errors.New("internal server error")
	}

	// Flag was provided, but value is NULL.
	if len(matches[0][1]) == 0 {
		return "", errors.New("invalid cluster flag")
	}

	return matches[0][1], nil
}

// ListenTLSConfig will return the certificate specified by the arguments or
// if none is specified - generate a self signed certificate.
func ListenTLSConfig(listenCert, listenKey string) (*tls.Config, error) {
	if (listenKey == "") != (listenCert == "") {
		return nil, errors.New("must specify either both or neither of cert and key")
	}

	if listenCert != "" {
		// Cert and key will load from file
		certBytes, err := ioutil.ReadFile(listenCert)
		if err != nil {
			return nil, err
		}
		keyBytes, err := ioutil.ReadFile(listenKey)
		if err != nil {
			return nil, err
		}
		cert, err := tls.X509KeyPair(certBytes, keyBytes)
		if err != nil {
			return nil, err
		}
		return &tls.Config{
			Certificates: []tls.Certificate{cert},
		}, nil
	}

	// Create self-signed cert
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
	}
	cer, err := x509.CreateCertificate(rand.Reader, &template, &template, pub, priv)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
			Certificates: []tls.Certificate{
				{
					Certificate: [][]byte{cer},
					PrivateKey:  priv,
				},
			},
		},
		nil
}
