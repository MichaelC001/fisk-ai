//  Copyright (c) 2026, R.I. Pienaar and the Choria Project contributors
//
//  SPDX-License-Identifier: Apache-2.0

package mcpclient

import (
	"errors"

	"github.com/choria-io/fisk-ai/config"
	"github.com/choria-io/fisk-ai/internal/telemetry"
)

// spanInfo is the one place a configured server becomes something a span may carry.
//
// Everything else about an entry stays here: the url can hold a credential in its
// query or in a path segment, and so can an argument, since the bridge an operator
// reaches for takes the token on the command line. A span leaves the process for a
// collector, so the rule is that only the configured name and a transport token go on
// one, and this function is what that rule is written as. telemetry.MCPServerInfo has
// no field for anything else, so a caller cannot widen it from the other side either.
func (s *Sessions) spanInfo(server config.MCPServer) telemetry.MCPServerInfo {
	return telemetry.MCPServerInfo{
		Server:    server.Name,
		Transport: s.transportToken(server),
	}
}

// transportToken names the transport this package builds for server. A Dialer of the
// caller's own is reported as neither stdio nor http, since what it dials is not this
// package's to describe, and so is an entry that declares no way to be reached.
func (s *Sessions) transportToken(server config.MCPServer) telemetry.MCPTransport {
	switch {
	case s.opts.Dialer != nil:
		return telemetry.MCPTransportOther
	case server.URL != "":
		return telemetry.MCPTransportHTTP
	case server.Command != "":
		return telemetry.MCPTransportStdio
	default:
		return telemetry.MCPTransportOther
	}
}

// connectClass is the error class for a server that could not be connected. A
// deadline that expired and a canceled run are reported as themselves, since the
// entry's startup timeout is the likeliest way a connect fails and reporting it as an
// unreachable server would hide which of the two happened.
func connectClass(err error) telemetry.ErrorClass {
	class, ok := telemetry.ClassifyContext(err)
	if ok {
		return class
	}

	return telemetry.ClassRemoteUnavailable
}

// listClass is the error class for a server whose tools could not be listed. The
// entry's own filters failing to compile is the operator's configuration rather than
// the server, and it is the one failure here that never made a round trip.
func listClass(err error) telemetry.ErrorClass {
	var filter filterError
	if errors.As(err, &filter) {
		return telemetry.ClassConfig
	}

	return connectClass(err)
}
