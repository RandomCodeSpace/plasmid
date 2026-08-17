package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/plasmid-dev/plasmid/internal/fixture"
)

func main() {
	root := flag.String("root", ".", "repository root")
	update := flag.Bool("update", false, "regenerate committed fixture artifacts")
	flag.Parse()

	var err error
	if *update {
		err = fixture.UpdatePack(*root)
	} else {
		err = fixture.VerifyPack(*root)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
