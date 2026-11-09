// Copyright (c) 2025 go-dicom-samples contributors.
// Licensed under the MIT License.

// Command print-scp is the DICOM Print Management SCP from the fo-dicom
// "Print SCP" sample. It accepts the Basic Grayscale Print Management Meta SOP
// Class, keeps the Film Session / Film Box / Image Box hierarchy in memory and
// persists accepted print jobs below -jobs-dir. There is no printer output and
// no image rendering: this sample exercises the DIMSE-N workflow only.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync"

	"github.com/cocosip/go-dicom/pkg/dicom/dataset"
	"github.com/cocosip/go-dicom/pkg/dicom/tag"
	"github.com/cocosip/go-dicom/pkg/dicom/uid"
	"github.com/cocosip/go-dicom/pkg/network/dimse"
	"github.com/cocosip/go-dicom/pkg/network/service"
	"github.com/cocosip/go-dicom/pkg/network/status"
	"github.com/cocosip/go-dicom/pkg/printing"

	"github.com/cocosip/go-dicom-samples/internal/scp"
)

// filmSessionRecord is one Basic Film Session received on any association,
// keyed by its SOP Instance UID.
type filmSessionRecord struct {
	session *printing.FilmSession
	printed bool
}

// printServer holds the in-memory print hierarchy. Every handler runs under mu
// because the service may dispatch requests of several associations at once.
type printServer struct {
	mu       sync.Mutex
	jobsDir  string
	verbose  bool
	status   *printing.PrinterStatusManager
	sessions map[string]*filmSessionRecord
}

func main() {
	port := flag.Int("port", 11115, "TCP port to listen on")
	ae := flag.String("ae", "PRINTSCP", "called AE title this SCP answers to")
	jobsDir := flag.String("jobs-dir", "./PrintJobs", "folder that receives persisted print jobs")
	verbose := flag.Bool("verbose", false, "log every received DIMSE-N request")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	printer := printing.NewPrinterStatusManager(*ae)
	if err := printer.Update(printing.PrinterStatusNormal, "NORMAL"); err != nil {
		log.Printf("initialize printer status: %v", err)
	}
	server := &printServer{
		jobsDir:  *jobsDir,
		verbose:  *verbose,
		status:   printer,
		sessions: make(map[string]*filmSessionRecord),
	}

	fmt.Printf("Starting print SCP server with AET: %s on port %d\n", *ae, *port)
	fmt.Printf("Print jobs are persisted below %s\n", *jobsDir)

	err := scp.New(scp.Config{
		Port:     *port,
		AETitle:  *ae,
		Handlers: server.handlers(),
		Logf:     log.Printf,
	}).ListenAndServe(ctx)
	if err != nil && ctx.Err() == nil {
		log.Printf("print SCP stopped: %v", err)
		os.Exit(1)
	}

	fmt.Println("Stopping print service")
}

func (s *printServer) handlers() *service.Handlers {
	return &service.Handlers{
		CEchoHandler:   s.echo,
		NCreateHandler: s.ncreate,
		NSetHandler:    s.nset,
		NActionHandler: s.naction,
		NDeleteHandler: s.ndelete,
		NGetHandler:    s.nget,
	}
}

func (s *printServer) echo(_ context.Context, req *dimse.CEchoRequest) (*dimse.CEchoResponse, error) {
	log.Printf("Received verification request (C-ECHO)")
	return dimse.NewCEchoResponseFromRequest(req, status.Success), nil
}

// ncreate handles N-CREATE for the Basic Film Session and Basic Film Box.
func (s *printServer) ncreate(_ context.Context, req *dimse.NCreateRequest) (*dimse.NCreateResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.verbose {
		log.Printf("N-CREATE-RQ %s", req.String())
	}

	switch classUID := req.AffectedSOPClassUID(); classUID {
	case uid.BasicFilmSession.UID():
		return s.createFilmSession(req)
	case uid.BasicFilmBox.UID():
		return s.createFilmBox(req)
	default:
		log.Printf("N-CREATE for unsupported SOP class %s", classUID)
		return dimse.NewNCreateResponse(req.MessageID(), status.RefusedSOPClassNotSupported,
			classUID, req.AffectedSOPInstanceUID(), nil), nil
	}
}

