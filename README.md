# go-dicom-samples

Go ports of the [fo-dicom samples](https://github.com/fo-dicom/fo-dicom-samples)
built on [go-dicom](https://github.com/cocosip/go-dicom) and
[go-dicom-codecs](https://github.com/cocosip/go-dicom-codecs).

The samples walk through the same DICOM workflows as the C# originals —
C-STORE, Query/Retrieve, Modality Worklist/MPPS, Print Management, DICOMDIR and
compressed transfer syntaxes — as small, runnable console programs.

GUI samples are intentionally omitted (no image rendering, no viewer, no
printer output): every program is headless and console/flag driven instead of
prompting with `Console.ReadLine`.

## Requirements

- Go 1.26 or newer
- No external data sets: programs that need DICOM files generate a small demo
  study when none is given

## Samples

| Go sample | fo-dicom sample | What it does | Default port / AE |
|---|---|---|---|
| [`cstore-scp`](cstore-scp) | C-Store SCP | C-ECHO + C-STORE storage server | 11112 / `STORESCP` |
| [`cstore-scu`](cstore-scu) | C-Store SCU | Sends files with C-STORE | — / `FODICOMSCU` |
| [`qr-scp`](qr-scp) | QueryRetrieve SCP | C-FIND/C-GET/C-MOVE over a folder index | 11113 / `QRSCP` |
| [`qr-scu`](qr-scu) | QueryRetreive SCU | Study/series query, C-GET, C-MOVE | — / `FODICOMSCU` |
| [`worklist-scp`](worklist-scp) | Worklist SCP | Modality Worklist C-FIND + MPPS N-CREATE/N-SET/N-GET | 11114 / `WORKLISTSCP` |
| [`worklist-scu`](worklist-scu) | Worklist SCU | Queries the worklist and completes an MPPS | — / `WORKLISTSCU` |
| [`print-scp`](print-scp) | Print SCP | Basic Grayscale Print Management SCP (DIMSE-N only) | 11115 / `PRINTSCP` |
| [`print-scu`](print-scu) | Print SCU | Builds a film session and prints it | — / `PRINTSCU` |
| [`dicom-media`](dicom-media) | DICOM Media | Writes and reads a DICOMDIR | — |
| [`transcode`](transcode) | *(go-dicom-codecs extra)* | Lists codecs and transcodes between transfer syntaxes | — |

## Quick start

Run each sample from the repository root.

### C-STORE

```bash
# terminal 1: storage SCP, stores below ./DICOM
go run ./cstore-scp

# terminal 2: send the generated demo study
go run ./cstore-scu
# or send real files
go run ./cstore-scu path/to/a.dcm path/to/b.dcm
```

### Query/Retrieve

```bash
# terminal 1: Q/R SCP; an empty data folder is filled with demo data
go run ./qr-scp

# terminal 2: find studies, list their series, C-GET the first series
go run ./qr-scu -data-dir DICOM-received

# C-MOVE needs a destination AE: start a storage SCP and route the AE to it
go run ./cstore-scp -port 11112 -ae STORESCP -storage DICOM-moved
go run ./qr-scp -ae-routes STORESCP=localhost:11112
go run ./qr-scu -move-dest STORESCP
```

`qr-scu` filters with `-patient`, `-study-uid` and `-series-uid`; `-move-dest`
is optional.

### Modality Worklist and MPPS

```bash
# terminal 1: serves MWL + MPPS on one port
go run ./worklist-scp

# terminal 2: query the list, start and complete the first scheduled step
go run ./worklist-scu -patient "Doe^*"
```

### Print Management

```bash
# terminal 1: print SCP, persists received jobs below ./PrintJobs
go run ./print-scp

# terminal 2: print the generated demo images on one film
go run ./print-scu
# or print real grayscale images
go run ./print-scu -file a.dcm -file b.dcm -image-display-format "STANDARD\\2,1"
```

`print-scu` copies raw 8-bit or 12-bit unsigned grayscale pixel data into the
Basic Grayscale Image Sequence; it does not render, rescale, or crop.

### DICOMDIR

```bash
go run ./dicom-media write ./media          # demo data is generated when needed
go run ./dicom-media read ./media
```

### Transfer syntaxes and codecs

```bash
go run ./transcode -list
go run ./transcode -to JPEG2000Lossless -out j2k.dcm
go run ./transcode -in j2k.dcm -to RLELossless -out rle.dcm
go run ./transcode -in rle.dcm -decode
```

`-to` accepts a transfer syntax name (case/punctuation insensitive) or a UID.
The sample reports which `go-dicom-codecs` package to import when a codec is
missing.

## Shared helpers

The sample programs are deliberately small; reusable pieces live in
[`internal/`](internal):

| Package | Purpose |
|---|---|
| [`internal/demo`](internal/demo) | Generates two small demo studies (`<StudyInstanceUID>/<SOPInstanceUID>.dcm`) so samples run without data |
| [`internal/store`](internal/store) | Saves received instances in that layout, loads files and indexes a folder for Q/R |
| [`internal/scp`](internal/scp) | Minimal service-based SCP loop used where N-services are needed (`pkg/network/server` only exposes C-* handlers) |
| [`internal/codecs`](internal/codecs) | Blank-import package that registers the RLE, JPEG, JPEG-LS and JPEG 2000 codecs |

## Differences from the fo-dicom samples

- Flags and positional arguments instead of interactive `Console.ReadLine`
  prompts, so the programs can run unattended and in scripts.
- No GUI, image rendering, or Windows printer output. `print-scp` validates and
  persists the received print hierarchy (film session, film boxes, image boxes)
  instead of producing film; `print-scu` sends raw pixel data instead of a
  rendered bitmap.
- Demo data is generated when an input folder or file is missing, so every
  sample is runnable on a fresh checkout.
- `qr-scp` also answers IMAGE-level queries; `qr-scu` uses one association for
  its query/retrieve calls instead of three.
- Q/R C-MOVE destinations come from `-ae-routes` instead of a hard-coded AE.
- DICOMDIR File IDs must follow PS3.10 (`[A-Z0-9_]`, 1–8 characters per
  component), so UID-named demo files are referenced by sequence number
  (`000001\000001`) rather than by their file name.
- `worklist-scu` opens its association with `pkg/network/service` directly: the
  high-level client builds C-FIND requests for the Patient Root and Study Root
  information models only, and Modality Worklist needs its own SOP Class.

## Upstream

The original C# samples are part of
[fo-dicom-samples](https://github.com/fo-dicom/fo-dicom-samples) and are
licensed under the Microsoft Public License (MS-PL). This port follows their
structure and behavior; the Go code is licensed under the MIT License.
