// Copyright (c) 2025 go-dicom-samples contributors.
// Licensed under the MIT License.

package main

import (
	"context"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/cocosip/go-dicom/pkg/dicom/dataset"
	"github.com/cocosip/go-dicom/pkg/dicom/tag"
	"github.com/cocosip/go-dicom/pkg/network/service"
	"github.com/cocosip/go-dicom/pkg/network/status"
)

// worklistItem is the subset of a scheduled procedure step served by this
// sample. A real RIS would load these rows from a database.
type worklistItem struct {
	patientName  string
	patientID    string
	birthDate    string
	sex          string
	accession    string
	studyUID     string
	modality     string
	spsID        string
	spsDesc      string
	stationAE    string
	stationName  string
	location     string
	startDate    string
	startTime    string
	requestedID  string
	requestedDes string
	referring    string
	performing   string
}

// danielsMD is the physician of two scheduled procedure steps.
const danielsMD = "Daniels^Jack^Md"

// worklistItems returns the hard-coded worklist of the fo-dicom sample.
func worklistItems(now time.Time) []worklistItem {
	today := now.Format("20060102")
	tomorrow := now.AddDate(0, 0, 1).Format("20060102")

	return []worklistItem{{
		patientName: "Doe^John", patientID: "10001", birthDate: "19800101", sex: "M",
		accession: "AB123", studyUID: "1.2.826.0.1.3680043.10.999.1.1",
		modality: "CT", spsID: "200002", spsDesc: "CT Chest",
		stationAE: "WORKLISTSCU", stationName: "CT1", location: "Room A",
		startDate: today, startTime: "090000",
		requestedID: "200001", requestedDes: "CT Chest",
		referring: "Smith^John^Md", performing: "Smith^John^Md",
	},
		{
			patientName: "Traxler^Yvonne", patientID: "10002", birthDate: "19720516", sex: "F",
			accession: "AB124", studyUID: "1.2.826.0.1.3680043.10.999.2.1",
			modality: "MR", spsID: "200004", spsDesc: "MR Brain",
			stationAE: "WORKLISTSCU", stationName: "MR1", location: "Room B",
			startDate: today, startTime: "100000",
			requestedID: "200003", requestedDes: "MR Brain",
			referring: danielsMD, performing: danielsMD,
		},
		{
			patientName: "Miller^Albert", patientID: "10003", birthDate: "19841002", sex: "M",
			accession: "AB125", studyUID: "1.2.826.0.1.3680043.10.999.3.1",
			modality: "CR", spsID: "200006", spsDesc: "Chest PA",
			stationAE: "CRMODALITY", stationName: "CR2", location: "Room C",
			startDate: tomorrow, startTime: "083000",
			requestedID: "200005", requestedDes: "Chest PA",
			referring: danielsMD, performing: danielsMD,
		},
	}
}

// worklistServer answers the Modality Worklist C-FIND and owns the MPPS store.
type worklistServer struct {
	items   []worklistItem
	verbose bool
	mpps    *mppsStore
}

func newWorklistServer(items []worklistItem, verbose bool) *worklistServer {
	return &worklistServer{items: items, verbose: verbose, mpps: newMppsStore()}
}

// handleCFind returns every item that matches the query identifier and finishes
// with a Success status, like WorklistHandler.FilterWorklistItems in fo-dicom.
func (s *worklistServer) handleCFind(_ context.Context, op service.CFindOperation) error {
	query := op.Identifier()
	matched := s.filter(query)

	if s.verbose {
		log.Printf("C-FIND level=%s matched=%d", op.QueryLevel(), len(matched))
	}

	for _, item := range matched {
		if err := op.SendPending(item.identifier(query)); err != nil {
			return err
		}
	}
	return op.SendFinal(status.Success)
}

func (s *worklistServer) filter(query *dataset.Dataset) []worklistItem {
	matched := make([]worklistItem, 0, len(s.items))
	for _, item := range s.items {
		if item.matches(query) {
			matched = append(matched, item)
		}
	}
	return matched
}

func (s *worklistServer) findByScheduledStepID(stepID string) (worklistItem, bool) {
	for _, item := range s.items {
		if item.spsID == stepID {
			return item, true
		}
	}
	return worklistItem{}, false
}

// matches applies the matching keys of the query. Empty values and "*" match
// everything, and "*"/"?" wildcards are supported, as in the fo-dicom sample.
func (it worklistItem) matches(query *dataset.Dataset) bool {
	if query == nil {
		return true
	}
	if !matchValue(queryString(query, tag.PatientName), it.patientName) ||
		!matchValue(queryString(query, tag.PatientID), it.patientID) ||
		!matchValue(queryString(query, tag.AccessionNumber), it.accession) ||
		!matchValue(queryString(query, tag.StudyInstanceUID), it.studyUID) {
		return false
	}

	sps := firstSequenceItem(query, tag.ScheduledProcedureStepSequence)
	if sps == nil {
		return true
	}
	if !matchValue(queryString(sps, tag.ScheduledStationAETitle), it.stationAE) ||
		!matchValue(queryString(sps, tag.Modality), it.modality) ||
		!matchValue(queryString(sps, tag.ScheduledProcedureStepID), it.spsID) ||
		!matchValue(queryString(sps, tag.ScheduledProcedureStepDescription), it.spsDesc) ||
		!matchValue(queryString(sps, tag.ScheduledStationName), it.stationName) ||
		!matchValue(queryString(sps, tag.ScheduledProcedureStepLocation), it.location) ||
		!matchValue(queryString(sps, tag.ScheduledPerformingPhysicianName), it.performing) {
		return false
	}
	return matchRangeValue(queryString(sps, tag.ScheduledProcedureStepStartDate), it.startDate) &&
		matchRangeValue(queryString(sps, tag.ScheduledProcedureStepStartTime), it.startTime)
}

