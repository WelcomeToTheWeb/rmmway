package export

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/parquet-go/parquet-go"
)

// Verify opens a bundle (the ZIP bytes) and checks it end-to-end against its
// OWN manifest — the self-describing property:
//
//  1. manifest.json is present and names the format + version;
//  2. every manifest file exists in the zip with matching size + sha256
//     (the manifest's own hash is omitted — it cannot carry itself);
//  3. no zip entry is missing from the manifest (no stray files);
//  4. device.json parses, carries the device schema, and its device id
//     matches the manifest;
//  5. alerts.json parses and its alert count matches the manifest rows;
//  6. each Parquet section re-reads with an independent standard Parquet
//     reader at exactly the manifest row count (and the read proves the
//     file is standard-conformant, i.e. openable by any parquet tool).
//
// Verify returns the validated manifest. Any mismatch is an error — a
// bundle that fails Verify is not the data it claims to be.
func Verify(r io.ReaderAt, size int64) (*Manifest, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("not a zip bundle: %w", err)
	}
	entries := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		entries[f.Name] = f
	}

	mf, ok := entries[ManifestName]
	if !ok {
		return nil, fmt.Errorf("bundle has no %s", ManifestName)
	}
	data, err := entryBytes(mf)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", ManifestName, err)
	}
	if m.Format != FormatName {
		return nil, fmt.Errorf("unknown bundle format %q (want %q)", m.Format, FormatName)
	}
	if m.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("unsupported bundle format version %d (want %d)", m.FormatVersion, FormatVersion)
	}
	if len(m.Files) == 0 {
		return nil, fmt.Errorf("manifest lists no files")
	}

	// 2 + 3: every manifest file present with matching size + hash; no
	// stray zip entries.
	manifestNames := make(map[string]bool, len(m.Files))
	for _, f := range m.Files {
		manifestNames[f.Name] = true
		zf, ok := entries[f.Name]
		if !ok {
			return nil, fmt.Errorf("manifest lists %q but the bundle does not contain it", f.Name)
		}
		if uint64(f.Size) != zf.UncompressedSize64 {
			return nil, fmt.Errorf("%s: size %d, manifest says %d", f.Name, zf.UncompressedSize64, f.Size)
		}
		if f.SHA256 == "" {
			if f.Name != ManifestName {
				return nil, fmt.Errorf("%s: manifest entry has no sha256", f.Name)
			}
			continue
		}
		b, err := entryBytes(zf)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != f.SHA256 {
			return nil, fmt.Errorf("%s: sha256 %s, manifest says %s", f.Name, hex.EncodeToString(sum[:]), f.SHA256)
		}
	}
	for name := range entries {
		if !manifestNames[name] {
			return nil, fmt.Errorf("bundle contains %q but the manifest does not list it", name)
		}
	}

	// 4: device.json.
	dz, ok := entries[DeviceName]
	if !ok {
		return nil, fmt.Errorf("bundle has no %s", DeviceName)
	}
	db, err := entryBytes(dz)
	if err != nil {
		return nil, err
	}
	var devFile DeviceFile
	if err := json.Unmarshal(db, &devFile); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", DeviceName, err)
	}
	if devFile.Schema != deviceSchema {
		return nil, fmt.Errorf("%s: schema %q (want %q)", DeviceName, devFile.Schema, deviceSchema)
	}
	if devFile.Device.ID == "" || devFile.Device.ID != m.Device.ID {
		return nil, fmt.Errorf("%s device id %q does not match manifest %q", DeviceName, devFile.Device.ID, m.Device.ID)
	}

	// 5: alerts.json.
	if az, ok := entries[AlertsName]; ok {
		ab, err := entryBytes(az)
		if err != nil {
			return nil, err
		}
		var af AlertsFile
		if err := json.Unmarshal(ab, &af); err != nil {
			return nil, fmt.Errorf("%s is not valid JSON: %w", AlertsName, err)
		}
		if af.Schema != alertsSchema {
			return nil, fmt.Errorf("%s: schema %q (want %q)", AlertsName, af.Schema, alertsSchema)
		}
		if rows := manifestRows(m, AlertsName); rows != int64(len(af.Alerts)) {
			return nil, fmt.Errorf("%s: %d alerts, manifest says %d", AlertsName, len(af.Alerts), rows)
		}
	}

	// 6: the Parquet sections (independent standard reader; a full decode
	// per section proves conformance, not just header validity).
	if rows, ok := manifestRowsOK(m, MetricsName); ok {
		if err := checkParquet(zr, MetricsName, rows, func(f *zip.File) error {
			_, err := readSectionBytes[MetricRow](f)
			return err
		}); err != nil {
			return nil, err
		}
	}
	if rows, ok := manifestRowsOK(m, RollupsName); ok {
		if err := checkParquet(zr, RollupsName, rows, func(f *zip.File) error {
			_, err := readSectionBytes[RollupRow](f)
			return err
		}); err != nil {
			return nil, err
		}
	}
	return &m, nil
}

