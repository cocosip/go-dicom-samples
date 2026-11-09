// Copyright (c) 2025 go-dicom-samples contributors.
// Licensed under the MIT License.

package store_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cocosip/go-dicom-samples/internal/demo"
	"github.com/cocosip/go-dicom-samples/internal/store"
)

func TestDemoStoreIndexRoundTrip(t *testing.T) {
	root := t.TempDir()
	files, err := demo.Generate(root)
	if err != nil {
		t.Fatalf("demo.Generate() error = %v", err)
	}
	if len(files) == 0 {
		t.Fatal("demo.Generate() wrote no files")
	}

	instances, err := store.Index(root)
	if err != nil {
		t.Fatalf("store.Index() error = %v", err)
	}
	if len(instances) != len(files) {
		t.Fatalf("store.Index() returned %d instances, want %d", len(instances), len(files))
	}
	for _, instance := range instances {
		if instance.StudyInstanceUID == "" || instance.SOPInstanceUID == "" || instance.PatientName == "" {
			t.Fatalf("incomplete index entry: %+v", instance)
		}
	}

	// Saving a loaded dataset must produce an indexable file in the
	// <StudyInstanceUID>/<SOPInstanceUID>.dcm layout.
	ds, err := store.Load(instances[0].Path)
	if err != nil {
		t.Fatalf("store.Load() error = %v", err)
	}
	out := t.TempDir()
	saved, err := store.Save(out, ds)
	if err != nil {
		t.Fatalf("store.Save() error = %v", err)
	}
	if want := filepath.Join(out, instances[0].StudyInstanceUID, instances[0].SOPInstanceUID+".dcm"); saved != want {
		t.Fatalf("store.Save() path = %q, want %q", saved, want)
	}
	if _, err := os.Stat(saved); err != nil {
		t.Fatalf("saved file missing: %v", err)
	}

	savedInstances, err := store.Index(out)
	if err != nil {
		t.Fatalf("store.Index(saved) error = %v", err)
	}
	if len(savedInstances) != 1 || savedInstances[0].SOPInstanceUID != instances[0].SOPInstanceUID {
		t.Fatalf("store.Index(saved) = %+v", savedInstances)
	}
}
