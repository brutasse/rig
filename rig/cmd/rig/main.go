package main

import (
	"os"

	"github.com/brutasse/rig/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
