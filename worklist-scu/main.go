// Copyright (c) 2025 go-dicom-samples contributors.
// Licensed under the MIT License.

// Package main is the Go port of the fo-dicom "Worklist SCU" sample.
//
// It queries the Modality Worklist of the matching SCP, starts the first
// scheduled procedure step with an MPPS N-CREATE, completes it with an N-SET,
// and reads the stored instance back with an N-GET. Every step is flag-driven
// instead of reading from the console, so the sample can run unattended.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	_ "github.com/cocosip/go-dicom-samples/internal/codecs"
	"github.com/cocosip/go-dicom/pkg/dicom/dataset"
	"github.com/cocosip/go-dicom/pkg/dicom/tag"
	"github.com/cocosip/go-dicom/pkg/dicom/transfer"
	"github.com/cocosip/go-dicom/pkg/dicom/uid"
	"github.com/cocosip/go-dicom/pkg/network/association"
	"github.com/cocosip/go-dicom/pkg/network/dimse"
	"github.com/cocosip/go-dicom/pkg/network/pdu"
	"github.com/cocosip/go-dicom/pkg/network/service"
)

const (
	// requestTimeout bounds the whole worklist and MPPS workflow.
	requestTimeout = 60 * time.Second
)

var (
	host      = flag.String("host", "localhost", "SCP host")
	port      = flag.Int("port", 11114, "SCP port")
	calledAE  = flag.String("called-ae", "WORKLISTSCP", "Called AE title")
	callingAE = flag.String("calling-ae", "WORKLISTSCU", "Calling AE title")
	patient   = flag.String("patient", "", "Optional PatientName match key (supports * and ? wildcards)")
	stationAE = flag.String("station-ae", "WORKLISTSCU", "Optional ScheduledStationAETitle match key")
	date      = flag.String("date", "", "Optional ScheduledProcedureStepStartDate (YYYYMMDD or YYYYMMDD-YYYYMMDD)")
)

func main() {
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	fmt.Println("=== Modality Worklist SCU (fo-dicom port) ===")
	fmt.Printf("Target:     %s:%d\n", *host, *port)
	fmt.Printf("Calling AE: %s\n", *callingAE)
	fmt.Printf("Called AE:  %s\n", *calledAE)
	fmt.Printf("Patient:    %s\n", orAny(*patient))
	fmt.Printf("Station AE: %s\n", orAny(*stationAE))
	fmt.Printf("Date:       %s\n", orAny(*date))
	fmt.Println()

	conn, err := connect(ctx)
	if err != nil {
		log.Fatalf("association failed: %v", err)
	}
	defer conn.close()

	fmt.Println("Querying modality worklist ...")
	item, err := findWorklist(ctx, conn)
	if err != nil {
		log.Fatalf("C-FIND failed: %v", err)
	}
	if item == nil {
		fmt.Println("No worklist items matched.")
		return
	}

	instanceUID, stepID, err := sendMPPSInProgress(ctx, conn, item)
	if err != nil {
		log.Fatalf("MPPS in progress failed: %v", err)
	}
	if err := sendMPPSCompleted(ctx, conn, instanceUID, stepID); err != nil {
		log.Fatalf("MPPS completed failed: %v", err)
	}
	if err := getMPPS(ctx, conn, instanceUID); err != nil {
		log.Fatalf("MPPS N-GET failed: %v", err)
	}

	fmt.Println("\nWorklist and MPPS workflow completed.")
}

// connection is an SCU association established with pkg/network/service.
//
// pkg/network/client only builds C-FIND requests for the Patient Root and Study
// Root information models, but a Modality Worklist SCU must send its C-FIND with
// the Modality Worklist Information Model - FIND SOP Class. This handshake is
// the same sequence client.Connect performs, so both the worklist C-FIND and the
// MPPS N-services run over one association.
type connection struct {
	svc *service.Service
}

