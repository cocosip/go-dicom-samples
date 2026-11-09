// Copyright (c) 2025 go-dicom-samples contributors.
// Licensed under the MIT License.

// Package store saves received DICOM instances and indexes a folder so the
// Q/R samples can answer queries and retrievals from local files.
package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cocosip/go-dicom/pkg/dicom/dataset"
	"github.com/cocosip/go-dicom/pkg/dicom/parser"
	"github.com/cocosip/go-dicom/pkg/dicom/tag"
	"github.com/cocosip/go-dicom/pkg/dicom/writer"
)

// DefaultDir is the storage folder used by the samples when none is given.
const DefaultDir = "DICOM"

// Save writes ds to root/<StudyInstanceUID>/<SOPInstanceUID>.dcm, creating
// folders as needed. The dataset keeps its own transfer syntax, so compressed
// instances are stored exactly as they were received.
func Save(root string, ds *dataset.Dataset) (string, error) {
	studyUID, ok := ds.GetString(tag.StudyInstanceUID)
	if !ok || strings.TrimSpace(studyUID) == "" {
		return "", errors.New("dataset has no StudyInstanceUID")
	}
	instanceUID, ok := ds.GetString(tag.SOPInstanceUID)
	if !ok || strings.TrimSpace(instanceUID) == "" {
		return "", errors.New("dataset has no SOPInstanceUID")
	}

	dir := filepath.Join(root, strings.TrimSpace(studyUID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, strings.TrimSpace(instanceUID)+".dcm")
	if err := writer.WriteFile(path, ds); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// Load parses a DICOM file.
func Load(path string) (*dataset.Dataset, error) {
	result, err := parser.ParseFile(path)
	if err != nil {
		return nil, err
	}
	return result.Dataset, nil
}

// Instance is the metadata needed to answer a query for one stored file.
type Instance struct {
	Path              string
	SOPClassUID       string
	SOPInstanceUID    string
	StudyInstanceUID  string
	SeriesInstanceUID string
	PatientName       string
	PatientID         string
	StudyDate         string
	StudyTime         string
	AccessionNumber   string
	StudyDescription  string
	Modality          string
	SeriesDescription string
	SeriesNumber      string
	InstanceNumber    string
	NumberOfFrames    string
}

// Index parses every DICOM file below root and returns the instances sorted by
// path. Files that cannot be parsed are skipped, so a folder that also holds
// non-DICOM files can be indexed directly.
func Index(root string) ([]Instance, error) {
	instances := make([]Instance, 0, 16)

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}

		result, parseErr := parser.ParseFile(path)
		if parseErr != nil {
			return nil
		}

		instances = append(instances, Instance{
			Path:              path,
			SOPClassUID:       stringValue(result.Dataset, tag.SOPClassUID),
			SOPInstanceUID:    stringValue(result.Dataset, tag.SOPInstanceUID),
			StudyInstanceUID:  stringValue(result.Dataset, tag.StudyInstanceUID),
			SeriesInstanceUID: stringValue(result.Dataset, tag.SeriesInstanceUID),
			PatientName:       stringValue(result.Dataset, tag.PatientName),
			PatientID:         stringValue(result.Dataset, tag.PatientID),
			StudyDate:         stringValue(result.Dataset, tag.StudyDate),
			StudyTime:         stringValue(result.Dataset, tag.StudyTime),
			AccessionNumber:   stringValue(result.Dataset, tag.AccessionNumber),
			StudyDescription:  stringValue(result.Dataset, tag.StudyDescription),
			Modality:          stringValue(result.Dataset, tag.Modality),
			SeriesDescription: stringValue(result.Dataset, tag.SeriesDescription),
			SeriesNumber:      stringValue(result.Dataset, tag.SeriesNumber),
			InstanceNumber:    stringValue(result.Dataset, tag.InstanceNumber),
			NumberOfFrames:    stringValue(result.Dataset, tag.NumberOfFrames),
		})
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	sort.Slice(instances, func(i, j int) bool { return instances[i].Path < instances[j].Path })
	return instances, nil
}

func stringValue(ds *dataset.Dataset, t *tag.Tag) string {
	value, ok := ds.GetString(t)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}
