// Copyright (c) 2025 go-dicom-samples contributors.
// Licensed under the MIT License.

// Package main demonstrates the go-dicom-codecs integration. It lists the
// transfer syntaxes that have a registered codec, decodes a first frame, and
// transcodes a DICOM file between transfer syntaxes, re-reading the output to
// prove the round trip. When no input is given, a demo instance is generated.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	_ "github.com/cocosip/go-dicom-samples/internal/codecs"
	"github.com/cocosip/go-dicom-samples/internal/demo"

	"github.com/cocosip/go-dicom/pkg/dicom/dataset"
	"github.com/cocosip/go-dicom/pkg/dicom/parser"
	"github.com/cocosip/go-dicom/pkg/dicom/transcode"
	"github.com/cocosip/go-dicom/pkg/dicom/transfer"
	"github.com/cocosip/go-dicom/pkg/dicom/writer"
	"github.com/cocosip/go-dicom/pkg/imaging/codec"
	"github.com/cocosip/go-dicom/pkg/imaging/pixeldata"
)

var (
	inputPath    = flag.String("in", "", "source DICOM file (a demo instance is generated when omitted)")
	outputPath   = flag.String("out", "transcoded.dcm", "output DICOM file")
	targetSyntax = flag.String("to", "ExplicitVRLittleEndian", "target transfer syntax name or UID")
	listCodecs   = flag.Bool("list", false, "list transfer syntaxes with a registered codec and exit")
	decodeOnly   = flag.Bool("decode", false, "decode the first frame and report its size")
)

func main() {
	flag.Parse()

	if *listCodecs {
		printRegisteredCodecs()
		return
	}

	path, cleanup, err := resolveInput()
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		fail(err)
	}

	ctx := context.Background()
	if *decodeOnly {
		if err := decodeFirstFrame(ctx, path); err != nil {
			fail(err)
		}
		return
	}
	if err := transcodeFile(ctx, path, *outputPath, *targetSyntax); err != nil {
		fail(err)
	}
}

func fail(err error) {
	_, _ = fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

// resolveInput returns the file to work on. Without -in it generates a demo
// study in a temp directory and returns its first instance plus a cleanup.
func resolveInput() (string, func(), error) {
	if *inputPath != "" {
		return *inputPath, nil, nil
	}

	root, err := os.MkdirTemp("", "dicom-transcode-*")
	if err != nil {
		return "", nil, err
	}
	files, err := demo.Generate(root)
	if err != nil {
		_ = os.RemoveAll(root)
		return "", nil, fmt.Errorf("generate demo data: %w", err)
	}
	fmt.Printf("No input given, generated %d demo instances below %s\n", len(files), root)
	return files[0], func() { _ = os.RemoveAll(root) }, nil
}

// printRegisteredCodecs lists the codecs currently registered with the global
// registry, which go-dicom-codecs init functions populate.
func printRegisteredCodecs() {
	registered := codec.GlobalRegistry().List()
	fmt.Printf("Transfer syntaxes with a registered codec (%d):\n", len(registered))
	for _, c := range registered {
		syntax := c.TransferSyntax().UID()
		fmt.Printf("  %-27s %-45s %s\n", syntax.UID(), syntax.Name(), c.Name())
	}
}

func decodeFirstFrame(ctx context.Context, path string) error {
	result, err := parser.ParseFile(path)
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}

	size, err := decodedFrameSize(ctx, result.Dataset, result.TransferSyntax)
	if err != nil {
		return fmt.Errorf("decode first frame: %w", err)
	}
	fmt.Printf("File: %s\n", path)
	fmt.Printf("Transfer syntax: %s (%s)\n", result.TransferSyntax.UID().UID(), result.TransferSyntax.UID().Name())
	fmt.Printf("Decoded first frame: %d bytes\n", size)
	return nil
}

// transcodeFile transcodes in to out, then re-parses out so the reported
// syntax, element count, and decoded frame size come from the written file.
func transcodeFile(ctx context.Context, in, out, targetName string) error {
	result, err := parser.ParseFile(in)
	if err != nil {
		return fmt.Errorf("parse %s: %w", in, err)
	}
	target, err := resolveTransferSyntax(targetName)
	if err != nil {
		return err
	}

	manager, err := transcode.NewManager(codec.GlobalRegistry())
	if err != nil {
		return fmt.Errorf("create transcode manager: %w", err)
	}
	if !manager.CanTranscode(result.TransferSyntax, target) {
		return missingCodecError(result.TransferSyntax, target)
	}

	transcoder, err := manager.NewTranscoder(result.TransferSyntax, target)
	if err != nil {
		return fmt.Errorf("create transcoder %s -> %s: %w", result.TransferSyntax.UID().UID(), target.UID().UID(), err)
	}
	transcoded, fileMeta, err := transcoder.TranscodeWithMetadata(ctx, result.Dataset, result.FileMetaInformation)
	if err != nil {
		return fmt.Errorf("transcode %s -> %s: %w", result.TransferSyntax.UID().UID(), target.UID().UID(), err)
	}
	if err := writer.WriteFile(out, transcoded, writer.WithFileMetaInfo(fileMeta.Dataset())); err != nil {
		return fmt.Errorf("write %s: %w", out, err)
	}

	verified, err := parser.ParseFile(out)
	if err != nil {
		return fmt.Errorf("re-parse %s: %w", out, err)
	}

	fmt.Printf("Source transfer syntax: %s (%s)\n", result.TransferSyntax.UID().UID(), result.TransferSyntax.UID().Name())
	fmt.Printf("Target transfer syntax: %s (%s)\n", target.UID().UID(), target.UID().Name())
	fmt.Printf("Wrote %s\n", out)
	fmt.Printf("Verified: %s, %d elements (source had %d)\n",
		verified.TransferSyntax.UID().UID(), verified.Dataset.Count(), result.Dataset.Count())

	if _, err := pixeldata.FromDataset(verified.Dataset); err != nil {
		fmt.Println("Decoded first frame: no pixel data")
		return nil
	}
	size, err := decodedFrameSize(ctx, verified.Dataset, verified.TransferSyntax)
	if err != nil {
		return fmt.Errorf("decode first frame of %s: %w", out, err)
	}
	fmt.Printf("Decoded first frame: %d bytes\n", size)
	return nil
}

// decodedFrameSize decodes frame 0 of ds and returns its native size.
func decodedFrameSize(ctx context.Context, ds *dataset.Dataset, syntax *transfer.Syntax) (int, error) {
	registered, ok := codec.GlobalRegistry().Lookup(syntax)
	if !ok {
		return 0, fmt.Errorf("no codec registered for transfer syntax %s (%s); import %s",
			syntax.UID().UID(), syntax.UID().Name(), codecForTransferSyntax(syntax))
	}
	pixels, err := pixeldata.FromDataset(ds)
	if err != nil {
		return 0, err
	}
	decoded, err := pixels.Decode(ctx, registered, nil)
	if err != nil {
		return 0, err
	}
	frame, err := decoded.Frame(ctx, 0)
	if err != nil {
		return 0, err
	}
	return len(frame), nil
}
