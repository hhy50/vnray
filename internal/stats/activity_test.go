package stats

import "testing"

func BenchmarkUserActivity(b *testing.B) {
	s := &PumpStats{}
	b.ReportAllocs()
	for b.Loop() {
		activity(s)
	}
}

func BenchmarkUserActivityDisabled(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		activity(nil)
	}
}

func activity(s *PumpStats) {
	defer s.OpenTCP().CloseTCP()
	defer s.OpenStream().CloseStream()
	s.Request()
}
