// Copyright (c) 2025 go-dicom-samples contributors.
// Licensed under the MIT License.

// Command print-scu is the DICOM Print Management SCU from the fo-dicom
// "Print SCU" sample. It loads one or more 8-bit or 12-bit grayscale DICOM
// images, builds a Basic Film Session with one Film Box per image (or per group
// of Image Boxes), and runs the DIMSE-N print workflow through
// pkg/printing. Images are copied, never rendered or rescaled.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/cocosip/go-dicom/pkg/dicom/dataset"
	"github.com/cocosip/go-dicom/pkg/dicom/element"
	"github.com/cocosip/go-dicom/pkg/dicom/tag"
	"github.com/cocosip/go-dicom/pkg/dicom/uid"
	"github.com/cocosip/go-dicom/pkg/network/client"
	"github.com/cocosip/go-dicom/pkg/network/dimse"
	"github.com/cocosip/go-dicom/pkg/printing"

	_ "github.com/cocosip/go-dicom-samples/internal/codecs"
	"github.com/cocosip/go-dicom-samples/internal/demo"
	"github.com/cocosip/go-dicom-samples/internal/store"
)

// sourceImage is one printable grayscale image copied from a DICOM file.
type sourceImage struct {
	path          string
	rows, columns uint16
	photometric   string
	bitsAllocated uint16
	bitsStored    uint16
	highBit       uint16
	pixels        []byte
}

// dimseAdapter exposes *client.Client as printing.DIMSEService.
type dimseAdapter struct {
	client *client.Client
}

func (a dimseAdapter) SendNCreate(ctx context.Context, request *dimse.NCreateRequest) (*dimse.NCreateResponse, error) {
	response, err := a.client.NCreate(ctx, request)
	logResponse("N-CREATE", request.AffectedSOPInstanceUID(), response, err)
	return response, err
}

func (a dimseAdapter) SendNSet(ctx context.Context, request *dimse.NSetRequest) (*dimse.NSetResponse, error) {
	response, err := a.client.NSet(ctx, request)
	logResponse("N-SET", request.RequestedSOPInstanceUID(), response, err)
	return response, err
}

func (a dimseAdapter) SendNAction(ctx context.Context, request *dimse.NActionRequest) (*dimse.NActionResponse, error) {
	response, err := a.client.NAction(ctx, request)
	logResponse("N-ACTION", request.RequestedSOPInstanceUID(), response, err)
	return response, err
}

func (a dimseAdapter) SendNDelete(ctx context.Context, request *dimse.NDeleteRequest) (*dimse.NDeleteResponse, error) {
	response, err := a.client.NDelete(ctx, request)
	logResponse("N-DELETE", request.RequestedSOPInstanceUID(), response, err)
	return response, err
}