func connect(ctx context.Context) (*connection, error) {
	address := net.JoinHostPort(strings.TrimSpace(*host), strconv.Itoa(*port))
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", address, err)
	}

	registry := transfer.NewRegistry()
	svc := service.NewService(conn, nil,
		service.WithAssociationRequestor(true),
		service.WithTransferSyntaxRegistry(registry),
		service.WithRequestTimeout(requestTimeout),
	)

	transferSyntaxes := []string{
		uid.ExplicitVRLittleEndian.UID(),
		uid.ImplicitVRLittleEndian.UID(),
	}
	rq := pdu.NewAAssociateRQ()
	rq.CalledAETitle = strings.TrimSpace(*calledAE)
	rq.CallingAETitle = strings.TrimSpace(*callingAE)
	rq.ApplicationContext = "1.2.840.10008.3.1.1.1" // DICOM Application Context
	rq.PresentationContexts = []pdu.PresentationContextRQ{
		{ID: 1, AbstractSyntax: uid.ModalityWorklistInformationModelFind.UID(), TransferSyntaxes: transferSyntaxes},
		{ID: 3, AbstractSyntax: uid.ModalityPerformedProcedureStep.UID(), TransferSyntaxes: transferSyntaxes},
	}

	svc.SetAssociation(association.FromAAssociateRQ(rq, registry))
	if err := svc.SendAssociationRequest(ctx, rq); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("send A-ASSOCIATE-RQ: %w", err)
	}
	ac, err := svc.ReceiveAssociationResponse(ctx)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("association rejected: %w", err)
	}

	accepted := association.FromAAssociateRQ(rq, registry)
	if err := association.ApplyAAssociateAC(accepted, ac, registry); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("invalid A-ASSOCIATE-AC: %w", err)
	}
	// A-ASSOCIATE-AC carries only presentation context IDs, so restore the
	// abstract syntax of every accepted context from the request.
	for _, pc := range rq.PresentationContexts {
		if acceptedContext := accepted.FindPresentationContextByID(pc.ID); acceptedContext != nil {
			acceptedContext.AbstractSyntax = pc.AbstractSyntax
		}
	}
	for _, sopClass := range []*uid.UID{uid.ModalityWorklistInformationModelFind, uid.ModalityPerformedProcedureStep} {
		if acceptedContext := accepted.FindPresentationContextByAbstractSyntax(sopClass.UID()); acceptedContext == nil || !acceptedContext.IsAccepted() {
			_ = conn.Close()
			return nil, fmt.Errorf("SCP did not accept the presentation context for %s", sopClass)
		}
	}

	svc.SetAssociation(accepted)
	if err := svc.Start(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("start service: %w", err)
	}

	return &connection{svc: svc}, nil
}

func (c *connection) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.svc.GracefulRelease(ctx)
}

// findWorklist runs the C-FIND and returns the first matching identifier.
func findWorklist(ctx context.Context, c *connection) (*dataset.Dataset, error) {
	identifier, err := buildIdentifier()
	if err != nil {
		return nil, err
	}

	req := dimse.NewCFindRequest(dimse.QueryRetrieveLevelStudy, identifier)
	req.SetAffectedSOPClassUID(uid.ModalityWorklistInformationModelFind.UID())
	// fo-dicom's DicomCFindRequest.CreateWorklistQuery marks the identifier as
	// WORKLIST, and the receiving side expects a Query/Retrieve Level element.
	if err := identifier.AddOrUpdateValue(tag.QueryRetrieveLevel, "WORKLIST"); err != nil {
		return nil, fmt.Errorf("mark identifier as WORKLIST: %w", err)
	}

	events, err := c.svc.SendCFind(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("send C-FIND: %w", err)
	}

	var first *dataset.Dataset
	for event := range events {
		if event.Err != nil {
			return first, event.Err
		}
		response := event.Response
		if response.IsPending() {
			if first == nil {
				first = response.DataDataset()
			}
			printWorklistItem(response.DataDataset())
			continue
		}
		fmt.Printf("C-FIND final status: %s\n", response.Status())
	}
	return first, nil
}

