// Copyright (c) 2025 go-dicom-samples contributors.
// Licensed under the MIT License.

package main

import (
	"context"
	"log"
	"strings"
	"sync"

	"github.com/cocosip/go-dicom/pkg/dicom/dataset"
	"github.com/cocosip/go-dicom/pkg/dicom/tag"
	"github.com/cocosip/go-dicom/pkg/dicom/uid"
	"github.com/cocosip/go-dicom/pkg/network/dimse"
	"github.com/cocosip/go-dicom/pkg/network/status"
)

// mppsStore keeps the performed procedure steps of this process. A real
// implementation would persist them in a database.
type mppsStore struct {
	mu    sync.Mutex
	steps map[string]*dataset.Dataset
}

func newMppsStore() *mppsStore {
	return &mppsStore{steps: make(map[string]*dataset.Dataset)}
}

// create stores a deep clone of attributes as a new performed procedure step,
// forces the status to IN PROGRESS, and reports false for a duplicate instance.
func (s *mppsStore) create(instanceUID string, attributes *dataset.Dataset) (*dataset.Dataset, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.steps[instanceUID]; exists {
		return nil, false
	}
	stored := attributes.DeepClone()
	for t, value := range map[*tag.Tag]string{
		tag.SOPClassUID:                  uid.ModalityPerformedProcedureStep.UID(),
		tag.SOPInstanceUID:               instanceUID,
		tag.PerformedProcedureStepStatus: "IN PROGRESS",
	} {
		if err := stored.AddOrUpdateValue(t, value); err != nil {
			log.Printf("store %s: %v", t, err)
		}
	}
	s.steps[instanceUID] = stored
	return stored.DeepClone(), true
}

// update merges the N-SET modification list into a stored instance.
func (s *mppsStore) update(instanceUID string, modifications *dataset.Dataset) (*dataset.Dataset, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.steps[instanceUID]
	if !ok {
		return nil, false
	}
	updated := stored.DeepClone()
	if err := updated.Merge(modifications.DeepClone(), true); err != nil {
		log.Printf("merge MPPS modification list: %v", err)
		return nil, false
	}
	s.steps[instanceUID] = updated
	return updated.DeepClone(), true
}

func (s *mppsStore) get(instanceUID string) (*dataset.Dataset, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.steps[instanceUID]
	if !ok {
		return nil, false
	}
	return stored.DeepClone(), true
}

// handleNCreate creates one in-memory performed procedure step. As in the
// fo-dicom sample, the scheduled procedure step of the request must still exist
// in the worklist, and the created step starts as IN PROGRESS.
func (s *worklistServer) handleNCreate(_ context.Context, req *dimse.NCreateRequest) (*dimse.NCreateResponse, error) {
	fail := func(st *status.Status) (*dimse.NCreateResponse, error) {
		return dimse.NewNCreateResponse(req.MessageID(), st,
			req.AffectedSOPClassUID(), req.AffectedSOPInstanceUID(), nil), nil
	}

	if req.AffectedSOPClassUID() != uid.ModalityPerformedProcedureStep.UID() {
		log.Printf("N-CREATE rejected: SOP class %s is not supported", req.AffectedSOPClassUID())
		return fail(status.RefusedSOPClassNotSupported)
	}
	attributes := req.DataDataset()
	if attributes == nil {
		return fail(status.NCreateFailureMissingAttributeValue)
	}

	instanceUID := req.AffectedSOPInstanceUID()
	if instanceUID == "" {
		instanceUID = uid.GenerateDerivedFromUUID().UID()
	}
	log.Printf("receiving N-Create with SOPUID %s", instanceUID)

	stepID := queryString(firstSequenceItem(attributes, tag.ScheduledStepAttributesSequence),
		tag.ScheduledProcedureStepID)
	if stepID == "" {
		log.Printf("N-CREATE rejected: no ScheduledProcedureStepID in ScheduledStepAttributesSequence")
		return fail(status.NCreateFailureMissingAttributeValue)
	}
	item, ok := s.findByScheduledStepID(stepID)
	if !ok {
		log.Printf("N-CREATE rejected: scheduled procedure step %s is unknown", stepID)
		return fail(status.NCreateFailureProcessingFailure)
	}

	created, ok := s.mpps.create(instanceUID, attributes)
	if !ok {
		log.Printf("N-CREATE rejected: SOP instance %s already exists", instanceUID)
		return fail(status.NCreateFailureDuplicateSOPInstance)
	}

	log.Printf("Procedure with id %s of Patient %s is started", item.spsID, item.patientName)
	return dimse.NewNCreateResponse(req.MessageID(), status.Success,
		req.AffectedSOPClassUID(), instanceUID, created), nil
}

