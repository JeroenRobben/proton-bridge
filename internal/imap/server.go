// Copyright (c) 2021 Proton Technologies AG
//
// This file is part of ProtonMail Bridge.
//
// ProtonMail Bridge is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// ProtonMail Bridge is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with ProtonMail Bridge.  If not, see <https://www.gnu.org/licenses/>.

package imap

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	imapid "github.com/ProtonMail/go-imap-id"
	"github.com/ProtonMail/proton-bridge/internal/bridge"
	"github.com/ProtonMail/proton-bridge/internal/events"
	"github.com/ProtonMail/proton-bridge/internal/imap/id"
	"github.com/ProtonMail/proton-bridge/internal/imap/uidplus"
	"github.com/ProtonMail/proton-bridge/pkg/listener"
	"github.com/emersion/go-imap"
	imapappendlimit "github.com/emersion/go-imap-appendlimit"
	imapidle "github.com/emersion/go-imap-idle"
	imapmove "github.com/emersion/go-imap-move"
	imapquota "github.com/emersion/go-imap-quota"
	imapunselect "github.com/emersion/go-imap-unselect"
	imapserver "github.com/emersion/go-imap/server"
	"github.com/emersion/go-sasl"
	"github.com/sirupsen/logrus"
)

type imapServer struct {
	server        *imapserver.Server
	eventListener listener.Listener
	debugClient   bool
	debugServer   bool
}

// NewIMAPServer constructs a new IMAP server configured with the given options.
func NewIMAPServer(debugClient, debugServer bool, port int, tls *tls.Config, imapBackend *imapBackend, eventListener listener.Listener) *imapServer { //nolint[golint]
	s := imapserver.New(imapBackend)
	s.Addr = fmt.Sprintf("%v:%v", bridge.Host, port)
	s.TLSConfig = tls
	s.AllowInsecureAuth = true
	s.ErrorLog = newServerErrorLogger("server-imap")
	s.AutoLogout = 30 * time.Minute

	serverID := imapid.ID{
		imapid.FieldName:       "ProtonMail Bridge",
		imapid.FieldVendor:     "Proton Technologies AG",
		imapid.FieldSupportURL: "https://protonmail.com/support",
	}

	s.EnableAuth(sasl.Login, func(conn imapserver.Conn) sasl.Server {
		return sasl.NewLoginServer(func(address, password string) error {
			user, err := conn.Server().Backend.Login(nil, address, password)
			if err != nil {
				return err
			}

			ctx := conn.Context()
			ctx.State = imap.AuthenticatedState
			ctx.User = user
			return nil
		})
	})

	s.Enable(
		imapidle.NewExtension(),
		imapmove.NewExtension(),
		id.NewExtension(serverID, imapBackend.bridge),
		imapquota.NewExtension(),
		imapappendlimit.NewExtension(),
		imapunselect.NewExtension(),
		uidplus.NewExtension(),
	)

	return &imapServer{
		server:        s,
		eventListener: eventListener,
		debugClient:   debugClient,
		debugServer:   debugServer,
	}
}

// Starts the server.
func (s *imapServer) ListenAndServe() {
	go s.monitorDisconnectedUsers()

	log.Info("IMAP server listening at ", s.server.Addr)
	l, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		s.eventListener.Emit(events.ErrorEvent, "IMAP failed: "+err.Error())
		log.Error("IMAP failed: ", err)
		return
	}

	err = s.server.Serve(&debugListener{
		Listener: l,
		server:   s,
	})
	if err != nil {
		s.eventListener.Emit(events.ErrorEvent, "IMAP failed: "+err.Error())
		log.Error("IMAP failed: ", err)
		return
	}
	defer s.server.Close() //nolint[errcheck]

	log.Info("IMAP server stopped")
}

// Stops the server.
func (s *imapServer) Close() {
	if err := s.server.Close(); err != nil {
		log.WithError(err).Error("Failed to close the connection")
	}
}

func (s *imapServer) monitorDisconnectedUsers() {
	ch := make(chan string)
	s.eventListener.Add(events.CloseConnectionEvent, ch)

	for address := range ch {
		address := address
		log.Info("Disconnecting all open IMAP connections for ", address)
		disconnectUser := func(conn imapserver.Conn) {
			connUser := conn.Context().User
			if connUser != nil && strings.EqualFold(connUser.Username(), address) {
				if err := conn.Close(); err != nil {
					log.WithError(err).Error("Failed to close the connection")
				}
			}
		}
		s.server.ForEachConn(disconnectUser)
	}
}

// debugListener sets debug loggers on server containing fields with local
// and remote addresses right after new connection is accepted.
type debugListener struct {
	net.Listener

	server *imapServer
}

func (dl *debugListener) Accept() (net.Conn, error) {
	conn, err := dl.Listener.Accept()

	if err == nil && (dl.server.debugServer || dl.server.debugClient) {
		debugLog := log
		if addr := conn.LocalAddr(); addr != nil {
			debugLog = debugLog.WithField("loc", addr.String())
		}
		if addr := conn.RemoteAddr(); addr != nil {
			debugLog = debugLog.WithField("rem", addr.String())
		}

		var localDebug, remoteDebug io.Writer
		if dl.server.debugServer {
			localDebug = debugLog.WithField("pkg", "imap/server").WriterLevel(logrus.DebugLevel)
		}
		if dl.server.debugClient {
			remoteDebug = debugLog.WithField("pkg", "imap/client").WriterLevel(logrus.DebugLevel)
		}

		dl.server.server.Debug = imap.NewDebugWriter(localDebug, remoteDebug)
	}

	return conn, err
}

// serverErrorLogger implements go-imap/logger interface.
type serverErrorLogger struct {
	tag string
}

func newServerErrorLogger(tag string) *serverErrorLogger {
	return &serverErrorLogger{tag}
}

func (s *serverErrorLogger) Printf(format string, args ...interface{}) {
	err := fmt.Sprintf(format, args...)
	log.WithField("pkg", s.tag).Error(err)
}

func (s *serverErrorLogger) Println(args ...interface{}) {
	err := fmt.Sprintln(args...)
	log.WithField("pkg", s.tag).Error(err)
}
