package ratelimit

import (
	"context"
	"testing"
)

func TestBucketUncontendedAllocations(t *testing.T) {
	bucket := NewBucket(1 << 20)
	allocs := testing.AllocsPerRun(100, func() {
		bucket.SetRate(1 << 19)
		if err := bucket.Wait(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
		bucket.SetRate(1 << 20)
	})
	if allocs != 0 {
		t.Fatalf("uncontended rate update/wait allocated %g objects", allocs)
	}
}

func BenchmarkBucketRateUpdates(b *testing.B) {
	bucket := NewBucket(1 << 20)
	b.ReportAllocs()
	for b.Loop() {
		bucket.SetRate(1 << 19)
		bucket.SetRate(1 << 20)
	}
}
