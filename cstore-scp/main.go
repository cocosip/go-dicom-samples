// Copyright (c) 2025 go-dicom-samples contributors.
// Licensed under the MIT License.

// Package main is the Go port of the fo-dicom "C-Store SCP" sample: a DICOM
// storage server that answers C-ECHO and stores every C-STORE instance in
// <storage>/<StudyInstanceUID>/<SOPInstanceUID>.dcm.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/cocosip/go-dicom-samples/internal/codecs"
	"github.com/cocosip/go-dicom-samples/internal/store"

	"github.com/cocosip/go-dicom/pkg/dicom/tag"
	"github.com/cocosip/go-dicom/pkg/dicom/transfer"
	"github.com/cocosip/go-dicom/pkg/dicom/uid"
	"github.com/cocosip/go-dicom/pkg/network/association"
	"github.com/cocosip/go-dicom/pkg/network/dimse"
	"github.com/cocosip/go-dicom/pkg/network/pdu"
	"github.com/cocosip/go-dicom/pkg/network/server"
	"github.com/cocosip/go-dicom/pkg/network/service"
	"github.com/cocosip/go-dicom/pkg/network/status"
)

var (
	port       = flag.Int("port", 11112, "TCP port to listen on")
	calledAE   = flag.String("ae", "STORESCP", "called AE title this SCP answers to")
	storageDir = flag.String("storage", store.DefaultDir, "folder for received instances")
	verbose    = flag.Bool("verbose", false, "log accepted presentation contexts")
)

// Transfer syntaxes accepted for C-ECHO (fo-dicom StoreScp sample).
var verificationSyntaxes = []*transfer.Syntax{
	transfer.ExplicitVRLittleEndian,
	transfer.ExplicitVRBigEndian,
	transfer.ImplicitVRLittleEndian,
}

// Transfer syntaxes accepted for storage (fo-dicom StoreScp sample, compressed
// ones come from go-dicom-codecs).
var imageSyntaxes = []*transfer.Syntax{
	// Lossless
	transfer.JPEGLSLossless,
	transfer.JPEG2000Lossless,
	transfer.JPEGLosslessSV1,
	transfer.JPEGLossless,
	transfer.RLELossless,
	// Lossy
	transfer.JPEGLSNearLossless,
	transfer.JPEG2000,
	transfer.JPEGBaseline8Bit,
	transfer.JPEGExtended12Bit,
	// Uncompressed
	transfer.ExplicitVRLittleEndian,
	transfer.ExplicitVRBigEndian,
	transfer.ImplicitVRLittleEndian,
}

// storageSOPClasses contains every standard storage SOP Class UID.
var storageSOPClasses = func() map[string]struct{} {
	set := make(map[string]struct{})
	for _, category := range []uid.StorageCategory{
		uid.StorageCategoryImage,
		uid.StorageCategoryPresentationState,
		uid.StorageCategoryStructuredReport,
		uid.StorageCategoryWaveform,
		uid.StorageCategoryDocument,
		uid.StorageCategoryRaw,
	} {
		for _, sopClass := range uid.StorageSOPClassUIDs(category) {
			set[sopClass] = struct{}{}
		}
	}
	return set
}()

func main() {
	flag.Parse()

	srv := server.New(
		server.WithPort(*port),
		server.WithMaxConnections(10),
		server.WithAssociationTimeout(30*time.Second),
		server.WithRequestTimeout(60*time.Second),
	)
	srv.SetCEchoHandler(handleCEcho)
	srv.SetCStoreHandler(handleCStore)
	srv.SetAssociationNegotiatorFunc(negotiate)
	srv.SetConnectionLifecycleHandlerFuncs(
		func(_ context.Context, source, reason byte) {
			log.Printf("association aborted: source=0x%02X reason=0x%02X", source, reason)
		},
		func(_ context.Context, err error) {
			if err != nil && *verbose {
				log.Printf("connection closed: %v", err)
			}
		},
	)

	fmt.Printf("Starting C-Store SCP on port %d (AE: %s, storage: %s)\n", *port, *calledAE, *storageDir)
	fmt.Println("Press Ctrl+C to stop")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe(ctx) }()

	select {
	case err := <-errCh:
		if err != nil {
			log.Fatalf("server error: %v", err)
		}
		return
	case <-ctx.Done():
	}

	fmt.Println("Shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown did not finish: %v", err)
		_ = srv.Close()
	}
	if err := <-errCh; err != nil {
		log.Printf("server error: %v", err)
	}
	fmt.Println("Stopped.")
}

func negotiate(ctx context.Context, assoc *association.Association, responder service.AssociationResponder) error {
	if strings.TrimSpace(assoc.CalledAE) != *calledAE {
		log.Printf("rejecting association: called AE %q is not %q", assoc.CalledAE, *calledAE)
		return responder.SendReject(ctx,
			pdu.ResultRejectedPermanent,
			pdu.SourceServiceUser,
			pdu.ReasonServiceUserCalledAETitleNotRecognized,
		)
	}

	for _, pc := range assoc.PresentationContexts {
		switch {
		case pc.AbstractSyntax == uid.Verification.UID():
			pc.AcceptTransferSyntaxes(true, verificationSyntaxes...)
		case isStorageSOPClass(pc.AbstractSyntax):
			pc.AcceptTransferSyntaxes(true, imageSyntaxes...)
		default:
			pc.Reject(association.ResultAbstractSyntaxNotSupported)
		}
		if *verbose {
			log.Printf("  PC %d: %s -> %s", pc.ID, pc.AbstractSyntax, pc.AcceptedTransferSyntax)
		}
	}

	return responder.SendAccept(ctx, assoc)
}

func isStorageSOPClass(sopClassUID string) bool {
	_, ok := storageSOPClasses[strings.TrimSpace(sopClassUID)]
	return ok
}

func handleCEcho(_ context.Context, req *dimse.CEchoRequest) (*dimse.CEchoResponse, error) {
	log.Printf("C-ECHO from %s", req.AffectedSOPClassUID())
	return dimse.NewCEchoResponseFromRequest(req, status.Success), nil
}

func handleCStore(_ context.Context, req *dimse.CStoreRequest) (*dimse.CStoreResponse, error) {
	ds := req.DataDataset()
	if ds == nil {
		return dimse.NewCStoreResponseFromRequest(req, status.CStoreErrorCannotUnderstand),
			errors.New("C-STORE request has no dataset")
	}

	path, err := store.Save(*storageDir, ds)
	if err != nil {
		log.Printf("C-STORE failed: %v", err)
		return dimse.NewCStoreResponseFromRequest(req, status.CStoreErrorCannotUnderstand), err
	}

	patientName, _ := ds.GetString(tag.PatientName)
	fmt.Printf("Stored %s (%s) from %s -> %s\n", req.AffectedSOPInstanceUID(), patientName, req.AffectedSOPClassUID(), path)
	return dimse.NewCStoreResponseFromRequest(req, status.Success), nil
}
