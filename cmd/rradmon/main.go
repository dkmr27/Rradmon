// Command rradmon is a Reticulum LoRa radio monitor.
package main

import (
	"flag"
	"fmt"
	"os"
)

var version = "0.0.0-dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("rradmon", version)
		return
	}

	fmt.Fprintln(os.Stderr, "rradmon: nothing to run yet (Phase 0)")
	os.Exit(1)
}
