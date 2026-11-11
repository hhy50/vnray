package metrics

import (
	"strconv"
	"strings"
)

func parseCPU(data string) (total, idle uint64, valid bool) {
	line, _, _ := strings.Cut(data, "\n")
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, false
	}
	// guest/guest_nice 已包含在 user/nice 中，不能重复累计。
	for i := 1; i < len(fields) && i <= 8; i++ {
		n, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			return 0, 0, false
		}
		total += n
		if i == 4 || i == 5 {
			idle += n
		}
	}
	return total, idle, true
}

func parseMemory(data string) (total, used *uint64) {
	values := make(map[string]uint64)
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		n, err := strconv.ParseUint(fields[1], 10, 64)
		if err == nil {
			values[strings.TrimSuffix(fields[0], ":")] = n * 1024
		}
	}
	t, ok := values["MemTotal"]
	if !ok || t == 0 {
		return nil, nil
	}
	a, ok := values["MemAvailable"]
	if !ok {
		return pointer(t), nil
	}
	if a > t {
		return pointer(t), nil
	}
	return pointer(t), pointer(t - a)
}

func parseNetwork(data string) map[string]networkCounter {
	network := make(map[string]networkCounter)
	valid := strings.Contains(data, "Inter-|")
	for _, line := range strings.Split(data, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		name = strings.TrimSpace(name)
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 16 {
			continue
		}
		rx, rerr := strconv.ParseUint(fields[0], 10, 64)
		tx, terr := strconv.ParseUint(fields[8], 10, 64)
		if rerr == nil && terr == nil {
			valid = true
			if name != "lo" {
				network[name] = networkCounter{rx, tx}
			}
		}
	}
	if !valid {
		return nil
	}
	return network
}