func (s *printServer) createFilmSession(req *dimse.NCreateRequest) (*dimse.NCreateResponse, error) {
	classUID, instanceUID := req.AffectedSOPClassUID(), req.AffectedSOPInstanceUID()
	if instanceUID == "" {
		instanceUID = uid.GenerateDerivedFromUUID().UID()
	}
	attributes := req.DataDataset()
	if attributes == nil {
		attributes = dataset.New()
	}

	session, err := printing.NewFilmSessionFromDataset(classUID, instanceUID, attributes, false)
	if err != nil {
		log.Printf("N-CREATE Film Session failed: %v", err)
		return dimse.NewNCreateResponse(req.MessageID(), status.NCreateFailureInvalidAttributeValue,
			classUID, instanceUID, nil), nil
	}
	if s.sessions[session.SOPInstanceUID] != nil {
		log.Printf("Film session %s already exists on this SCP", session.SOPInstanceUID)
		return dimse.NewNCreateResponse(req.MessageID(), status.NCreateFailureDuplicateSOPInstance,
			classUID, session.SOPInstanceUID, nil), nil
	}

	s.sessions[session.SOPInstanceUID] = &filmSessionRecord{session: session}
	log.Printf("Created film session %s: label=%q copies=%d priority=%s medium=%s destination=%s",
		session.SOPInstanceUID, session.FilmSessionLabel, session.NumberOfCopies,
		session.PrintPriority, session.MediumType, session.FilmDestination)
	return dimse.NewNCreateResponse(req.MessageID(), status.NCreateSuccess,
		classUID, session.SOPInstanceUID, nil), nil
}

func (s *printServer) createFilmBox(req *dimse.NCreateRequest) (*dimse.NCreateResponse, error) {
	classUID, instanceUID := req.AffectedSOPClassUID(), req.AffectedSOPInstanceUID()
	attributes := req.DataDataset()
	record := s.filmSessionFor(attributes)
	if record == nil {
		log.Printf("N-CREATE Film Box %s: no matching film session", instanceUID)
		return dimse.NewNCreateResponse(req.MessageID(), status.ErrorNoSuchObjectInstance,
			classUID, instanceUID, nil), nil
	}
	if attributes == nil {
		log.Printf("N-CREATE Film Box: request has no attribute list")
		return dimse.NewNCreateResponse(req.MessageID(), status.NCreateFailureMissingAttributeValue,
			classUID, instanceUID, nil), nil
	}

	if instanceUID == "" {
		instanceUID = uid.GenerateDerivedFromUUID().UID()
	}
	filmBox, err := record.session.CreateFilmBox(instanceUID, attributes)
	if err != nil {
		log.Printf("N-CREATE Film Box %s failed: %v", instanceUID, err)
		return dimse.NewNCreateResponse(req.MessageID(), status.NCreateFailureInvalidAttributeValue,
			classUID, instanceUID, nil), nil
	}
	if err := filmBox.InitializeImageBoxes(); err != nil {
		log.Printf("Initialize film box %s failed: %v", filmBox.SOPInstanceUID, err)
		record.session.DeleteFilmBox(filmBox.SOPInstanceUID)
		return dimse.NewNCreateResponse(req.MessageID(), status.NCreateFailureInvalidAttributeValue,
			classUID, filmBox.SOPInstanceUID, nil), nil
	}

	// The print SCU requires the SCP-assigned Image Box UIDs in the response.
	responseDataset, err := filmBox.ToDataset()
	if err != nil {
		log.Printf("Encode film box %s failed: %v", filmBox.SOPInstanceUID, err)
		return dimse.NewNCreateResponse(req.MessageID(), status.NCreateFailureProcessingFailure,
			classUID, filmBox.SOPInstanceUID, nil), nil
	}

	log.Printf("Created film box %s: image display format=%q image boxes=%d",
		filmBox.SOPInstanceUID, filmBox.ImageDisplayFormat, filmBox.ImageBoxCount())
	return dimse.NewNCreateResponse(req.MessageID(), status.NCreateSuccess,
		classUID, filmBox.SOPInstanceUID, responseDataset), nil
}