// identifier builds the C-FIND response for one item. Only attributes present
// in the request identifier are returned, as in WorklistHandler.AddIfExistsInRequest.
func (it worklistItem) identifier(query *dataset.Dataset) *dataset.Dataset {
	result := dataset.New()

	if spsQuery := firstSequenceItem(query, tag.ScheduledProcedureStepSequence); spsQuery != nil {
		sps := dataset.New()
		addIfRequested(sps, spsQuery, tag.ScheduledStationAETitle, it.stationAE)
		addIfRequested(sps, spsQuery, tag.ScheduledProcedureStepStartDate, it.startDate)
		addIfRequested(sps, spsQuery, tag.ScheduledProcedureStepStartTime, it.startTime)
		addIfRequested(sps, spsQuery, tag.Modality, it.modality)
		addIfRequested(sps, spsQuery, tag.ScheduledPerformingPhysicianName, it.performing)
		addIfRequested(sps, spsQuery, tag.ScheduledProcedureStepDescription, it.spsDesc)
		addIfRequested(sps, spsQuery, tag.ScheduledProcedureStepID, it.spsID)
		addIfRequested(sps, spsQuery, tag.ScheduledStationName, it.stationName)
		addIfRequested(sps, spsQuery, tag.ScheduledProcedureStepLocation, it.location)

		sequence := dataset.NewSequence(tag.ScheduledProcedureStepSequence)
		sequence.AddItem(sps)
		if err := result.Add(sequence); err != nil {
			log.Printf("add ScheduledProcedureStepSequence: %v", err)
		}
	}

	addIfRequested(result, query, tag.AccessionNumber, it.accession)
	addIfRequested(result, query, tag.InstitutionName, "")
	addIfRequested(result, query, tag.ReferringPhysicianName, it.referring)
	addIfRequested(result, query, tag.PatientName, it.patientName)
	addIfRequested(result, query, tag.PatientID, it.patientID)
	addIfRequested(result, query, tag.PatientBirthDate, it.birthDate)
	addIfRequested(result, query, tag.PatientSex, it.sex)
	addIfRequested(result, query, tag.StudyInstanceUID, it.studyUID)
	addIfRequested(result, query, tag.RequestingPhysician, it.referring)
	addIfRequested(result, query, tag.RequestedProcedureDescription, it.requestedDes)
	addIfRequested(result, query, tag.RequestedProcedureID, it.requestedID)

	// Type 2 attributes this sample does not support: return the requested key
	// with an empty value instead of omitting it.
	addIfRequested(result, query, tag.PatientTransportArrangements, "")
	addIfRequested(result, query, tag.AdmissionID, "")
	addIfRequested(result, query, tag.CurrentPatientLocation, "")
	addIfRequested(result, query, tag.PatientWeight, "")
	addIfRequested(result, query, tag.ConfidentialityConstraintOnPatientDataDescription, "")
	addEmptySequenceIfRequested(result, query, tag.ReferencedStudySequence)
	addEmptySequenceIfRequested(result, query, tag.ReferencedPatientSequence)
	if query != nil && query.Contains(tag.Priority) {
		if err := result.AddValue(tag.Priority, uint16(0)); err != nil {
			log.Printf("add Priority: %v", err)
		}
	}

	return result
}

func addIfRequested(dst, query *dataset.Dataset, t *tag.Tag, value string) {
	if query == nil || !query.Contains(t) {
		return
	}
	if err := dst.AddOrUpdateValue(t, value); err != nil {
		log.Printf("add %s to C-FIND response: %v", t, err)
	}
}

func addEmptySequenceIfRequested(dst, query *dataset.Dataset, t *tag.Tag) {
	if query == nil || !query.Contains(t) {
		return
	}
	if err := dst.Add(dataset.NewSequence(t)); err != nil {
		log.Printf("add %s to C-FIND response: %v", t, err)
	}
}

// firstSequenceItem returns the first item of a sequence attribute, or nil.
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

func queryString(ds *dataset.Dataset, t *tag.Tag) string {
	if ds == nil {
		return ""
	}
	return strings.TrimSpace(ds.TryGetString(t))
}

// matchValue reports whether a DICOM matching key matches value. An empty key
// or "*" matches everything; "*" and "?" are wildcards.
func matchValue(pattern, value string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" || pattern == "*" {
		return true
	}
	value = strings.TrimSpace(value)
	if !strings.ContainsAny(pattern, "*?") {
		return strings.EqualFold(pattern, value)
	}
	re, err := regexp.Compile("^" + wildcardPattern(pattern) + "$")
	if err != nil {
		return false
	}
	return re.MatchString(strings.ToUpper(value))
}

// matchRangeValue matches a single value or a "from-to" range, which is how
// MWL dates and times are queried (YYYYMMDD-YYYYMMDD, HHMMSS-HHMMSS).
func matchRangeValue(pattern, value string) bool {
	pattern = strings.TrimSpace(pattern)
	if start, end, ok := strings.Cut(pattern, "-"); ok {
		value = strings.TrimSpace(value)
		if start != "" && value < start {
			return false
		}
		if end != "" && value > end {
			return false
		}
		return true
	}
	return matchValue(pattern, value)
}

func wildcardPattern(pattern string) string {
	var builder strings.Builder
	for _, r := range strings.ToUpper(pattern) {
		switch r {
		case '*':
			builder.WriteString(".*")
		case '?':
			builder.WriteByte('.')
		default:
			builder.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	return builder.String()
}
