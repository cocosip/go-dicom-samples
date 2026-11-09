// Copyright (c) 2025 go-dicom-samples contributors.
// Licensed under the MIT License.

// Package main is the Go port of the fo-dicom "C-Store SCU" sample: it sends
// DICOM files to a storage SCP with C-STORE. When no files are given, a small
// demo study is generated so the sample runs on its own.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "github.com/cocosip/go-dicom-samples/internal/codecs"
	"github.com/cocosip/go-dicom-samples/internal/demo"

	"github.com/cocosip/go-dicom/pkg/dicom/dataset"
	"github.com/cocosip/go-dicom/pkg/dicom/parser"
	"github.com/cocosip/go-dicom/pkg/dicom/tag"
	"github.com/cocosip/go-dicom/pkg/dicom/transfer"
	"github.com/cocosip/go-dicom/pkg/network/client"
)

var (
	host      = flag.String("host", "localhost", "storage SCP host")
	port      = flag.Int("port", 11112, "storage SCP port")
	calledAE  = flag.String("called-ae", "STORESCP", "called AE title")
	callingAE = flag.String("calling-ae", "FODICOMSCU", "calling AE title")
)

// instance is one parsed input file ready to be stored.
type instance struct {
	path string
	ds   *dataset.Dataset
	ts   *transfer.Syntax
}

func main() {
	flag.Parse()

	files := flag.Args()
	if len(files) == 0 {
		generated, cleanup, err := generateDemoFiles()
		if err != nil {
			log.Fatalf("no files given and demo generation failed: %v", err)
		}
		defer cleanup()
		files = generated
		fmt.Printf("No files given, generated %d demo instances\n", len(files))
	}

	instances, err := loadInstances(files)
	if err != nil {
		log.Fatalf("load input files: %v", err)
	}

	fmt.Println("***************************************************")
	fmt.Printf("Server AE Title:     %s\n", *calledAE)
	fmt.Printf("Server Host Address: %s\n", *host)
	fmt.Printf("Server Port:         %d\n", *port)
	fmt.Printf("Client AE Title:     %s\n", *callingAE)
	fmt.Println("***************************************************")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	c := client.New(
		client.WithCallingAE(*callingAE),
		client.WithCalledAE(*calledAE),
		client.WithConnectTimeout(10*time.Second),
		client.WithRequestTimeout(60*time.Second),
	)
	if err := addPresentationContexts(c, instances); err != nil {
		log.Fatalf("configure presentation contexts: %v", err)
	}
	if err := c.Connect(ctx, *host, *port); err != nil {
		log.Fatalf("connect to %s:%d: %v", *host, *port, err)
	}
	defer func() { _ = c.Close() }()

	failed := 0
	for _, in := range instances {
		if err := c.CStore(ctx, in.ds); err != nil {
			failed++
			fmt.Printf("C-STORE %s failed: %v\n", filepath.Base(in.path), err)
			continue
		}
		patientName, _ := in.ds.GetString(tag.PatientName)
		fmt.Printf("C-STORE %s (%s) succeeded\n", filepath.Base(in.path), patientName)
	}

	if failed > 0 {
		log.Fatalf("%d of %d instances failed", failed, len(instances))
	}
	fmt.Printf("Sent %d instances successfully\n", len(instances))
}

// generateDemoFiles writes a demo study to a temporary folder and returns the
// files plus a cleanup function.
func generateDemoFiles() ([]string, func(), error) {
	dir, err := os.MkdirTemp("", "go-dicom-samples-*")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	files, err := demo.Generate(dir)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return files, cleanup, nil
}

func loadInstances(files []string) ([]instance, error) {
	instances := make([]instance, 0, len(files))
	for _, file := range files {
		result, err := parser.ParseFile(file)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		ts := result.TransferSyntax
		if ts == nil {
			ts = transfer.ExplicitVRLittleEndian
		}
		instances = append(instances, instance{path: file, ds: result.Dataset, ts: ts})
	}
	return instances, nil
}

// addPresentationContexts proposes each SOP Class once with the instance's own
// transfer syntax first, followed by the uncompressed fallbacks.
func addPresentationContexts(c *client.Client, instances []instance) error {
	proposed := make(map[string]struct{})
	for _, in := range instances {
		sopClassUID, ok := in.ds.GetString(tag.SOPClassUID)
		if !ok || strings.TrimSpace(sopClassUID) == "" {
			return fmt.Errorf("%s has no SOPClassUID", in.path)
		}
		if _, exists := proposed[sopClassUID]; exists {
			continue
		}
		proposed[sopClassUID] = struct{}{}

		syntaxes := []string{
			in.ts.UID().UID(),
			transfer.ExplicitVRLittleEndian.UID().UID(),
			transfer.ImplicitVRLittleEndian.UID().UID(),
		}
		if err := c.AddPresentationContext(sopClassUID, syntaxes...); err != nil {
			return fmt.Errorf("%s (%s): %w", in.path, sopClassUID, err)
		}
	}
	return nil
}