// nset handles N-SET for the Basic Grayscale Image Box.
func (s *printServer) nset(_ context.Context, req *dimse.NSetRequest) (*dimse.NSetResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.verbose {
		log.Printf("N-SET-RQ %s", req.String())
	}

	classUID, instanceUID := req.RequestedSOPClassUID(), req.RequestedSOPInstanceUID()
	if classUID != printing.SOPClassGrayscaleImageBox {
		log.Printf("N-SET for unsupported SOP class %s", classUID)
		return dimse.NewNSetResponse(req.MessageID(), status.RefusedSOPClassNotSupported, classUID, instanceUID, nil), nil
	}
	attributes := req.DataDataset()
	if attributes == nil {
		log.Printf("N-SET Image Box %s: request has no modification list", instanceUID)
		return dimse.NewNSetResponse(req.MessageID(), status.NSetFailureMissingAttribute, classUID, instanceUID, nil), nil
	}
	imageBox := s.findImageBox(instanceUID)
	if imageBox == nil {
		log.Printf("N-SET for unknown image box %s", instanceUID)
		return dimse.NewNSetResponse(req.MessageID(), status.NSetFailureNoSuchObjectInstance, classUID, instanceUID, nil), nil
	}

	updated, err := printing.NewImageBoxFromDataset(imageBox.SOPInstanceUID, attributes, imageBox.IsColor)
	if err != nil {
		log.Printf("N-SET Image Box %s: %v", imageBox.SOPInstanceUID, err)
		return dimse.NewNSetResponse(req.MessageID(), status.NSetFailureInvalidAttributeValue, classUID, instanceUID, nil), nil
	}
	sequence, err := updated.ImageSequence()
	if err != nil || sequence == nil {
		log.Printf("N-SET Image Box %s: no image sequence (%v)", imageBox.SOPInstanceUID, err)
		return dimse.NewNSetResponse(req.MessageID(), status.NSetFailureMissingAttribute, classUID, instanceUID, nil), nil
	}
	if err := imageBox.SetImageSequence(sequence); err != nil {
		log.Printf("N-SET Image Box %s: %v", imageBox.SOPInstanceUID, err)
		return dimse.NewNSetResponse(req.MessageID(), status.NSetFailureProcessingFailure, classUID, instanceUID, nil), nil
	}

	log.Printf("Set image box %s (position %d): %dx%d, %d/%d bits, %d pixel bytes",
		imageBox.SOPInstanceUID, imageBox.ImageBoxPosition,
		uint16OrZero(sequence, tag.Columns), uint16OrZero(sequence, tag.Rows),
		uint16OrZero(sequence, tag.BitsStored), uint16OrZero(sequence, tag.BitsAllocated),
		pixelByteCount(sequence))
	return dimse.NewNSetResponse(req.MessageID(), status.NSetSuccess,
		imageBox.SOPClassUID, imageBox.SOPInstanceUID, nil), nil
}

