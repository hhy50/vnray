package pipe

import "testing"

func BenchmarkPipeTransfer32KiB(b *testing.B) {
	r, w := New()
	defer r.Interrupt()
	input := make([]byte, 32*1024)
	output := make([]byte, len(input))
	if _, err := w.Write(input); err != nil {
		b.Fatal(err)
	}
	if _, err := r.Read(output); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(input)))
	for b.Loop() {
		if n, err := w.Write(input); n != len(input) || err != nil {
			b.Fatalf("write = %d, %v", n, err)
		}
		if n, err := r.Read(output); n != len(output) || err != nil {
			b.Fatalf("read = %d, %v", n, err)
		}
	}
}

func BenchmarkPipeFirstWrite(b *testing.B) {
	input := []byte{1}
	b.ReportAllocs()
	for b.Loop() {
		r, w := New()
		if _, err := w.Write(input); err != nil {
			b.Fatal(err)
		}
		r.Interrupt()
	}
}
