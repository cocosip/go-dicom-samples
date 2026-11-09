// Copyright (c) 2025 go-dicom-samples contributors.
// Licensed under the MIT License.

// Command qr-scu is the Go port of the fo-dicom "QueryRetreive SCU" sample: it
// finds a study, lists its series, retrieves one series with C-GET and, when
// -move-dest is set, moves the study to that destination with C-MOVE.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cocosip/go-dicom/pkg/dicom/dataset"
	"github.com/cocosip/go-dicom/pkg/dicom/tag"
	"github.com/cocosip/go-dicom/pkg/dicom/uid"
	"github.com/cocosip/go-dicom/pkg/network/client"
	"github.com/cocosip/go-dicom/pkg/network/dimse"
	"github.com/cocosip/go-dicom/pkg/network/status"

	_ "github.com/cocosip/go-dicom-samples/internal/codecs"
	"github.com/cocosip/go-dicom-samples/internal/store"
)

var (
	host      = flag.String("host", "localhost", "Query/Retrieve SCP host")
	port      = flag.Int("port", 11113, "Query/Retrieve SCP port")
	calledAE  = flag.String("called-ae", "QRSCP", "called AE title")
	callingAE = flag.String("calling-ae", "FODICOMSCU", "calling AE title")
	patient   = flag.String("patient", "", "PatientName match key (* and ? wildcards; empty matches every patient)")
	studyUID  = flag.String("study-uid", "", "StudyInstanceUID match key; empty uses the first study found")
	seriesUID = flag.String("series-uid", "", "SeriesInstanceUID to retrieve; empty uses the first series found")
	moveDest  = flag.String("move-dest", "", "move the study to this destination AE with C-MOVE (empty: no C-MOVE)")
	dataDir   = flag.String("data-dir", store.DefaultDir, "folder where C-GET instances are saved")
)

// savedFiles counts the C-STORE sub-operations written during C-GET.
var savedFiles atomic.Int32

