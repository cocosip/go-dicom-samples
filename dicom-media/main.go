// Copyright (c) 2025 go-dicom-samples contributors.
// Licensed under the MIT License.

// Package main is the Go port of the fo-dicom "DICOM Media" sample. It writes a
// DICOMDIR by walking a directory and prints the patient/study/series/image
// tree stored in an existing one. When the directory holds no DICOM files, a
// small demo study is generated first so the sample runs on its own.
package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cocosip/go-dicom-samples/internal/demo"

	"github.com/cocosip/go-dicom/pkg/dicom/dataset"
	"github.com/cocosip/go-dicom/pkg/dicom/parser"
	"github.com/cocosip/go-dicom/pkg/dicom/tag"
	"github.com/cocosip/go-dicom/pkg/media"
)

const dicomDirName = "DICOMDIR"

func main() {
	if len(os.Args) < 3 {
		printUsage()
		os.Exit(2)
	}

	var err error
	switch strings.ToLower(os.Args[1]) {
	case "read":
		err = readMedia(os.Args[2])
	case "write":
		err = writeMedia(os.Args[2])
	default:
		printUsage()
		os.Exit(2)
	}
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func printUsage() {
	_, _ = fmt.Fprintln(os.Stderr, "Usage: go run ./dicom-media read|write <directory>")
}

// mediaFile is one parseable DICOM file found below the File-set root.
type mediaFile struct {
	path   string
	result *parser.ParseResult
}

// scan collects every parseable DICOM file below root, sorted by path so the
// File IDs generated for them are deterministic. The DICOMDIR itself and
// non-DICOM files are ignored.
func scan(root string) ([]mediaFile, error) {
	files := make([]mediaFile, 0, 16)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || strings.EqualFold(entry.Name(), dicomDirName) {
			return nil
		}
		result, parseErr := parser.ParseFile(path)
		if parseErr != nil {
			return nil
		}
		files = append(files, mediaFile{path: path, result: result})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	return files, err
}

// writeMedia builds <dir>/DICOMDIR from every DICOM file below dir.
func writeMedia(dir string) error {
	files, err := scan(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("scan %s: %w", dir, err)
	}

	if len(files) == 0 {
		generated, genErr := demo.Generate(dir)
		if genErr != nil {
			return fmt.Errorf("no DICOM files found and demo generation failed: %w", genErr)
		}
		fmt.Printf("No DICOM files found, generated %d demo instances in %s\n", len(generated), dir)
		if files, err = scan(dir); err != nil {
			return fmt.Errorf("scan %s: %w", dir, err)
		}
	}

	directory, err := media.NewDirectory()
	if err != nil {
		return fmt.Errorf("create DICOMDIR: %w", err)
	}

	added, failed := 0, 0
	for index, file := range files {
		// PS3.10 File IDs allow only 1-8 characters from [A-Z0-9_] per
		// component, so UID-named files cannot be referenced by name. The
		// sample uses a deterministic sequence instead.
		fileID, idErr := media.NewFileID("000001", fmt.Sprintf("%06d", index+1))
		if idErr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "skip %s: %v\n", file.path, idErr)
			failed++
			continue
		}
		if _, addErr := directory.AddFile(file.result, fileID); addErr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "skip %s: %v\n", file.path, addErr)
			failed++
			continue
		}
		added++
	}

	dicomDirPath := filepath.Join(dir, dicomDirName)
	if err := directory.Save(dicomDirPath); err != nil {
		return fmt.Errorf("save %s: %w", dicomDirPath, err)
	}

	fmt.Printf("Added %d files to the DICOMDIR (%d skipped)\n", added, failed)
	fmt.Printf("DICOMDIR written to %s\n", dicomDirPath)
	if added == 0 && len(files) > 0 {
		return errors.New("no DICOM files could be added to the DICOMDIR")
	}
	return nil
}

// readMedia prints the directory record tree stored in <dir>/DICOMDIR.
func readMedia(dir string) error {
	path := filepath.Join(dir, dicomDirName)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("DICOMDIR file not found: %s", path)
		}
		return fmt.Errorf("stat %s: %w", path, err)
	}

	directory, err := media.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	for _, record := range directory.RootRecords() {
		printRecord(record, 0)
	}
	return nil
}

// printRecord prints one directory record in the fo-dicom sample's shape and
// then recurses into its children. The record type selects the line format, so
// record types this sample does not know are still shown with their own label.
func printRecord(record *media.Record, depth int) {
	indent := strings.Repeat("\t", depth)
	attributes := record.Dataset()

	switch record.Type() {
	case media.RecordPatient:
		fmt.Printf("Patient: %s (%s)\n",
			recordString(attributes, tag.PatientName),
			recordString(attributes, tag.PatientID))
	case media.RecordStudy:
		fmt.Printf("%sStudy: %s\n", indent, recordString(attributes, tag.StudyInstanceUID))
	case media.RecordSeries:
		fmt.Printf("%sSeries: %s\n", indent, recordString(attributes, tag.SeriesInstanceUID))
	case media.RecordImage:
		fmt.Printf("%sImage: %s [%s]\n", indent,
			recordString(attributes, tag.ReferencedSOPInstanceUIDInFile),
			recordString(attributes, tag.ReferencedFileID))
	default:
		fmt.Printf("%s%s: %s\n", indent, record.Type(),
			recordString(attributes, tag.ReferencedFileID))
	}

	for _, child := range record.Children() {
		printRecord(child, depth+1)
	}
}

func recordString(attributes *dataset.Dataset, valueTag *tag.Tag) string {
	if attributes == nil {
		return ""
	}
	value, ok := attributes.GetString(valueTag)
	if !ok {
		return ""
	}
	return value
}