func printWorklistItem(item *dataset.Dataset) {
	sps := firstSequenceItem(item, tag.ScheduledProcedureStepSequence)

	fmt.Printf("- Patient: %s (ID %s), Accession: %s\n",
		valueOf(item, tag.PatientName), valueOf(item, tag.PatientID), valueOf(item, tag.AccessionNumber))
	fmt.Printf("  StudyInstanceUID: %s\n", valueOf(item, tag.StudyInstanceUID))
	fmt.Printf("  Requested procedure: %s %q\n",
		valueOf(item, tag.RequestedProcedureID), valueOf(item, tag.RequestedProcedureDescription))
	if sps != nil {
		fmt.Printf("  Scheduled step: %s %q, modality %s, station %s\n",
			valueOf(sps, tag.ScheduledProcedureStepID), valueOf(sps, tag.ScheduledProcedureStepDescription),
			valueOf(sps, tag.Modality), valueOf(sps, tag.ScheduledStationAETitle))
		fmt.Printf("  Scheduled: %s %s, location %s\n",
			valueOf(sps, tag.ScheduledProcedureStepStartDate), valueOf(sps, tag.ScheduledProcedureStepStartTime),
			valueOf(sps, tag.ScheduledProcedureStepLocation))
	}
}

// sendMPPSInProgress creates a performed procedure step (N-CREATE) for the
// first scheduled procedure step of the worklist item.
func sendMPPSInProgress(ctx context.Context, c *connection, item *dataset.Dataset) (string, string, error) {
	sps := firstSequenceItem(item, tag.ScheduledProcedureStepSequence)
	if sps == nil {
		return "", "", fmt.Errorf("worklist item has no ScheduledProcedureStepSequence")
	}
	stepID := valueOf(sps, tag.ScheduledProcedureStepID)
	instanceUID := uid.GenerateDerivedFromUUID().UID()
	now := time.Now()

	scheduledStep, err := newDataset(
		attribute{tag.StudyInstanceUID, valueOf(item, tag.StudyInstanceUID)},
		attribute{tag.AccessionNumber, valueOf(item, tag.AccessionNumber)},
		attribute{tag.RequestedProcedureID, valueOf(item, tag.RequestedProcedureID)},
		attribute{tag.RequestedProcedureDescription, valueOf(item, tag.RequestedProcedureDescription)},
		attribute{tag.ScheduledProcedureStepID, stepID},
		attribute{tag.ScheduledProcedureStepDescription, valueOf(sps, tag.ScheduledProcedureStepDescription)},
	)
	if err != nil {
		return "", "", err
	}

	attributes, err := newDataset(
		attribute{tag.PatientName, valueOf(item, tag.PatientName)},
		attribute{tag.PatientID, valueOf(item, tag.PatientID)},
		attribute{tag.PatientBirthDate, valueOf(item, tag.PatientBirthDate)},
		attribute{tag.PatientSex, valueOf(item, tag.PatientSex)},
		attribute{tag.PerformedProcedureStepID, stepID},
		attribute{tag.PerformedStationAETitle, strings.TrimSpace(*callingAE)},
		attribute{tag.PerformedStationName, strings.TrimSpace(*callingAE)},
		attribute{tag.PerformedLocation, ""},
		attribute{tag.PerformedProcedureStepStartDate, now.Format("20060102")},
		attribute{tag.PerformedProcedureStepStartTime, now.Format("150405")},
		attribute{tag.PerformedProcedureStepStatus, "IN PROGRESS"},
		attribute{tag.PerformedProcedureStepDescription, stepID},
		attribute{tag.PerformedProcedureTypeDescription, ""},
		attribute{tag.PerformedProcedureStepEndDate, ""},
		attribute{tag.PerformedProcedureStepEndTime, ""},
		attribute{tag.Modality, valueOf(sps, tag.Modality)},
		attribute{tag.StudyID, valueOf(item, tag.StudyID)},
	)
	if err != nil {
		return "", "", err
	}
	if err := attributes.Add(sequenceOf(tag.ScheduledStepAttributesSequence, scheduledStep)); err != nil {
		return "", "", err
	}
	for _, sequence := range []*dataset.Sequence{
		dataset.NewSequence(tag.ReferencedStudySequence),
		dataset.NewSequence(tag.ProcedureCodeSequence),
		dataset.NewSequence(tag.PerformedProtocolCodeSequence),
		dataset.NewSequence(tag.PerformedSeriesSequence),
		dataset.NewSequence(tag.ReferencedPatientSequence),
	} {
		if err := attributes.Add(sequence); err != nil {
			return "", "", err
		}
	}

	req := dimse.NewNCreateRequest(uid.ModalityPerformedProcedureStep.UID(), instanceUID, attributes)
	response, err := c.svc.SendNCreate(ctx, req)
	if err != nil {
		return "", "", fmt.Errorf("send N-CREATE: %w", err)
	}
	fmt.Printf("N-CREATE status: %s (SOP Instance UID %s)\n", response.Status(), instanceUID)
	if !response.IsSuccess() {
		return instanceUID, stepID, fmt.Errorf("N-CREATE returned %s", response.Status())
	}
	return instanceUID, stepID, nil
}