// manifestRows looks up a file's row count (0 when absent/not a data file).
func manifestRows(m Manifest, name string) int64 {
	r, _ := manifestRowsOK(m, name)
	return r
}

func manifestRowsOK(m Manifest, name string) (int64, bool) {
	for _, f := range m.Files {
		if f.Name == name {
			return f.Rows, true
		}
	}
	return 0, false
}

// checkParquet re-reads one Parquet section with the standard reader and
// asserts the row count. decode fully decodes every row: the conformance
// check — a non-standard/broken file fails here.
func checkParquet(zr *zip.Reader, name string, wantRows int64, decode func(*zip.File) error) error {
	f, err := entryFor(zr, name)
	if err != nil {
		return err
	}
	b, err := entryBytes(f)
	if err != nil {
		return err
	}
	pf, err := parquet.OpenFile(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return fmt.Errorf("%s is not a valid Parquet file: %w", name, err)
	}
	if pf.NumRows() != wantRows {
		return fmt.Errorf("%s: %d rows, manifest says %d", name, pf.NumRows(), wantRows)
	}
	return decode(f)
}

// entryFor finds a zip entry by name.
func entryFor(zr *zip.Reader, name string) (*zip.File, error) {
	for _, f := range zr.File {
		if f.Name == name {
			return f, nil
		}
	}
	return nil, fmt.Errorf("bundle has no %s", name)
}

// entryBytes reads one zip entry fully.
func entryBytes(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", f.Name, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", f.Name, err)
	}
	return b, nil
}

// ReadMetrics reads the metrics.parquet section of a bundle with the
// standard Parquet reader — the "opens in a standard tool" half of the
// no-lock-in promise, callable by a skeptic without trusting RMMWay.
func ReadMetrics(r io.ReaderAt, size int64) ([]MetricRow, error) {
	return readSection[MetricRow](r, size, MetricsName)
}

// ReadRollups reads the metrics_1m.parquet section (nil when the bundle
// omits it).
func ReadRollups(r io.ReaderAt, size int64) ([]RollupRow, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("not a zip bundle: %w", err)
	}
	for _, f := range zr.File {
		if f.Name == RollupsName {
			return readSectionBytes[RollupRow](f)
		}
	}
	return nil, nil
}

func readSection[T any](r io.ReaderAt, size int64, name string) ([]T, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("not a zip bundle: %w", err)
	}
	for _, f := range zr.File {
		if f.Name == name {
			return readSectionBytes[T](f)
		}
	}
	return nil, fmt.Errorf("bundle has no %s", name)
}

func readSectionBytes[T any](f *zip.File) ([]T, error) {
	b, err := entryBytes(f)
	if err != nil {
		return nil, err
	}
	return parquet.Read[T](bytes.NewReader(b), int64(len(b)))
}

// SortedFileNames returns the bundle's file names sorted (tests / UI).
func SortedFileNames(r io.ReaderAt, size int64) ([]string, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		out = append(out, f.Name)
	}
	sort.Strings(out)
	return out, nil
}