func main() {
	flag.Parse()

	if strings.TrimSpace(*dataDir) == "" {
		log.Fatal("-data-dir must not be empty")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	fmt.Printf("Query/Retrieve SCU -> %s:%d (called AE %s, calling AE %s)\n", *host, *port, *calledAE, *callingAE)

	c, err := connect(ctx)
	if err != nil {
		log.Fatalf("cannot associate with %s:%d: %v", *host, *port, err)
	}
	defer func() { _ = c.Close() }()

	// 1. Find the studies.
	studies, err := findStudies(ctx, c)
	if err != nil {
		log.Fatalf("C-FIND study failed: %v", err)
	}
	study := strings.TrimSpace(*studyUID)
	if study == "" && len(studies) > 0 {
		study = studies[0]
	}
	if study == "" {
		fmt.Println("No study matched; nothing to retrieve.")
		return
	}
	fmt.Printf("Using study %s\n", study)

	// 2. Find the series of that study.
	series, err := findSeries(ctx, c, study)
	if err != nil {
		log.Fatalf("C-FIND series failed: %v", err)
	}
	selectedSeries := strings.TrimSpace(*seriesUID)
	if selectedSeries == "" && len(series) > 0 {
		selectedSeries = series[0]
	}
	if selectedSeries == "" {
		fmt.Println("No series matched; nothing to retrieve.")
		return
	}
	fmt.Printf("Using series %s\n", selectedSeries)

	// 3. Retrieve that series over the same association and save it locally.
	if err := retrieveSeries(ctx, c, study, selectedSeries); err != nil {
		log.Fatalf("C-GET failed: %v", err)
	}
	fmt.Printf("C-GET saved %d instance(s) under %s\n", savedFiles.Load(), *dataDir)

	// 4. Optionally move the whole study to a third-party Storage SCP.
	if destination := strings.TrimSpace(*moveDest); destination != "" {
		if err := moveStudy(ctx, c, study, destination); err != nil {
			fmt.Printf("Error sending datasets: %v\n", err)
			return
		}
		fmt.Println("Sending successfully finished")
	}
}

// connect opens one association carrying the Query/Retrieve presentation
// contexts, the C-STORE handler used by C-GET, and the SCP-role storage
// contexts the SCP needs to push instances back.
func connect(ctx context.Context) (*client.Client, error) {
	transferSyntaxes := []string{uid.ExplicitVRLittleEndian.UID(), uid.ImplicitVRLittleEndian.UID()}
	c := client.New(
		client.WithCallingAE(strings.TrimSpace(*callingAE)),
		client.WithCalledAE(strings.TrimSpace(*calledAE)),
		client.WithConnectTimeout(10*time.Second),
		client.WithRequestTimeout(60*time.Second),
		client.WithCStoreHandler(handleCStore),
	)
	for _, sopClass := range []*uid.UID{
		uid.StudyRootQueryRetrieveInformationModelFind,
		uid.StudyRootQueryRetrieveInformationModelGet,
		uid.StudyRootQueryRetrieveInformationModelMove,
	} {
		if err := c.AddPresentationContext(sopClass.UID(), transferSyntaxes...); err != nil {
			return nil, err
		}
	}

	// Every SOP class the SCP may return needs an SCP-role context. Extend the
	// list when retrieving other modalities.
	storageClasses := []*uid.UID{
		uid.CTImageStorage,
		uid.MRImageStorage,
		uid.SecondaryCaptureImageStorage,
		uid.UltrasoundImageStorage,
		uid.XRayAngiographicImageStorage,
	}
	storageUIDs := make([]string, 0, len(storageClasses))
	for _, sopClass := range storageClasses {
		storageUIDs = append(storageUIDs, sopClass.UID())
	}
	if err := c.AddStoragePresentationContexts(storageUIDs, transferSyntaxes...); err != nil {
		return nil, err
	}

	if err := c.Connect(ctx, *host, *port); err != nil {
		return nil, err
	}
	return c, nil
}

// queryKey is one attribute of a C-FIND identifier.
type queryKey struct {
	t     *tag.Tag
	value string
}

func queryDataset(keys ...queryKey) *dataset.Dataset {
	query := dataset.New()
	for _, key := range keys {
		if err := query.AddValue(key.t, key.value); err != nil {
			log.Fatalf("cannot build the query: %v", err)
		}
	}
	return query
}

func findStudies(ctx context.Context, c *client.Client) ([]string, error) {
	fmt.Printf("Finding studies (patient %q)...\n", strings.TrimSpace(*patient))
	query := queryDataset(
		queryKey{tag.SpecificCharacterSet, "ISO_IR 100"},
		queryKey{tag.PatientName, strings.TrimSpace(*patient)},
		queryKey{tag.PatientID, ""},
		queryKey{tag.ModalitiesInStudy, ""},
		queryKey{tag.StudyDate, ""},
		queryKey{tag.StudyInstanceUID, strings.TrimSpace(*studyUID)},
		queryKey{tag.StudyDescription, ""},
	)

	var studies []string
	err := c.CFindWithCallback(ctx, dimse.QueryRetrieveLevelStudy, query, func(result *dataset.Dataset) bool {
		found := result.TryGetString(tag.StudyInstanceUID)
		fmt.Printf("Patient %s, %s-Study from %s with UID %s\n",
			valueOrDash(result.TryGetString(tag.PatientName)),
			valueOrDash(result.TryGetString(tag.ModalitiesInStudy)),
			valueOrDash(result.TryGetString(tag.StudyDate)),
			found)
		if found != "" {
			studies = append(studies, found)
		}
		return true
	})
	return studies, err
}

func findSeries(ctx context.Context, c *client.Client, study string) ([]string, error) {
	fmt.Printf("Finding series of study %s...\n", study)
	query := queryDataset(
		queryKey{tag.SpecificCharacterSet, "ISO_IR 100"},
		queryKey{tag.SeriesInstanceUID, ""},
		queryKey{tag.SeriesDescription, ""},
		queryKey{tag.Modality, ""},
		queryKey{tag.NumberOfSeriesRelatedInstances, ""},
		queryKey{tag.StudyInstanceUID, study},
	)

	var series []string
	err := c.CFindWithCallback(ctx, dimse.QueryRetrieveLevelSeries, query, func(result *dataset.Dataset) bool {
		found := result.TryGetString(tag.SeriesInstanceUID)
		fmt.Printf("Serie %s, %s, %s instances\n",
			valueOrDash(result.TryGetString(tag.SeriesDescription)),
			valueOrDash(result.TryGetString(tag.Modality)),
			valueOrDash(result.TryGetString(tag.NumberOfSeriesRelatedInstances)))
		if found != "" {
			series = append(series, found)
		}
		return true
	})
	return series, err
}

func retrieveSeries(ctx context.Context, c *client.Client, study, series string) error {
	fmt.Printf("Retrieving series %s with C-GET...\n", series)
	identifier := dataset.New()
	if err := identifier.AddValue(tag.StudyInstanceUID, study); err != nil {
		return err
	}
	if err := identifier.AddValue(tag.SeriesInstanceUID, series); err != nil {
		return err
	}
	return c.CGet(ctx, dimse.QueryRetrieveLevelSeries, identifier,
		func(remaining, completed, failed, _ uint16) bool {
			fmt.Printf("  remaining=%d completed=%d failed=%d\n", remaining, completed, failed)
			return true
		})
}

func moveStudy(ctx context.Context, c *client.Client, study, destination string) error {
	fmt.Printf("Moving study %s to %s with C-MOVE...\n", study, destination)
	identifier := dataset.New()
	if err := identifier.AddValue(tag.StudyInstanceUID, study); err != nil {
		return err
	}
	return c.CMove(ctx, dimse.QueryRetrieveLevelStudy, destination, identifier,
		func(remaining, _, _, _ uint16) bool {
			fmt.Printf("Sending is in progress. please wait: %d remaining\n", remaining)
			return true
		})
}

// handleCStore receives one C-GET sub-operation, saves it below -data-dir and
// acknowledges it, like SaveImage in the fo-dicom sample.
func handleCStore(_ context.Context, req *dimse.CStoreRequest) (*dimse.CStoreResponse, error) {
	ds := req.DataDataset()
	if ds == nil {
		return dimse.NewCStoreResponseFromRequest(req, status.CStoreErrorCannotUnderstand), nil
	}
	path, err := store.Save(*dataDir, ds)
	if err != nil {
		fmt.Printf("  cannot save the received instance: %v\n", err)
		return dimse.NewCStoreResponseFromRequest(req, status.CStoreErrorCannotUnderstand), nil
	}
	savedFiles.Add(1)
	fmt.Printf("  received %s\n", path)
	return dimse.NewCStoreResponseFromRequest(req, status.Success), nil
}

func valueOrDash(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	return value
}
