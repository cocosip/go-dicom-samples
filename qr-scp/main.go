// Copyright (c) 2025 go-dicom-samples contributors.
// Licensed under the MIT License.

// Command qr-scp is the Go port of the fo-dicom "QueryRetrieve SCP" sample: a
// Query/Retrieve SCP answering C-ECHO, C-FIND, C-GET and C-MOVE for the DICOM
// instances found in a local folder.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cocosip/go-dicom/pkg/dicom/dataset"
	"github.com/cocosip/go-dicom/pkg/dicom/uid"
	"github.com/cocosip/go-dicom/pkg/network/association"
	"github.com/cocosip/go-dicom/pkg/network/client"
	"github.com/cocosip/go-dicom/pkg/network/dimse"
	"github.com/cocosip/go-dicom/pkg/network/pdu"
	"github.com/cocosip/go-dicom/pkg/network/server"
	"github.com/cocosip/go-dicom/pkg/network/service"
	"github.com/cocosip/go-dicom/pkg/network/status"

	_ "github.com/cocosip/go-dicom-samples/internal/codecs"
	"github.com/cocosip/go-dicom-samples/internal/store"
)

// maxSubOperations is the largest sub-operation count one C-GET or C-MOVE
// response can carry: the DICOM counts are unsigned 16-bit values.
const maxSubOperations = 65535

// queryServer answers C-FIND, C-GET and C-MOVE from the indexed folder.
type queryServer struct {
	ae        string
	instances []store.Instance
	routes    map[string]aeRoute
	verbose   bool
}

func main() {
	port := flag.Int("port", 11113, "TCP port to listen on")
	ae := flag.String("ae", "QRSCP", "called AE title this SCP answers to")
	dataDir := flag.String("data-dir", "", "folder used to build the query index (default ./DICOM; demo data is generated when it holds no instance)")
	aeRoutes := flag.String("ae-routes", "", "C-MOVE routing table, format: AE1=host:port,AE2=host:port")
	verbose := flag.Bool("verbose", false, "log every request")
	flag.Parse()

	routes, err := parseAERoutes(*aeRoutes)
	if err != nil {
		log.Fatalf("invalid -ae-routes: %v", err)
	}
	q := &queryServer{ae: strings.TrimSpace(*ae), routes: routes, verbose: *verbose}
	if q.instances, err = indexInstances(strings.TrimSpace(*dataDir)); err != nil {
		log.Fatalf("cannot build the query index: %v", err)
	}

	srv := server.New(
		server.WithPort(*port),
		server.WithAssociationTimeout(30*time.Second),
		server.WithRequestTimeout(60*time.Second),
	)
	srv.SetAssociationNegotiatorFunc(q.negotiate)
	srv.SetCEchoHandler(q.handleCEcho)
	srv.SetCFindHandler(q.handleCFind)
	srv.SetCGetHandler(q.handleCGet)
	srv.SetCMoveHandler(q.handleCMove)

	fmt.Printf("Query/Retrieve SCP listening on port %d\n  Called AE: %s\n  Index:     %d instance(s)\n",
		*port, q.ae, len(q.instances))
	for destination, route := range routes {
		fmt.Printf("  C-MOVE:    %s -> %s:%d\n", destination, route.host, route.port)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe(ctx) }()

	select {
	case err := <-serveErr:
		if err != nil {
			log.Fatalf("QR SCP stopped with error: %v", err)
		}
	case <-ctx.Done():
		fmt.Println("\nStopping QR SCP...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown did not finish: %v; closing active connections", err)
			_ = srv.Close()
		}
		<-serveErr
	}
	fmt.Println("QR SCP stopped.")
}

// negotiate rejects associations that address another AE, like the fo-dicom
// sample does, and otherwise accepts every proposed presentation context.
func (q *queryServer) negotiate(ctx context.Context, assoc *association.Association, responder service.AssociationResponder) error {
	if assoc.CalledAE != q.ae {
		fmt.Printf("Rejected association from AE %q: called AE %q is unknown\n", assoc.CallingAE, assoc.CalledAE)
		return responder.SendReject(ctx, pdu.ResultRejectedPermanent, pdu.SourceServiceUser, pdu.ReasonServiceUserCalledAETitleNotRecognized)
	}
	if q.verbose {
		fmt.Printf("Association accepted from AE %q\n", assoc.CallingAE)
	}
	return (&service.DefaultAssociationNegotiator{}).OnAssociationRequest(ctx, assoc, responder)
}

func (q *queryServer) handleCEcho(_ context.Context, req *dimse.CEchoRequest) (*dimse.CEchoResponse, error) {
	if q.verbose {
		fmt.Println("C-ECHO request")
	}
	return dimse.NewCEchoResponseFromRequest(req, status.Success), nil
}