// sendMPPSCompleted finishes the performed procedure step (N-SET) with the
// images the modality would have created.
func sendMPPSCompleted(ctx context.Context, c *connection, instanceUID, stepID string) error {
	now := time.Now()

	image, err := newDataset(
		attribute{tag.ReferencedSOPClassUID, uid.SecondaryCaptureImageStorage.UID()},
		attribute{tag.ReferencedSOPInstanceUID, uid.GenerateDerivedFromUUID().UID()},
	)
	if err != nil {
		return err
	}
	series, err := newDataset(
		attribute{tag.RetrieveAETitle, ""},
		attribute{tag.SeriesDescription, "Serie 1"},
		attribute{tag.PerformingPhysicianName, ""},
		attribute{tag.OperatorsName, ""},
		attribute{tag.ProtocolName, "ProtocolName"},
		attribute{tag.SeriesInstanceUID, uid.GenerateDerivedFromUUID().UID()},
	)
	if err != nil {
		return err
	}
	if err := series.Add(sequenceOf(tag.ReferencedImageSequence, image)); err != nil {
		return err
	}
	if err := series.Add(dataset.NewSequence(tag.ReferencedNonImageCompositeSOPInstanceSequence)); err != nil {
		return err
	}

	attributes, err := newDataset(
		attribute{tag.PerformedProcedureStepEndDate, now.Format("20060102")},
		attribute{tag.PerformedProcedureStepEndTime, now.Format("150405")},
		attribute{tag.PerformedProcedureStepStatus, "COMPLETED"},
		attribute{tag.PerformedProcedureStepDescription, stepID},
		attribute{tag.PerformedProcedureTypeDescription, ""},
	)
	if err != nil {
		return err
	}
	if err := attributes.Add(dataset.NewSequence(tag.PerformedProtocolCodeSequence)); err != nil {
		return err
	}
	if err := attributes.Add(sequenceOf(tag.PerformedSeriesSequence, series)); err != nil {
		return err
	}

	req := dimse.NewNSetRequest(uid.ModalityPerformedProcedureStep.UID(), instanceUID, attributes)
	response, err := c.svc.SendNSet(ctx, req)
	if err != nil {
		return fmt.Errorf("send N-SET: %w", err)
	}
	fmt.Printf("N-SET status: %s\n", response.Status())
	if !response.IsSuccess() {
		return fmt.Errorf("N-SET returned %s", response.Status())
	}
	return nil
}

