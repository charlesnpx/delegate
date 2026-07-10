package main

import (
	"fmt"
	"io"
	"os"
)

// Version is the development version reported by the initial scaffold.
const Version = "v0.0.0-dev"

func main() {
	writeVersion(os.Stdout)
}

func writeVersion(w io.Writer) {
	fmt.Fprintln(w, versionLine())
}

func versionLine() string {
	return "delegate " + Version
}
