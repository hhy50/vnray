package main

import "os"

func main() {
	if err := RunClient(); err != nil {
		os.Exit(1)
	}
}
