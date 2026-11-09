// Copyright (c) 2025 go-dicom-samples contributors.
// Licensed under the MIT License.

// This file holds the Go equivalent of the fo-dicom sample's finder service:
// the folder index, the query matching and the C-FIND response building.
package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/cocosip/go-dicom/pkg/dicom/dataset"
	"github.com/cocosip/go-dicom/pkg/dicom/tag"
	"github.com/cocosip/go-dicom/pkg/network/dimse"

	"github.com/cocosip/go-dicom-samples/internal/demo"
	"github.com/cocosip/go-dicom-samples/internal/store"
)

// aeRoute is where a C-MOVE destination AE title can be reached.
type aeRoute struct {
	host string
	port int
}

// indexInstances indexes dir. An empty dir, or one holding no DICOM instance,
// falls back to demo.Generate so the sample always has data.
func indexInstances(dir string) ([]store.Instance, error) {
	if dir == "" {
		dir = store.DefaultDir
	}
	instances, err := store.Index(dir)
	if err != nil {
		return nil, err
	}
	if len(instances) == 0 {
		if _, err := demo.Generate(dir); err != nil {
			return nil, err
		}
		fmt.Printf("No DICOM instance in %s; generated demo data\n", dir)
		if instances, err = store.Index(dir); err != nil {
			return nil, err
		}
	}
	fmt.Printf("Indexed %d instance(s) from %s\n", len(instances), dir)
	return instances, nil
}

// match returns every indexed instance selected by the query level and keys.
func (q *queryServer) match(level dimse.QueryRetrieveLevel, query *dataset.Dataset) []store.Instance {
	matched := make([]store.Instance, 0, len(q.instances))
	for _, instance := range q.instances {
		if answerKey(level, instance) != "" && matchesQuery(query, instance) {
			matched = append(matched, instance)
		}
	}
	return matched
}

// matchesQuery compares the query keys against one instance; an absent or empty
// key matches everything, like the fo-dicom sample.
func matchesQuery(query *dataset.Dataset, instance store.Instance) bool {
	if query == nil {
		return true
	}
	return matchKey(query, tag.PatientName, instance.PatientName) &&
		matchKey(query, tag.PatientID, instance.PatientID) &&
		matchKey(query, tag.StudyInstanceUID, instance.StudyInstanceUID) &&
		matchKey(query, tag.SeriesInstanceUID, instance.SeriesInstanceUID) &&
		matchKey(query, tag.SOPInstanceUID, instance.SOPInstanceUID) &&
		matchKey(query, tag.Modality, instance.Modality) &&
		matchKey(query, tag.StudyDate, instance.StudyDate) &&
		matchKey(query, tag.AccessionNumber, instance.AccessionNumber)
}

func matchKey(query *dataset.Dataset, t *tag.Tag, value string) bool {
	pattern, _ := query.GetString(t)
	return matchPattern(pattern, value)
}

// matchPattern applies DICOM single-value matching: "*" and "?" wildcards,
// case-insensitive comparison, empty pattern matches every value.
func matchPattern(pattern, value string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return true
	}
	expr := regexp.QuoteMeta(pattern)
	expr = strings.NewReplacer(`\*`, ".*", `\?`, ".").Replace(expr)
	matched, err := regexp.MatchString("(?i)^"+expr+"$", strings.TrimSpace(value))
	return err == nil && matched
}

// answerKey is the level identifier of one instance; an empty key means the
// instance cannot answer that level.
func answerKey(level dimse.QueryRetrieveLevel, instance store.Instance) string {
	switch level {
	case dimse.QueryRetrieveLevelPatient:
		if instance.PatientID == "" && instance.PatientName == "" {
			return ""
		}
		return instance.PatientID + "|" + instance.PatientName
	case dimse.QueryRetrieveLevelStudy:
		return instance.StudyInstanceUID
	case dimse.QueryRetrieveLevelSeries:
		if instance.StudyInstanceUID == "" || instance.SeriesInstanceUID == "" {
			return ""
		}
		return instance.StudyInstanceUID + "|" + instance.SeriesInstanceUID
	default:
		return instance.SOPInstanceUID
	}
}

