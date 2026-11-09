// Copyright (c) 2025 go-dicom-samples contributors.
// Licensed under the MIT License.

package main

import (
	"fmt"
	"strings"

	"github.com/cocosip/go-dicom/pkg/dicom/transfer"
	"github.com/cocosip/go-dicom/pkg/imaging/codec"
)

// transferSyntaxAliases maps the short names used by the transfer package
// constants to their syntaxes. The standard names are matched generically by
// resolveTransferSyntax; these cover the codec targets whose official DICOM
// names are too long to type.
var transferSyntaxAliases = map[string]*transfer.Syntax{
	"jpegbaseline8bit":   transfer.JPEGBaseline8Bit,
	"jpegextended12bit":  transfer.JPEGExtended12Bit,
	"jpeglossless":       transfer.JPEGLossless,
	"jpeglosslesssv1":    transfer.JPEGLosslessSV1,
	"jpeglslossless":     transfer.JPEGLSLossless,
	"jpeglsnearlossless": transfer.JPEGLSNearLossless,
	"jpeg2000lossless":   transfer.JPEG2000Lossless,
	"jpeg2000":           transfer.JPEG2000,
	"rlelossless":        transfer.RLELossless,
}

// codecPackages names the go-dicom-codecs import that supplies a codec, so a
// missing codec error tells the user what to import.
var codecPackages = map[string]string{
	"1.2.840.10008.1.2.4.50": "github.com/cocosip/go-dicom-codecs/jpeg/baseline",
	"1.2.840.10008.1.2.4.51": "github.com/cocosip/go-dicom-codecs/jpeg/extended",
	"1.2.840.10008.1.2.4.57": "github.com/cocosip/go-dicom-codecs/jpeg/lossless",
	"1.2.840.10008.1.2.4.70": "github.com/cocosip/go-dicom-codecs/jpeg/lossless14sv1",
	"1.2.840.10008.1.2.4.80": "github.com/cocosip/go-dicom-codecs/jpegls/lossless",
	"1.2.840.10008.1.2.4.81": "github.com/cocosip/go-dicom-codecs/jpegls/nearlossless",
	"1.2.840.10008.1.2.4.90": "github.com/cocosip/go-dicom-codecs/jpeg2000/lossless",
	"1.2.840.10008.1.2.4.91": "github.com/cocosip/go-dicom-codecs/jpeg2000/lossy",
	"1.2.840.10008.1.2.5":    "github.com/cocosip/go-dicom-codecs/rle",
}

// resolveTransferSyntax accepts a UID or an official name, ignoring case and
// punctuation, plus the short aliases above.
func resolveTransferSyntax(value string) (*transfer.Syntax, error) {
	wanted := compactName(value)
	for _, syntax := range transfer.NewRegistry().List() {
		if strings.EqualFold(syntax.UID().UID(), strings.TrimSpace(value)) || compactName(syntax.String()) == wanted {
			return syntax, nil
		}
	}
	if syntax, ok := transferSyntaxAliases[wanted]; ok {
		return syntax, nil
	}
	return nil, fmt.Errorf("unknown transfer syntax %q: use a UID such as 1.2.840.10008.1.2.5 or a name such as \"RLE Lossless\"", value)
}

// missingCodecError reports which side of the conversion lacks a codec and
// which go-dicom-codecs package would supply it.
func missingCodecError(source, target *transfer.Syntax) error {
	for _, syntax := range []*transfer.Syntax{source, target} {
		if !syntax.IsEncapsulated() {
			continue
		}
		if _, ok := codec.GlobalRegistry().Lookup(syntax); !ok {
			return fmt.Errorf("no codec registered for transfer syntax %s (%s); import %s",
				syntax.UID().UID(), syntax.UID().Name(), codecForTransferSyntax(syntax))
		}
	}
	return fmt.Errorf("no codec path from %s to %s", source.UID().UID(), target.UID().UID())
}

func codecForTransferSyntax(syntax *transfer.Syntax) string {
	if pkg, ok := codecPackages[syntax.UID().UID()]; ok {
		return pkg
	}
	return "a go-dicom-codecs package for this transfer syntax"
}

// compactName keeps only lower-case letters and digits so "Explicit VR Little
// Endian" and "ExplicitVRLittleEndian" compare equal.
func compactName(value string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(value) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}
