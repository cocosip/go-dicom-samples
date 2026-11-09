// Copyright (c) 2025 go-dicom-samples contributors.
// Licensed under the MIT License.

// Package demo generates small DICOM studies so the samples can be run
// end-to-end without any external data set.
package demo

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/cocosip/go-dicom/pkg/dicom/dataset"
	"github.com/cocosip/go-dicom/pkg/dicom/element"
	"github.com/cocosip/go-dicom/pkg/dicom/tag"
	"github.com/cocosip/go-dicom/pkg/dicom/transfer"
	"github.com/cocosip/go-dicom/pkg/dicom/uid"
	"github.com/cocosip/go-dicom/pkg/dicom/writer"
)

const (
	imageRows    = 64
	imageColumns = 64
)

type patient struct {
	name, id, birthDate, sex string
}

type study struct {
	uid, accession, description string
	series                      []series
}

type series struct {
	description string
	modality    string
	instances   int
}

var patients = []struct {
	who patient
	st  study
}{
	{
		who: patient{name: "Doe^John", id: "10001", birthDate: "19800101", sex: "M"},
		st: study{
			uid:         "1.2.826.0.1.3680043.10.999.1.1",
			accession:   "ACC10001",
			description: "CT Chest",
			series: []series{
				{description: "Axial", modality: "CT", instances: 3},
				{description: "Scout", modality: "CT", instances: 1},
			},
		},
	},
	{
		who: patient{name: "Traxler^Yvonne", id: "10002", birthDate: "19720516", sex: "F"},
		st: study{
			uid:         "1.2.826.0.1.3680043.10.999.2.1",
			accession:   "ACC10002",
			description: "MR Brain",
			series: []series{
				{description: "T1", modality: "MR", instances: 2},
				{description: "T2", modality: "MR", instances: 2},
			},
		},
	},
}

// Generate writes the demo studies below root using the same
// <StudyInstanceUID>/<SOPInstanceUID>.dcm layout as internal/store, and returns
// every written file path.
func Generate(root string) ([]string, error) {
	now := time.Now()
	files := make([]string, 0, 12)

	for _, p := range patients {
		studyDir := filepath.Join(root, p.st.uid)
		if err := os.MkdirAll(studyDir, 0o755); err != nil {
			return nil, err
		}
		for seriesIndex, sr := range p.st.series {
			seriesUID := fmt.Sprintf("%s.%d", p.st.uid, seriesIndex+1)
			for instanceIndex := 0; instanceIndex < sr.instances; instanceIndex++ {
				instanceUID := fmt.Sprintf("%s.%d", seriesUID, instanceIndex+1)
				ds, err := newImage(p.who, p.st, sr, seriesUID, instanceUID, seriesIndex+1, instanceIndex+1, now)
				if err != nil {
					return nil, err
				}
				path := filepath.Join(studyDir, instanceUID+".dcm")
				if err := writer.WriteFile(path, ds); err != nil {
					return nil, err
				}
				files = append(files, path)
			}
		}
	}

	return files, nil
}

func newImage(p patient, st study, sr series, seriesUID, instanceUID string, seriesNumber, instanceNumber int, now time.Time) (*dataset.Dataset, error) {
	ds := dataset.NewWithTransferSyntax(transfer.ExplicitVRLittleEndian)

	values := []struct {
		t     *tag.Tag
		value any
	}{
		{tag.SOPClassUID, uid.CTImageStorage.UID()},
		{tag.SOPInstanceUID, instanceUID},
		{tag.StudyInstanceUID, st.uid},
		{tag.SeriesInstanceUID, seriesUID},
		{tag.PatientName, p.name},
		{tag.PatientID, p.id},
		{tag.PatientBirthDate, p.birthDate},
		{tag.PatientSex, p.sex},
		{tag.StudyDate, now.Format("20060102")},
		{tag.StudyTime, now.Format("150405")},
		{tag.AccessionNumber, st.accession},
		{tag.StudyDescription, st.description},
		{tag.StudyID, "1"},
		{tag.Modality, sr.modality},
		{tag.SeriesNumber, strconv.Itoa(seriesNumber)},
		{tag.SeriesDescription, sr.description},
		{tag.InstanceNumber, strconv.Itoa(instanceNumber)},
		{tag.Rows, uint16(imageRows)},
		{tag.Columns, uint16(imageColumns)},
		{tag.SamplesPerPixel, uint16(1)},
		{tag.PhotometricInterpretation, "MONOCHROME2"},
		{tag.BitsAllocated, uint16(8)},
		{tag.BitsStored, uint16(8)},
		{tag.HighBit, uint16(7)},
		{tag.PixelRepresentation, uint16(0)},
	}
	for _, v := range values {
		if err := ds.AddValue(v.t, v.value); err != nil {
			return nil, fmt.Errorf("add %s: %w", v.t, err)
		}
	}

	pixels := make([]byte, imageRows*imageColumns)
	for i := range pixels {
		pixels[i] = byte((i + instanceNumber*17 + seriesNumber*31) % 256)
	}
	if err := ds.Add(element.NewOtherByte(tag.PixelData, pixels)); err != nil {
		return nil, fmt.Errorf("add pixel data: %w", err)
	}

	return ds, nil
}
