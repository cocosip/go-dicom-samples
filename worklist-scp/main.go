// Copyright (c) 2025 go-dicom-samples contributors.
// Licensed under the MIT License.

// Package main is the Go port of the fo-dicom "Worklist SCP" sample.
//
// A single association serves both information models:
//   - Modality Worklist Information Model - FIND (C-FIND)
//   - Modality Performed Procedure Step (N-CREATE, N-SET, N-GET)
//
// Like the fo-dicom sample, the worklist items are hard-coded and the MPPS
// instances are kept in memory.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/cocosip/go-dicom-samples/internal/codecs"
	"github.com/cocosip/go-dicom-samples/internal/scp"
	"github.com/cocosip/go-dicom/pkg/network/dimse"
	"github.com/cocosip/go-dicom/pkg/network/service"
	"github.com/cocosip/go-dicom/pkg/network/status"
)

var (
	port    = flag.Int("port", 11114, "Port to listen on")
	aeTitle = flag.String("ae", "WORKLISTSCP", "Called AE title")
	verbose = flag.Bool("verbose", false, "Enable verbose logging")
)

func main() {
	flag.Parse()

	worklist := newWorklistServer(worklistItems(time.Now()), *verbose)
	handlers := &service.Handlers{
		CEchoHandler:   worklist.handleCEcho,
		CFindHandler:   worklist.handleCFind,
		NCreateHandler: worklist.handleNCreate,
		NSetHandler:    worklist.handleNSet,
		NGetHandler:    worklist.handleNGet,
	}

	fmt.Printf("Starting Worklist SCP server with AET: %s on port %d\n", *aeTitle, *port)
	fmt.Printf("Worklist items: %d\n", len(worklist.items))
	fmt.Println("Supported operations: C-ECHO, C-FIND (Modality Worklist), N-CREATE/N-SET/N-GET (MPPS)")
	fmt.Println("Press Ctrl+C to stop the service")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := scp.New(scp.Config{
		Port:     *port,
		AETitle:  *aeTitle,
		Handlers: handlers,
		Logf:     log.Printf,
	})

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.ListenAndServe(ctx)
	}()

	select {
	case err := <-serverErr:
		if err != nil {
			log.Fatalf("server stopped with error: %v", err)
		}
	case <-ctx.Done():
		fmt.Println("\nStopping Worklist SCP service")
		if err := server.Close(); err != nil {
			log.Printf("close server: %v", err)
		}
		if err := <-serverErr; err != nil {
			log.Printf("server stopped with error: %v", err)
		}
	}

	fmt.Println("Server stopped.")
}

func (s *worklistServer) handleCEcho(_ context.Context, req *dimse.CEchoRequest) (*dimse.CEchoResponse, error) {
	log.Printf("C-ECHO request received")
	return dimse.NewCEchoResponseFromRequest(req, status.Success), nil
}
