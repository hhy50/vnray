package storagebench

import (
	"crypto/sha256"
	"encoding/binary"
	"testing"

	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/badger/v4/options"
)

func writeBadger(t *testing.T, f fixture, dir string) func() {
	t.Helper()
	opts := badger.DefaultOptions(dir).WithLogger(nil).WithCompression(options.ZSTD).WithZSTDCompressionLevel(3).WithValueThreshold(1 << 20).WithValueLogFileSize(32 << 20).WithCompactL0OnClose(true)
	db, err := badger.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	batch := db.NewWriteBatch()
	defer func() { batch.Cancel() }()
	readSamples(t, f, func(i int, s sample) {
		key := make([]byte, 8)
		binary.BigEndian.PutUint64(key, uint64(s.at))
		if err := batch.Set(key, append([]byte(nil), s.raw[8:]...)); err != nil {
			t.Fatal(err)
		}
		if i%901 == 900 {
			if err := batch.Flush(); err != nil {
				t.Fatal(err)
			}
			batch = db.NewWriteBatch()
		}
	})
	if err := batch.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := db.Flatten(1); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return func() {
		// Badger 在 Windows 不支持只读打开，使用普通打开完成回读。
		db, err := badger.Open(opts)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.New()
		count := 0
		err = db.View(func(txn *badger.Txn) error {
			it := txn.NewIterator(badger.DefaultIteratorOptions)
			defer it.Close()
			var timestamp [8]byte
			for it.Rewind(); it.Valid(); it.Next() {
				item := it.Item()
				binary.LittleEndian.PutUint64(timestamp[:], binary.BigEndian.Uint64(item.Key()))
				digest.Write(timestamp[:])
				if err := item.Value(func(v []byte) error { _, err := digest.Write(v); return err }); err != nil {
					return err
				}
				count++
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		checkHash(t, digest, f.SHA256, count, f.Samples)
	}
}