// answer builds one C-FIND response from the attributes the SCU requested,
// dropping the keys this SCP cannot answer.
func (q *queryServer) answer(level dimse.QueryRetrieveLevel, query *dataset.Dataset, instance store.Instance) *dataset.Dataset {
	result := dataset.New()
	_ = result.AddValue(tag.QueryRetrieveLevel, string(level))
	if query == nil {
		return result
	}
	for _, elem := range query.Elements() {
		t := elem.Tag()
		if t.Equals(tag.QueryRetrieveLevel) {
			continue
		}
		if value := q.value(t, instance); value != "" {
			_ = result.AddValue(t, value)
		}
	}
	return result
}

// value resolves one requested attribute. ModalitiesInStudy and
// NumberOfSeriesRelatedInstances are computed from the index because a stored
// DICOM file does not carry them.
func (q *queryServer) value(t *tag.Tag, instance store.Instance) string {
	switch {
	case t.Equals(tag.PatientName):
		return instance.PatientName
	case t.Equals(tag.PatientID):
		return instance.PatientID
	case t.Equals(tag.StudyDate):
		return instance.StudyDate
	case t.Equals(tag.StudyTime):
		return instance.StudyTime
	case t.Equals(tag.AccessionNumber):
		return instance.AccessionNumber
	case t.Equals(tag.StudyDescription):
		return instance.StudyDescription
	case t.Equals(tag.Modality):
		return instance.Modality
	case t.Equals(tag.SeriesDescription):
		return instance.SeriesDescription
	case t.Equals(tag.SeriesNumber):
		return instance.SeriesNumber
	case t.Equals(tag.InstanceNumber):
		return instance.InstanceNumber
	case t.Equals(tag.StudyInstanceUID):
		return instance.StudyInstanceUID
	case t.Equals(tag.SeriesInstanceUID):
		return instance.SeriesInstanceUID
	case t.Equals(tag.SOPInstanceUID):
		return instance.SOPInstanceUID
	case t.Equals(tag.SOPClassUID):
		return instance.SOPClassUID
	case t.Equals(tag.ModalitiesInStudy):
		modalities, _ := q.aggregates(instance)
		return modalities
	case t.Equals(tag.NumberOfSeriesRelatedInstances):
		_, series := q.aggregates(instance)
		return strconv.Itoa(series)
	case t.Equals(tag.RetrieveAETitle):
		return q.ae
	}
	return ""
}

// aggregates computes the values for one study that are not stored in a DICOM
// file: the modalities of the study and the instance count of its series.
func (q *queryServer) aggregates(instance store.Instance) (modalities string, series int) {
	seen := make(map[string]bool, 2)
	unique := make([]string, 0, 2)
	for _, other := range q.instances {
		if other.StudyInstanceUID != instance.StudyInstanceUID {
			continue
		}
		if other.SeriesInstanceUID == instance.SeriesInstanceUID {
			series++
		}
		if other.Modality != "" && !seen[other.Modality] {
			seen[other.Modality] = true
			unique = append(unique, other.Modality)
		}
	}
	return strings.Join(unique, "\\"), series
}

// parseAERoutes turns "AE1=host:port,AE2=host:port" into a routing table.
func parseAERoutes(raw string) (map[string]aeRoute, error) {
	routes := make(map[string]aeRoute)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		aeTitle, address, found := strings.Cut(entry, "=")
		host, portText, hasPort := strings.Cut(strings.TrimSpace(address), ":")
		port, err := strconv.Atoi(portText)
		if !found || !hasPort || err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("entry %q: expected AE=host:port", entry)
		}
		routes[strings.TrimSpace(aeTitle)] = aeRoute{host: host, port: port}
	}
	return routes, nil
}