// naction handles N-ACTION action type 1 (Print) on a Film Box or Film Session.
func (s *printServer) naction(_ context.Context, req *dimse.NActionRequest) (*dimse.NActionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.verbose {
		log.Printf("N-ACTION-RQ %s", req.String())
	}

	if req.ActionTypeID() != 1 {
		log.Printf("N-ACTION for unsupported action type %d", req.ActionTypeID())
		return dimse.NewNActionResponse(req.MessageID(), status.NActionFailureNoSuchActionType,
			req.RequestedSOPClassUID(), req.RequestedSOPInstanceUID(), req.ActionTypeID(), nil), nil
	}

	classUID, instanceUID := req.RequestedSOPClassUID(), req.RequestedSOPInstanceUID()
	var record *filmSessionRecord
	var filmBoxes []*printing.FilmBox
	switch classUID {
	case uid.BasicFilmBox.UID():
		filmBox, found := s.findFilmBox(instanceUID)
		if filmBox == nil {
			log.Printf("N-ACTION for unknown film box %s", instanceUID)
			return dimse.NewNActionResponse(req.MessageID(), status.NActionFailureNoSuchObjectInstance,
				classUID, instanceUID, req.ActionTypeID(), nil), nil
		}
		record, filmBoxes = found, []*printing.FilmBox{filmBox}
	case uid.BasicFilmSession.UID():
		record = s.sessions[instanceUID]
		if record == nil {
			log.Printf("N-ACTION for unknown film session %s", instanceUID)
			return dimse.NewNActionResponse(req.MessageID(), status.NActionFailureNoSuchObjectInstance,
				classUID, instanceUID, req.ActionTypeID(), nil), nil
		}
		filmBoxes = record.session.BasicFilmBoxes
	default:
		log.Printf("N-ACTION for unsupported SOP class %s", classUID)
		return dimse.NewNActionResponse(req.MessageID(), status.RefusedSOPClassNotSupported,
			classUID, instanceUID, req.ActionTypeID(), nil), nil
	}

	if record.printed {
		log.Printf("Film session %s already has a print action; persisting again", record.session.SOPInstanceUID)
	}
	record.printed = true

	folder, err := s.persist(record)
	if err != nil {
		log.Printf("Persist film session %s failed: %v", record.session.SOPInstanceUID, err)
		return dimse.NewNActionResponse(req.MessageID(), status.NActionFailureProcessingFailure,
			classUID, instanceUID, req.ActionTypeID(), nil), nil
	}

	imageBoxes := 0
	for _, filmBox := range filmBoxes {
		imageBoxes += filmBox.ImageBoxCount()
	}
	log.Printf("Print job for film session %s: %d film box(es), %d image box(es) received; persisted to %s",
		record.session.SOPInstanceUID, len(filmBoxes), imageBoxes, folder)
	return dimse.NewNActionResponse(req.MessageID(), status.NActionSuccess,
		classUID, instanceUID, req.ActionTypeID(), nil), nil
}

// ndelete removes a Film Box or Film Session after persisting it.
func (s *printServer) ndelete(_ context.Context, req *dimse.NDeleteRequest) (*dimse.NDeleteResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.verbose {
		log.Printf("N-DELETE-RQ %s", req.String())
	}

	classUID, instanceUID := req.RequestedSOPClassUID(), req.RequestedSOPInstanceUID()
	switch classUID {
	case uid.BasicFilmSession.UID():
		record := s.sessions[instanceUID]
		if record == nil {
			log.Printf("N-DELETE for unknown film session %s", instanceUID)
			return dimse.NewNDeleteResponse(req.MessageID(), status.NDeleteFailureNoSuchObjectInstance,
				classUID, instanceUID), nil
		}
		if folder, err := s.persist(record); err != nil {
			log.Printf("Persist film session %s before delete failed: %v", record.session.SOPInstanceUID, err)
		} else {
			log.Printf("Persisted film session %s to %s", record.session.SOPInstanceUID, folder)
		}
		delete(s.sessions, record.session.SOPInstanceUID)
		log.Printf("Deleted film session %s", instanceUID)
	case uid.BasicFilmBox.UID():
		filmBox, record := s.findFilmBox(instanceUID)
		if filmBox == nil || !record.session.DeleteFilmBox(filmBox.SOPInstanceUID) {
			log.Printf("N-DELETE for unknown film box %s", instanceUID)
			return dimse.NewNDeleteResponse(req.MessageID(), status.NDeleteFailureNoSuchObjectInstance,
				classUID, instanceUID), nil
		}
		log.Printf("Deleted film box %s", instanceUID)
	default:
		log.Printf("N-DELETE for unsupported SOP class %s", classUID)
		return dimse.NewNDeleteResponse(req.MessageID(), status.ErrorNoSuchSOPClass, classUID, instanceUID), nil
	}

	return dimse.NewNDeleteResponse(req.MessageID(), status.NDeleteSuccess, classUID, instanceUID), nil
}

