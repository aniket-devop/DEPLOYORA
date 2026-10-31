package main

import (
	"fmt"
	"os"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println("deployora", version)
		return
	}
	fmt.Fprintln(os.Stderr, "usage: deployora version")
	os.Exit(2)
}
