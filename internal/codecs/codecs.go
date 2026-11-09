// Copyright (c) 2025 go-dicom-samples contributors.
// Licensed under the MIT License.

// Package codecs registers the go-dicom-codecs implementations used by the
// samples. A blank import of this package is enough to make the compressed
// transfer syntaxes available to the parser, the writer, and the network
// client/server (the codecs register themselves with the global registry).
package codecs

import (
	// Lossless codecs.
	_ "github.com/cocosip/go-dicom-codecs/jpeg/lossless"
	_ "github.com/cocosip/go-dicom-codecs/jpeg/lossless14sv1"
	_ "github.com/cocosip/go-dicom-codecs/jpeg2000/lossless"
	_ "github.com/cocosip/go-dicom-codecs/jpegls/lossless"
	_ "github.com/cocosip/go-dicom-codecs/rle"

	// Lossy codecs.
	_ "github.com/cocosip/go-dicom-codecs/jpeg/baseline"
	_ "github.com/cocosip/go-dicom-codecs/jpeg/extended"
	_ "github.com/cocosip/go-dicom-codecs/jpeg2000/lossy"
	_ "github.com/cocosip/go-dicom-codecs/jpegls/nearlossless"
)
