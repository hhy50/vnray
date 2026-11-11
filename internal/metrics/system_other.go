//go:build !linux && !darwin

package metrics

func readSystem() systemReading { return systemReading{} }
