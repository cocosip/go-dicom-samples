// Copyright (c) 2025 go-dicom-samples contributors.
// Licensed under the MIT License.

// Package scp serves DIMSE requests as an SCP by driving pkg/network/service
// directly.
//
// pkg/network/server exposes handlers for the C-* services only. The samples
// that also need N-services (print management, MPPS) use this small server
// loop, which is the same sequence the library server uses internally:
// accept, negotiate the association, start the service, wait for it to close.
package scp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/cocosip/go-dicom/pkg/dicom/transfer"
	"github.com/cocosip/go-dicom/pkg/network/association"
	"github.com/cocosip/go-dicom/pkg/network/pdu"
	"github.com/cocosip/go-dicom/pkg/network/service"
)

// DefaultAssociationTimeout bounds how long a peer may take to send its
// A-ASSOCIATE-RQ.
const DefaultAssociationTimeout = 30 * time.Second

// Config configures a Server.
type Config struct {
	// Port is the TCP port to listen on.
	Port int

	// AETitle is the called AE title the server answers to. Empty accepts any.
	AETitle string

	// Handlers receives the DIMSE requests.
	Handlers *service.Handlers

	// AssociationTimeout bounds the A-ASSOCIATE-RQ wait. Zero uses
	// DefaultAssociationTimeout.
	AssociationTimeout time.Duration

	// Logf receives one line per accepted connection. Nil logs nothing.
	Logf func(format string, args ...any)
}

// Server is a minimal DIMSE SCP.
type Server struct {
	config Config

	mu       sync.Mutex
	listener net.Listener
	conns    map[net.Conn]struct{}
	closed   bool
	wg       sync.WaitGroup
}

// New returns a Server for config.
func New(config Config) *Server {
	return &Server{config: config, conns: make(map[net.Conn]struct{})}
}

// ListenAndServe serves until ctx is cancelled or Close is called.
func (s *Server) ListenAndServe(ctx context.Context) error {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", s.config.Port))
	if err != nil {
		return fmt.Errorf("listen on port %d: %w", s.config.Port, err)
	}

	s.mu.Lock()
	s.listener = listener
	s.mu.Unlock()

	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-stop:
		}
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				break
			}
			continue
		}
		if !s.track(conn) {
			_ = conn.Close()
			break
		}
		s.wg.Add(1)
		go s.serve(ctx, conn)
	}

	close(stop)
	s.wg.Wait()
	return nil
}

// Close stops accepting connections and closes the active ones.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	listener := s.listener
	conns := make([]net.Conn, 0, len(s.conns))
	for conn := range s.conns {
		conns = append(conns, conn)
	}
	s.mu.Unlock()

	if listener != nil {
		_ = listener.Close()
	}
	for _, conn := range conns {
		_ = conn.Close()
	}
	return nil
}

func (s *Server) serve(ctx context.Context, conn net.Conn) {
	defer s.wg.Done()
	defer s.untrack(conn)
	defer func() { _ = conn.Close() }()

	svc := service.NewService(conn, nil,
		service.WithHandlers(s.config.Handlers),
		service.WithAssociationNegotiator(&negotiator{
			aeTitle:  s.config.AETitle,
			registry: transfer.NewRegistry(),
		}),
	)
	defer func() { _ = svc.Close() }()

	timeout := s.config.AssociationTimeout
	if timeout <= 0 {
		timeout = DefaultAssociationTimeout
	}
	assocCtx, cancel := context.WithTimeout(ctx, timeout)
	_, err := svc.ReceiveAssociationRequest(assocCtx)
	cancel()
	if err != nil {
		s.logf("association from %s failed: %v", conn.RemoteAddr(), err)
		return
	}

	assoc := svc.GetAssociation()
	s.logf("association accepted: calling AE=%s called AE=%s contexts=%d",
		assoc.CallingAE, assoc.CalledAE, len(assoc.PresentationContexts))

	if err := svc.Start(); err != nil {
		s.logf("service start failed: %v", err)
		return
	}
	<-svc.Context().Done()
	if err := svc.CloseError(); err != nil && !errors.Is(err, service.ErrServiceClosed) {
		s.logf("connection %s closed: %v", conn.RemoteAddr(), err)
	}
}

// negotiator enforces the called AE title and then accepts every presentation
// context with the first proposed transfer syntax the registry knows.
type negotiator struct {
	aeTitle  string
	registry *transfer.Registry
}

func (n *negotiator) OnAssociationRequest(ctx context.Context, assoc *association.Association, responder service.AssociationResponder) error {
	if n.aeTitle != "" && strings.TrimSpace(assoc.CalledAE) != n.aeTitle {
		return responder.SendReject(ctx,
			pdu.ResultRejectedPermanent,
			pdu.SourceServiceUser,
			pdu.ReasonServiceUserCalledAETitleNotRecognized,
		)
	}
	return (&service.DefaultAssociationNegotiator{Registry: n.registry}).OnAssociationRequest(ctx, assoc, responder)
}

func (s *Server) track(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[conn] = struct{}{}
	return true
}

func (s *Server) untrack(conn net.Conn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}

func (s *Server) logf(format string, args ...any) {
	if s.config.Logf != nil {
		s.config.Logf(format, args...)
	}
}
