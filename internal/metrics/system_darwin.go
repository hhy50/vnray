package metrics

import "golang.org/x/sys/unix"

func readSystem() systemReading {
	var r systemReading
	if total, err := unix.SysctlUint64("hw.memsize"); err == nil {
		r.memoryTotal = &total
	}
	var usage unix.Rusage
	if unix.Getrusage(unix.RUSAGE_SELF, &usage) == nil {
		r.processCPU = pointer(float64(usage.Utime.Sec) + float64(usage.Stime.Sec) + (float64(usage.Utime.Usec)+float64(usage.Stime.Usec))/1e6)
	}
	return r
}
