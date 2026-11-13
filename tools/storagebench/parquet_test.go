package storagebench

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/compress/zstd"
)

func writeParquet(t *testing.T, f fixture, dir string) func() {
	t.Helper()
	group := parquet.Group{"a_timestamp": parquet.Encoded(parquet.Leaf(parquet.Int64Type), &parquet.DeltaBinaryPacked)}
	for j := range f.Series {
		group[fmt.Sprintf("v%03d", j)] = parquet.Encoded(parquet.Leaf(parquet.DoubleType), &parquet.ByteStreamSplit)
	}
	schema := parquet.NewSchema("monitoring", group)
	codec := &zstd.Codec{Level: zstd.SpeedDefault, Concurrency: 1}
	var writer *parquet.Writer
	var file *os.File
	var files []string
	row := make(parquet.Row, len(f.Series)+1)
	closeWriter := func() {
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	readSamples(t, f, func(i int, s sample) {
		if i%901 == 0 {
			if writer != nil {
				closeWriter()
			}
			path := filepath.Join(dir, partitionName(i/901, ".parquet"))
			var err error
			file, err = os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			writer = parquet.NewWriter(file, schema, parquet.Compression(codec), parquet.MaxRowsPerRowGroup(901))
			files = append(files, path)
		}
		row[0] = parquet.Int64Value(s.at).Level(0, 0, 0)
		for j, v := range s.values {
			row[j+1] = parquet.DoubleValue(v).Level(0, 0, j+1)
		}
		if _, err := writer.WriteRows([]parquet.Row{row}); err != nil {
			t.Fatal(err)
		}
	})
	closeWriter()
	return func() {
		digest := sha256.New()
		record := make([]byte, 8+8*len(f.Series))
		count := 0
		for _, path := range files {
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			r := parquet.NewReader(file)
			rows := make([]parquet.Row, 128)
			for {
				n, err := r.ReadRows(rows)
				for _, row := range rows[:n] {
					for _, v := range row {
						if v.Column() == 0 {
							binary.LittleEndian.PutUint64(record, uint64(v.Int64()))
						} else {
							binary.LittleEndian.PutUint64(record[v.Column()*8:], math.Float64bits(v.Double()))
						}
					}
					digest.Write(record)
					count++
				}
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		}
		checkHash(t, digest, f.SHA256, count, f.Samples)
	}
}