// handleCFind answers PATIENT/STUDY/SERIES/IMAGE queries with one response per
// patient, study, series or image, like the fo-dicom sample.
func (q *queryServer) handleCFind(_ context.Context, op service.CFindOperation) error {
	level, query := op.QueryLevel(), op.Identifier()
	seen := make(map[string]bool)
	answers := make([]store.Instance, 0)
	for _, instance := range q.match(level, query) {
		if key := answerKey(level, instance); !seen[key] {
			seen[key] = true
			answers = append(answers, instance)
		}
	}
	fmt.Printf("C-FIND %s: %d match(es)\n", level, len(answers))

	for _, instance := range answers {
		if err := op.SendPending(q.answer(level, query, instance)); err != nil {
			return err
		}
	}
	return op.SendFinal(status.Success)
}

func (q *queryServer) handleCGet(ctx context.Context, op service.CGetOperation) error {
	instances := q.match(op.QueryLevel(), op.Identifier())
	fmt.Printf("C-GET %s: %d instance(s)\n", op.QueryLevel(), len(instances))
	return q.retrieve(ctx, op, instances, func(ctx context.Context, ds *dataset.Dataset) error {
		resp, err := op.SendCStore(ctx, ds)
		if err != nil {
			return err
		}
		if !resp.Status().IsSuccess() {
			return fmt.Errorf("C-STORE returned %s", resp.Status())
		}
		return nil
	})
}

func (q *queryServer) handleCMove(ctx context.Context, op service.CMoveOperation) error {
	destination := strings.TrimSpace(op.MoveDestination())
	route, ok := q.routes[destination]
	if !ok {
		fmt.Printf("C-MOVE %s: unknown destination AE %q (configure -ae-routes)\n", op.QueryLevel(), destination)
		return op.SendFailure(status.CMoveRefusedMoveDestinationUnknown)
	}

	instances := q.match(op.QueryLevel(), op.Identifier())
	fmt.Printf("C-MOVE %s: %d instance(s) to %s (%s:%d)\n", op.QueryLevel(), len(instances), destination, route.host, route.port)

	target := client.New(
		client.WithCallingAE(q.ae),
		client.WithCalledAE(destination),
		client.WithConnectTimeout(10*time.Second),
		client.WithRequestTimeout(60*time.Second),
	)
	proposed := make(map[string]bool)
	for _, instance := range instances {
		if instance.SOPClassUID == "" || proposed[instance.SOPClassUID] {
			continue
		}
		proposed[instance.SOPClassUID] = true
		if err := target.AddPresentationContext(instance.SOPClassUID,
			uid.ExplicitVRLittleEndian.UID(), uid.ImplicitVRLittleEndian.UID()); err != nil {
			fmt.Printf("C-MOVE: cannot propose %s: %v\n", instance.SOPClassUID, err)
			return op.SendFailure(status.CMoveRefusedOutOfResourcesSubOps)
		}
	}
	if err := target.Connect(ctx, route.host, route.port); err != nil {
		fmt.Printf("C-MOVE: cannot connect to %s (%s:%d): %v\n", destination, route.host, route.port, err)
		return op.SendFailure(status.CMoveRefusedOutOfResourcesSubOps)
	}
	defer func() { _ = target.Close() }()

	return q.retrieve(ctx, op, instances, target.CStore)
}

// retrieve pushes every instance and reports the running sub-operation counts,
// then ends with Success, or Warning when some sub-operations failed.
func (q *queryServer) retrieve(ctx context.Context, op service.SubOperationResponder, instances []store.Instance, put func(context.Context, *dataset.Dataset) error) error {
	if len(instances) == 0 {
		return op.SendSuccess()
	}
	// ponytail: the DICOM sub-operation counts are uint16, so one operation
	// carries at most 65535 instances; split larger batches if that matters.
	if len(instances) > maxSubOperations {
		instances = instances[:maxSubOperations]
	}

	total := uint16(len(instances))
	var completed, failed uint16
	for _, instance := range instances {
		ds, err := store.Load(instance.Path)
		if err != nil {
			fmt.Printf("  cannot read %s: %v\n", instance.Path, err)
			failed++
		} else if err := put(ctx, ds); err != nil {
			fmt.Printf("  %s: %v\n", instance.SOPInstanceUID, err)
			failed++
		} else {
			completed++
		}
		if err := op.SendPending(total-completed-failed, completed, failed, 0); err != nil {
			return err
		}
	}

	if failed > 0 {
		return op.SendWarning()
	}
	return op.SendSuccess()
}
