package metrics

import (
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func readSystem() systemReading {
	var r systemReading
	if b, err := os.ReadFile("/proc/stat"); err == nil {
		r.cpuTotal, r.cpuIdle, r.cpuValid = parseCPU(string(b))
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		r.memoryTotal, r.memoryUsed = parseMemory(string(b))
	}
	if b, err := os.ReadFile("/proc/net/dev"); err == nil {
		r.network = parseNetwork(string(b))
	}
	if b, err := os.ReadFile("/proc/self/statm"); err == nil {
		fields := strings.Fields(string(b))
		if len(fields) >= 2 {
			if pages, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				r.resident = pointer(pages * uint64(os.Getpagesize()))
			}
		}
	}
	var usage unix.Rusage
	if unix.Getrusage(unix.RUSAGE_SELF, &usage) == nil {
		r.processCPU = pointer(float64(usage.Utime.Sec) + float64(usage.Stime.Sec) + (float64(usage.Utime.Usec)+float64(usage.Stime.Usec))/1e6)
	}
	return r
}