func main() {
	host := flag.String("host", "localhost", "print SCP host")
	port := flag.Int("port", 11115, "print SCP port")
	calledAE := flag.String("called-ae", "PRINTSCP", "called AE title of the print SCP")
	callingAE := flag.String("calling-ae", "PRINTSCU", "calling AE title of this print SCU")
	var files stringList
	flag.Var(&files, "file", "DICOM image file to print (repeatable; defaults to a generated demo image)")
	copies := flag.Int("copies", 1, "number of copies of each film")
	filmSize := flag.String("film-size", "A4", "film size ID, for example A4 or 14INX17IN")
	displayFormat := flag.String("image-display-format", `STANDARD\1,1`, "image display format, for example STANDARD\\2,2")
	flag.Parse()

	started := time.Now()

	images, cleanup, err := loadImages(files)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		log.Fatalf("Load images: %v", err)
	}
	if len(images) == 0 {
		log.Fatal("No printable grayscale image found")
	}

	boxesPerFilm, err := printing.ParseImageDisplayFormat(*displayFormat)
	if err != nil {
		log.Fatalf("Image display format %q: %v", *displayFormat, err)
	}
	if *copies < 1 {
		log.Fatalf("Copies must be at least 1, got %d", *copies)
	}

	session := printing.NewFilmSession(uid.BasicFilmSession.UID(), "", false)
	session.FilmSessionLabel = "DICOM PRINT JOB"
	session.MediumType = printing.MediumTypePaper
	session.NumberOfCopies = *copies
	fmt.Printf("Film session %s: label=%q copies=%d medium=%s\n",
		session.SOPInstanceUID, session.FilmSessionLabel, session.NumberOfCopies, session.MediumType)

	// One Film Box per group of Image Boxes; the last group reuses images when
	// the count is not a multiple of the Image Display Format.
	filmBoxCount := (len(images) + boxesPerFilm - 1) / boxesPerFilm
	for index := 0; index < filmBoxCount; index++ {
		filmBox := printing.NewFilmBox("", *displayFormat)
		filmBox.FilmOrientation = printing.FilmOrientationPortrait
		filmBox.FilmSizeID = printing.FilmSize(*filmSize)
		filmBox.MagnificationType = printing.MagnificationNone
		filmBox.BorderDensity = printing.BorderDensityBlack
		filmBox.EmptyImageDensity = printing.EmptyImageDensityBlack
		session.AddFilmBox(filmBox)
		if err := filmBox.InitializeImageBoxes(); err != nil {
			log.Fatalf("Initialize film box %s: %v", filmBox.SOPInstanceUID, err)
		}
		for position, imageBox := range filmBox.BasicImageBoxes {
			image := images[(index*boxesPerFilm+position)%len(images)]
			attributes, err := image.imageDataset()
			if err != nil {
				log.Fatalf("Build image box data from %s: %v", image.path, err)
			}
			if err := imageBox.SetImageSequence(attributes); err != nil {
				log.Fatalf("Set image box %s data: %v", imageBox.SOPInstanceUID, err)
			}
		}
		fmt.Printf("Film box %s: format=%q orientation=%s size=%s image boxes=%d\n",
			filmBox.SOPInstanceUID, filmBox.ImageDisplayFormat, filmBox.FilmOrientation,
			filmBox.FilmSizeID, filmBox.ImageBoxCount())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	endpoint := fmt.Sprintf("%s:%d", *host, *port)
	fmt.Printf("Starting print SCU: calling AE=%s, called AE=%s at %s\n", *callingAE, *calledAE, endpoint)

	printClient := client.New(client.WithCallingAE(*callingAE), client.WithCalledAE(*calledAE))
	defer func() {
		if err := printClient.Close(); err != nil {
			log.Printf("Close association: %v", err)
		}
	}()
	for _, sopClass := range []string{
		uid.BasicFilmSession.UID(),
		uid.BasicFilmBox.UID(),
		printing.SOPClassGrayscaleImageBox,
		uid.Printer.UID(),
	} {
		if err := printClient.AddPresentationContext(sopClass,
			uid.ExplicitVRLittleEndian.UID(), uid.ImplicitVRLittleEndian.UID()); err != nil {
			log.Fatalf("Add presentation context %s: %v", sopClass, err)
		}
	}
	if err := printClient.Connect(ctx, *host, *port); err != nil {
		log.Fatalf("Connect to %s: %v", endpoint, err)
	}
	fmt.Println("Association accepted")

	if response, err := printClient.NGet(ctx, dimse.NewNGetRequest(uid.Printer.UID(), uid.PrinterInstance.UID(), nil)); err != nil {
		log.Printf("Printer status N-GET failed: %v", err)
	} else if response.IsSuccess() {
		if attributes := response.DataDataset(); attributes != nil {
			name, _ := attributes.GetString(tag.PrinterName)
			printerStatus, _ := attributes.GetString(tag.PrinterStatus)
			statusInfo, _ := attributes.GetString(tag.PrinterStatusInfo)
			fmt.Printf("Printer %q status: %s (%s)\n", name, printerStatus, statusInfo)
		}
	}

	printCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	fmt.Println("Creating film session, film box(es) and image box(es), then activating the print ...")
	if err := printing.NewClient(dimseAdapter{client: printClient}).Print(printCtx, session); err != nil {
		log.Fatalf("Print job failed: %v", err)
	}
	fmt.Println("Print job completed successfully")

	if response, err := printClient.NDelete(ctx,
		dimse.NewNDeleteRequest(session.SOPClassUID, session.SOPInstanceUID)); err != nil {
		log.Printf("N-DELETE film session failed: %v", err)
	} else {
		fmt.Printf("N-DELETE film session %s: %s\n", session.SOPInstanceUID, response.Status())
	}

	fmt.Printf("Elapsed: %s\n", time.Since(started).Round(time.Millisecond))
}

// loadImages loads every file it can print. Without -file a small demo study is
// generated and its first two images are used, matching the fo-dicom sample.
func loadImages(files []string) ([]*sourceImage, func(), error) {
	cleanup := func() {}
	paths := files
	if len(paths) == 0 {
		folder, err := os.MkdirTemp("", "print-scu-*")
		if err != nil {
			return nil, nil, err
		}
		cleanup = func() { _ = os.RemoveAll(folder) }
		generated, err := demo.Generate(folder)
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		if len(generated) > 2 {
			generated = generated[:2]
		}
		paths = generated
		fmt.Printf("No -file given; generated %d demo image(s) in %s\n", len(paths), folder)
	}

	images := make([]*sourceImage, 0, len(paths))
	for _, path := range paths {
		image, err := loadSourceImage(path)
		if err != nil {
			log.Printf("Skipping %s: %v", path, err)
			continue
		}
		fmt.Printf("Loaded %s: %dx%d, %d/%d bits, %s\n",
			image.path, image.columns, image.rows, image.bitsStored, image.bitsAllocated, image.photometric)
		images = append(images, image)
	}
	return images, cleanup, nil
}