// nget reports the printer status (Printer SOP Class) so a print SCU can check
// the printer before creating a film session.
func (s *printServer) nget(_ context.Context, req *dimse.NGetRequest) (*dimse.NGetResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.verbose {
		log.Printf("N-GET-RQ %s", req.String())
	}

	classUID, instanceUID := req.RequestedSOPClassUID(), req.RequestedSOPInstanceUID()
	if classUID != uid.Printer.UID() {
		log.Printf("N-GET for unsupported SOP class %s", classUID)
		return dimse.NewNGetResponse(req.MessageID(), status.RefusedSOPClassNotSupported, classUID, instanceUID, nil), nil
	}
	if instanceUID != uid.PrinterInstance.UID() {
		log.Printf("N-GET for unknown printer instance %s", instanceUID)
		return dimse.NewNGetResponse(req.MessageID(), status.NGetFailureNoSuchObjectInstance, classUID, instanceUID, nil), nil
	}

	attributes := dataset.New()
	if err := s.status.ApplyToDataset(attributes); err != nil {
		log.Printf("N-GET printer status failed: %v", err)
		return dimse.NewNGetResponse(req.MessageID(), status.NGetFailureProcessingFailure, classUID, instanceUID, nil), nil
	}
	log.Printf("N-GET printer status: %s", s.status.Status())
	return dimse.NewNGetResponse(req.MessageID(), status.NGetSuccess, classUID, instanceUID, attributes), nil
}

// persist writes the film session and every film box below jobs-dir/<session UID>.
func (s *printServer) persist(record *filmSessionRecord) (string, error) {
	folder := filepath.Join(s.jobsDir, record.session.SOPInstanceUID)
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return "", err
	}
	if err := record.session.Save(filepath.Join(folder, "FilmSession.dcm")); err != nil {
		return "", err
	}
	for index, filmBox := range record.session.BasicFilmBoxes {
		if err := filmBox.Save(filepath.Join(folder, fmt.Sprintf("F%06d", index+1))); err != nil {
			return "", err
		}
	}
	return folder, nil
}

func (s *printServer) filmSessionFor(attributes *dataset.Dataset) *filmSessionRecord {
	if attributes != nil {
		if sequence, err := attributes.GetSequence(tag.ReferencedFilmSessionSequence); err == nil && sequence.Count() > 0 {
			if item := sequence.GetItem(0); item != nil {
				if sessionUID, ok := item.GetString(tag.ReferencedSOPInstanceUID); ok {
					if record := s.sessions[sessionUID]; record != nil {
						return record
					}
				}
			}
		}
	}
	if len(s.sessions) == 1 {
		for _, record := range s.sessions {
			return record
		}
	}
	return nil
}

func (s *printServer) findFilmBox(sopInstanceUID string) (*printing.FilmBox, *filmSessionRecord) {
	for _, record := range s.sessions {
		if filmBox := record.session.FindFilmBox(sopInstanceUID); filmBox != nil {
			return filmBox, record
		}
	}
	return nil, nil
}

func (s *printServer) findImageBox(sopInstanceUID string) *printing.ImageBox {
	for _, record := range s.sessions {
		if imageBox := record.session.FindImageBox(sopInstanceUID); imageBox != nil {
			return imageBox
		}
	}
	return nil
}

func uint16OrZero(attributes *dataset.Dataset, t *tag.Tag) uint16 {
	value, _ := attributes.GetUInt16(t, 0)
	return value
}

func pixelByteCount(attributes *dataset.Dataset) int {
	pixelData := attributes.GetOrNil(tag.PixelData)
	if pixelData == nil || pixelData.Buffer() == nil {
		return 0
	}
	return len(pixelData.Buffer().Data())
}
