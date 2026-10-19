package main

import (
	"os"

	app "github.com/hhy50/vnray/internal/core"
)

func main() {
	if err := app.RunServer(); err != nil {
		os.Exit(1)
	}
}