// getMPPS reads the performed procedure step back (N-GET).
func getMPPS(ctx context.Context, c *connection, instanceUID string) error {
	req := dimse.NewNGetRequest(uid.ModalityPerformedProcedureStep.UID(), instanceUID, nil)
	response, err := c.svc.SendNGet(ctx, req)
	if err != nil {
		return fmt.Errorf("send N-GET: %w", err)
	}
	fmt.Printf("N-GET status: %s\n", response.Status())
	if !response.IsSuccess() {
		return fmt.Errorf("N-GET returned %s", response.Status())
	}
	if attributes := response.DataDataset(); attributes != nil {
		fmt.Printf("  PerformedProcedureStepStatus: %s\n", valueOf(attributes, tag.PerformedProcedureStepStatus))
		fmt.Printf("  PerformedProcedureStepID:     %s\n", valueOf(attributes, tag.PerformedProcedureStepID))
		fmt.Printf("  PerformedProcedureStepEnd:    %s %s\n",
			valueOf(attributes, tag.PerformedProcedureStepEndDate),
			valueOf(attributes, tag.PerformedProcedureStepEndTime))
	}
	return nil
}

// buildIdentifier builds the STUDY-level Modality Worklist C-FIND identifier.
func buildIdentifier() (*dataset.Dataset, error) {
	sps, err := newDataset(
		attribute{tag.ScheduledStationAETitle, strings.TrimSpace(*stationAE)},
		attribute{tag.ScheduledProcedureStepStartDate, strings.TrimSpace(*date)},
		attribute{tag.ScheduledProcedureStepStartTime, ""},
		attribute{tag.ScheduledProcedureStepID, ""},
		attribute{tag.ScheduledProcedureStepDescription, ""},
		attribute{tag.ScheduledProcedureStepLocation, ""},
		attribute{tag.ScheduledPerformingPhysicianName, ""},
		attribute{tag.Modality, ""},
	)
	if err != nil {
		return nil, err
	}

	identifier, err := newDataset(
		attribute{tag.PatientName, strings.TrimSpace(*patient)},
		attribute{tag.PatientID, ""},
		attribute{tag.AccessionNumber, ""},
		attribute{tag.StudyInstanceUID, ""},
		attribute{tag.RequestedProcedureID, ""},
		attribute{tag.RequestedProcedureDescription, ""},
		attribute{tag.ReferringPhysicianName, ""},
		attribute{tag.RequestingPhysician, ""},
		attribute{tag.PatientBirthDate, ""},
		attribute{tag.PatientSex, ""},
		attribute{tag.InstitutionName, ""},
	)
	if err != nil {
		return nil, err
	}
	if err := identifier.Add(sequenceOf(tag.ScheduledProcedureStepSequence, sps)); err != nil {
		return nil, err
	}
	return identifier, nil
}

type attribute struct {
	tag   *tag.Tag
	value any
}

func newDataset(attributes ...attribute) (*dataset.Dataset, error) {
	ds := dataset.New()
	for _, entry := range attributes {
		if err := ds.AddValue(entry.tag, entry.value); err != nil {
			return nil, fmt.Errorf("add %s: %w", entry.tag, err)
		}
	}
	return ds, nil
}

func sequenceOf(t *tag.Tag, items ...*dataset.Dataset) *dataset.Sequence {
	sequence := dataset.NewSequence(t)
	for _, item := range items {
		sequence.AddItem(item)
	}
	return sequence
}

func firstSequenceItem(ds *dataset.Dataset, t *tag.Tag) *dataset.Dataset {
	if ds == nil {
		return nil
	}
	sequence, err := ds.GetSequence(t)
	if err != nil || sequence == nil || sequence.Count() == 0 {
		return nil
	}
	return sequence.GetItem(0)
}

func valueOf(ds *dataset.Dataset, t *tag.Tag) string {
	if ds == nil {
		return ""
	}
	return strings.TrimSpace(ds.TryGetString(t))
}

func orAny(value string) string {
	if strings.TrimSpace(value) == "" {
		return "(any)"
	}
	return value
}