// handleNSet completes or discontinues a performed procedure step.
func (s *worklistServer) handleNSet(_ context.Context, req *dimse.NSetRequest) (*dimse.NSetResponse, error) {
	sopClassUID := req.RequestedSOPClassUID()
	instanceUID := req.RequestedSOPInstanceUID()
	fail := func(st *status.Status) (*dimse.NSetResponse, error) {
		return dimse.NewNSetResponse(req.MessageID(), st, sopClassUID, instanceUID, nil), nil
	}

	if sopClassUID != uid.ModalityPerformedProcedureStep.UID() {
		log.Printf("N-SET rejected: SOP class %s is not supported", sopClassUID)
		return fail(status.RefusedSOPClassNotSupported)
	}
	modifications := req.DataDataset()
	if modifications == nil {
		return fail(status.NSetFailureMissingAttribute)
	}
	log.Printf("receiving N-Set with SOPUID %s", instanceUID)

	stepStatus := strings.ToUpper(queryString(modifications, tag.PerformedProcedureStepStatus))
	switch stepStatus {
	case "COMPLETED", "DISCONTINUED":
	default:
		log.Printf("N-SET rejected: status %q is not COMPLETED or DISCONTINUED", stepStatus)
		return fail(status.NSetFailureInvalidAttributeValue)
	}

	if _, ok := s.mpps.update(instanceUID, modifications); !ok {
		log.Printf("N-SET rejected: unknown SOP instance %s", instanceUID)
		return fail(status.NSetFailureNoSuchObjectInstance)
	}
	log.Printf("Procedure with SOPUID %s is %s", instanceUID, strings.ToLower(stepStatus))
	if stepStatus == "COMPLETED" {
		logPerformedSeries(instanceUID, modifications)
	}

	return dimse.NewNSetResponse(req.MessageID(), status.Success, sopClassUID, instanceUID, nil), nil
}

// handleNGet returns the stored attributes of one performed procedure step.
// The requested AttributeIdentifierList is ignored; the whole instance is
// returned, which is all the sample needs.
func (s *worklistServer) handleNGet(_ context.Context, req *dimse.NGetRequest) (*dimse.NGetResponse, error) {
	sopClassUID := req.RequestedSOPClassUID()
	instanceUID := req.RequestedSOPInstanceUID()

	if sopClassUID != uid.ModalityPerformedProcedureStep.UID() {
		log.Printf("N-GET rejected: SOP class %s is not supported", sopClassUID)
		return dimse.NewNGetResponse(req.MessageID(), status.RefusedSOPClassNotSupported,
			sopClassUID, instanceUID, nil), nil
	}

	attributes, ok := s.mpps.get(instanceUID)
	if !ok {
		log.Printf("N-GET rejected: unknown SOP instance %s", instanceUID)
		return dimse.NewNGetResponse(req.MessageID(), status.NGetFailureNoSuchObjectInstance,
			sopClassUID, instanceUID, nil), nil
	}

	log.Printf("N-GET returning SOP instance %s with status %s", instanceUID,
		queryString(attributes, tag.PerformedProcedureStepStatus))
	return dimse.NewNGetResponse(req.MessageID(), status.Success, sopClassUID, instanceUID, attributes), nil
}

// logPerformedSeries reports the vendor-supplied series and instance UIDs of a
// COMPLETED message, which the fo-dicom sample stores in its database.
func logPerformedSeries(instanceUID string, modifications *dataset.Dataset) {
	series, err := modifications.GetSequence(tag.PerformedSeriesSequence)
	if err != nil || series == nil {
		return
	}
	instances := 0
	for _, item := range series.GetItems() {
		images, err := item.GetSequence(tag.ReferencedImageSequence)
		if err != nil || images == nil {
			continue
		}
		instances += images.Count()
	}
	log.Printf("COMPLETED procedure %s contains %d series and %d referenced instances",
		instanceUID, series.Count(), instances)
}
