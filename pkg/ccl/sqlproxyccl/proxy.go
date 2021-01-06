// Copyright 2020 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package sqlproxyccl

import (
	"io"
	"net"
	"os"

	"github.com/cockroachdb/errors"
	"github.com/jackc/pgproto3/v2"
)

const pgAcceptSSLRequest = 'S'

// See https://www.postgresql.org/docs/9.1/protocol-message-formats.html.
var pgSSLRequest = []int32{8, 80877103}

// UpdateMetricsForError updates the metrics relevant for the type of the
// error message.
func UpdateMetricsForError(metrics *Metrics, err error) {
	if err == nil {
		return
	}
	codeErr := (*CodeError)(nil)
	if errors.As(err, &codeErr) {
		switch codeErr.Code {
		case CodeExpiredClientConnection:
			metrics.ExpiredClientConnCount.Inc(1)
		case CodeBackendDisconnected:
			metrics.BackendDisconnectCount.Inc(1)
		case CodeClientDisconnected:
			metrics.ClientDisconnectCount.Inc(1)
		case CodeIdleDisconnect:
			metrics.IdleDisconnectCount.Inc(1)
		case CodeProxyRefusedConnection:
			metrics.RefusedConnCount.Inc(1)
			metrics.BackendDownCount.Inc(1)
		case CodeParamsRoutingFailed:
			metrics.RoutingErrCount.Inc(1)
			metrics.BackendDownCount.Inc(1)
		case CodeBackendDown:
			metrics.BackendDownCount.Inc(1)
		case CodeAuthFailed:
			metrics.AuthFailedCount.Inc(1)
		}
	}
}

// SendErrToClient will encode and pass back to the SQL client an error message.
// It can be called by the implementors of ProxyHandler to give more
// information to the end user in case of a problem.
func SendErrToClient(conn net.Conn, err error) {
	if err == nil || conn == nil {
		return
	}
	codeErr := (*CodeError)(nil)
	if errors.As(err, &codeErr) {
		var msg string
		switch codeErr.Code {
		// These are send as is.
		case CodeExpiredClientConnection,
			CodeBackendDown,
			CodeParamsRoutingFailed,
			CodeClientDisconnected,
			CodeBackendDisconnected,
			CodeAuthFailed,
			CodeProxyRefusedConnection:
			msg = codeErr.Error()
		// The rest - the message send back is sanitized.
		case CodeIdleDisconnect:
			msg = "terminating connection due to idle timeout"
		case CodeUnexpectedInsecureStartupMessage:
			msg = "server requires encryption"
		}

		var pgCode string
		if codeErr.Code == CodeIdleDisconnect {
			pgCode = "57P01" // admin shutdown
		} else {
			pgCode = "08004" // rejected connection
		}
		_, _ = conn.Write((&pgproto3.ErrorResponse{
			Severity: "FATAL",
			Code:     pgCode,
			Message:  msg,
		}).Encode(nil))
	}
}

// ConnectionCopy does a bi-directional copy between the backend and frontend
// connections. It terminates when one of connections terminate.
func ConnectionCopy(crdbConn, conn net.Conn) error {
	errOutgoing := make(chan error, 1)
	errIncoming := make(chan error, 1)

	go func() {
		_, err := io.Copy(crdbConn, conn)
		errOutgoing <- err
	}()
	go func() {
		_, err := io.Copy(conn, crdbConn)
		errIncoming <- err
	}()

	select {
	// NB: when using pgx, we see a nil errIncoming first on clean connection
	// termination. Using psql I see a nil errOutgoing first. I think the PG
	// protocol stipulates sending a message to the server at which point
	// the server closes the connection (errIncoming), but presumably the
	// client gets to close the connection once it's sent that message,
	// meaning either case is possible.
	case err := <-errIncoming:
		if err == nil {
			return nil
		} else if codeErr := (*CodeError)(nil); errors.As(err, &codeErr) &&
			codeErr.Code == CodeExpiredClientConnection {
			return codeErr
		} else if errors.Is(err, os.ErrDeadlineExceeded) {
			return NewErrorf(CodeIdleDisconnect, "terminating connection due to idle timeout: %v", err)
		} else {
			return NewErrorf(CodeBackendDisconnected, "copying from target server to client: %s", err)
		}
	case err := <-errOutgoing:
		// The incoming connection got closed.
		if err != nil {
			return NewErrorf(CodeClientDisconnected, "copying from target server to client: %v", err)
		}
		return nil
	}
}