func loadSourceImage(path string) (*sourceImage, error) {
	attributes, err := store.Load(path)
	if err != nil {
		return nil, err
	}
	if syntax := attributes.InternalTransferSyntax(); syntax != nil && syntax.IsEncapsulated() {
		return nil, errors.New("compressed transfer syntax is not supported; the sample prints raw pixel data")
	}

	samplesPerPixel, err := attributes.GetUInt16(tag.SamplesPerPixel, 0)
	if err != nil {
		return nil, fmt.Errorf("samples per pixel: %w", err)
	}
	if samplesPerPixel != 1 {
		return nil, fmt.Errorf("samples per pixel is %d; only grayscale images are supported", samplesPerPixel)
	}
	photometric, ok := attributes.GetString(tag.PhotometricInterpretation)
	if !ok {
		return nil, errors.New("missing Photometric Interpretation")
	}
	photometric = strings.TrimSpace(photometric)
	if photometric != "MONOCHROME1" && photometric != "MONOCHROME2" {
		return nil, fmt.Errorf("photometric interpretation %q is not grayscale", photometric)
	}

	rows, err := attributes.GetUInt16(tag.Rows, 0)
	if err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	columns, err := attributes.GetUInt16(tag.Columns, 0)
	if err != nil {
		return nil, fmt.Errorf("columns: %w", err)
	}
	bitsAllocated, err := attributes.GetUInt16(tag.BitsAllocated, 0)
	if err != nil {
		return nil, fmt.Errorf("bits allocated: %w", err)
	}
	bitsStored, err := attributes.GetUInt16(tag.BitsStored, 0)
	if err != nil {
		return nil, fmt.Errorf("bits stored: %w", err)
	}
	highBit, err := attributes.GetUInt16(tag.HighBit, 0)
	if err != nil {
		return nil, fmt.Errorf("high bit: %w", err)
	}
	pixelRepresentation, err := attributes.GetUInt16(tag.PixelRepresentation, 0)
	if err != nil {
		return nil, fmt.Errorf("pixel representation: %w", err)
	}
	if pixelRepresentation != 0 {
		return nil, errors.New("signed pixel data is not supported; only unsigned grayscale is printed")
	}
	grayscale8 := bitsStored == 8 && bitsAllocated == 8 && highBit == 7
	grayscale12 := bitsStored == 12 && bitsAllocated == 16 && highBit == 11
	if !grayscale8 && !grayscale12 {
		return nil, fmt.Errorf("unsupported encoding bits allocated/stored/high bit %d/%d/%d; want 8/8/7 or 16/12/11",
			bitsAllocated, bitsStored, highBit)
	}

	pixelData := attributes.GetOrNil(tag.PixelData)
	if pixelData == nil || pixelData.Buffer() == nil {
		return nil, errors.New("missing pixel data")
	}
	pixels := append([]byte(nil), pixelData.Buffer().Data()...)
	frameSize := int(rows) * int(columns) * int(bitsAllocated/8)
	if len(pixels) < frameSize {
		return nil, fmt.Errorf("pixel data has %d bytes, want at least %d", len(pixels), frameSize)
	}
	// Multi-frame instances are printed as their first frame.
	return &sourceImage{
		path:          path,
		rows:          rows,
		columns:       columns,
		photometric:   photometric,
		bitsAllocated: bitsAllocated,
		bitsStored:    bitsStored,
		highBit:       highBit,
		pixels:        pixels[:frameSize],
	}, nil
}

// imageDataset builds one item of the Basic Grayscale Image Sequence.
func (image *sourceImage) imageDataset() (*dataset.Dataset, error) {
	attributes := dataset.New()
	values := []struct {
		t     *tag.Tag
		value any
	}{
		{tag.SamplesPerPixel, uint16(1)},
		{tag.PhotometricInterpretation, image.photometric},
		{tag.Rows, image.rows},
		{tag.Columns, image.columns},
		{tag.BitsAllocated, image.bitsAllocated},
		{tag.BitsStored, image.bitsStored},
		{tag.HighBit, image.highBit},
		{tag.PixelRepresentation, uint16(0)},
	}
	for _, item := range values {
		if err := attributes.AddValue(item.t, item.value); err != nil {
			return nil, fmt.Errorf("add %s: %w", item.t, err)
		}
	}

	var pixelData element.Element = element.NewOtherByte(tag.PixelData, append([]byte(nil), image.pixels...))
	if image.bitsAllocated > 8 {
		pixelData = element.NewOtherWord(tag.PixelData, append([]byte(nil), image.pixels...))
	}
	if err := attributes.Add(pixelData); err != nil {
		return nil, fmt.Errorf("add Pixel Data: %w", err)
	}
	return attributes, nil
}

func logResponse(operation, sopInstanceUID string, response dimse.Response, err error) {
	if err != nil {
		log.Printf("%s %s: %v", operation, sopInstanceUID, err)
		return
	}
	log.Printf("%s %s -> %s", operation, sopInstanceUID, response.Status())
}

// stringList is a repeatable string flag.
type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ", ") }

func (values *stringList) Set(value string) error {
	*values = append(*values, value)
	return nil
}
