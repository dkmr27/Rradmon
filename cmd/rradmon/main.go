// Command rradmon is a Reticulum LoRa radio monitor.
package main

import (
	"fmt"
	"os"
)

var version = "0.0.0-dev"

const usage = `usage: rradmon <command> [flags]

commands:
  run       read an RNode and/or MQTT observers and store what they hear
  import    store a capture file in the database
  serve     web UI for an existing database (no radio)
  observe   read an RNode (or replay a capture) and publish to MQTT
  capture   read an RNode in promiscuous mode and save raw frames (JSON lines)
  decode    reassemble and parse a capture file
  ports     list serial ports
  version   print version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "capture":
		err = runCapture(os.Args[2:])
	case "ports":
		err = runPorts()
	case "decode":
		err = runDecode(os.Args[2:])
	case "run":
		err = runRun(os.Args[2:])
	case "import":
		err = runImport(os.Args[2:])
	case "serve":
		err = runServe(os.Args[2:])
	case "observe":
		err = runObserve(os.Args[2:])
	case "version", "-version", "--version":
		fmt.Println("rradmon", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "rradmon: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "rradmon:", err)
		os.Exit(1)
	}
}
