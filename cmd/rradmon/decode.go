package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/dkmr27/Rradmon/internal/decode"
	"github.com/dkmr27/Rradmon/internal/reassembly"
	"github.com/dkmr27/Rradmon/internal/reticulum"
)

func runDecode(args []string) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: rradmon decode <capture.jsonl>")
	}
	f, err := os.Open(args[0])
	if err != nil {
		return err
	}
	defer f.Close()
	items, err := decode.File(f)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for _, it := range items {
		fmt.Println(decode.Describe(it, items))
		switch {
		case it.R.Part != reassembly.Whole:
			counts["orphans"]++
		case it.Packet == nil:
			counts["unparsed"]++
		default:
			counts[it.Packet.Type.String()]++
			if it.R.Frames == 2 {
				counts["split packets"]++
			}
			if it.ProofFor >= 0 {
				counts["proofs matched"]++
			}
			if it.Announce != nil && it.Announce.Validity != reticulum.Valid {
				counts["invalid announces"]++
			}
		}
	}
	fmt.Fprintf(os.Stderr, "\n%d items: %v\n", len(items), counts)
	return nil
}
