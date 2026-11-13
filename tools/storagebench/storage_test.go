package storagebench

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

type fixtureSeries struct {
	Metric string `json:"metric"`
	UserID uint64 `json:"user_id,omitempty"`
}

type fixture struct {
	Samples         int             `json:"samples"`
	Users           int             `json:"users"`
	IntervalSeconds int             `json:"interval_seconds"`
	Seed            uint64          `json:"seed"`
	Series          []fixtureSeries `json:"series"`
	SHA256          string          `json:"sha256"`
	path            string
	seriesHashes    []hash.Hash
}

type sample struct {
	at     int64
	values []float64
	raw    []byte
}

type benchReport struct {
	Engine        string           `json:"engine"`
	GoVersion     string           `json:"go_version"`
	Platform      string           `json:"platform"`
	Samples       int              `json:"samples"`
	Series        int              `json:"series"`
	Points        int64            `json:"points"`
	FixtureSHA256 string           `json:"fixture_sha256"`
	WriteSeconds  float64          `json:"write_seconds"`
	VerifySeconds float64          `json:"verify_seconds"`
	Files         int              `json:"files"`
	TotalBytes    int64            `json:"total_bytes"`
	ByExtension   map[string]int64 `json:"bytes_by_extension"`
}

func TestStorageComparison(t *testing.T) {
	root, engine := os.Getenv("VNRAY_STORAGE_BENCH_DIR"), os.Getenv("VNRAY_STORAGE_ENGINE")
	if root == "" || engine == "" {
		t.Skip("set VNRAY_STORAGE_BENCH_DIR and VNRAY_STORAGE_ENGINE")
	}
	f := readFixture(t, filepath.Join(root, "fixture"))
	runName := engine
	if suffix := os.Getenv("VNRAY_STORAGE_RUN_SUFFIX"); suffix != "" {
		runName += "-" + suffix
	}
	dir := filepath.Join(root, runName)
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(f.path, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "schema.json"), manifest, 0644); err != nil {
		t.Fatal(err)
	}
	report := benchReport{Engine: engine, GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH, Samples: f.Samples, Series: len(f.Series), Points: int64(f.Samples * len(f.Series)), FixtureSHA256: f.SHA256}
	started := time.Now()
	var verify func()
	switch engine {
	case "parquet-zstd":
		verify = writeParquet(t, f, dir)
	case "badger-zstd":
		verify = writeBadger(t, f, dir)
	case "prometheus":
		verify = writePrometheus(t, f, dir)
	default:
		t.Fatalf("unknown engine %q", engine)
	}
	report.WriteSeconds = time.Since(started).Seconds()
	report.ByExtension = make(map[string]int64)
	err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		report.Files++
		report.TotalBytes += info.Size()
		ext := filepath.Ext(path)
		if ext == "" {
			ext = entry.Name()
		}
		report.ByExtension[ext] += info.Size()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: wrote %d bytes in %.2fs; verifying every value", engine, report.TotalBytes, report.WriteSeconds)
	started = time.Now()
	verify()
	report.VerifySeconds = time.Since(started).Seconds()
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, runName+"-report.json"), append(data, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s", data)
}

func readFixture(t *testing.T, path string) fixture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(path, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	f.path = path
	for range f.Series {
		f.seriesHashes = append(f.seriesHashes, sha256.New())
	}
	digest := sha256.New()
	var pair [16]byte
	readSamples(t, f, func(_ int, s sample) {
		digest.Write(s.raw)
		copy(pair[:8], s.raw[:8])
		for j := range f.Series {
			copy(pair[8:], s.raw[8+j*8:16+j*8])
			f.seriesHashes[j].Write(pair[:])
		}
	})
	if hex.EncodeToString(digest.Sum(nil)) != f.SHA256 {
		t.Fatal("fixture hash mismatch")
	}
	return f
}

func readSamples(t *testing.T, f fixture, visit func(int, sample)) {
	t.Helper()
	file, err := os.Open(filepath.Join(f.path, "samples.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	r := bufio.NewReaderSize(file, 1<<20)
	s := sample{raw: make([]byte, 8+8*len(f.Series)), values: make([]float64, len(f.Series))}
	for i := range f.Samples {
		if _, err := io.ReadFull(r, s.raw); err != nil {
			t.Fatal(err)
		}
		s.at = int64(binary.LittleEndian.Uint64(s.raw))
		for j := range s.values {
			s.values[j] = math.Float64frombits(binary.LittleEndian.Uint64(s.raw[8+j*8:]))
		}
		visit(i, s)
	}
	if _, err := r.ReadByte(); err != io.EOF {
		t.Fatalf("fixture trailing data: %v", err)
	}
}

func checkHash(t *testing.T, digest hash.Hash, expected string, count, want int) {
	t.Helper()
	if count != want || hex.EncodeToString(digest.Sum(nil)) != expected {
		t.Fatalf("round trip mismatch: count %d/%d hash %x", count, want, digest.Sum(nil))
	}
}

func partitionName(i int, suffix string) string { return fmt.Sprintf("%06d%s", i, suffix) }
