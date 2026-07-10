package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/charlesnpx/delegate/internal/handoff"
)

// Version is the development version overridden by release builds.
var Version = "0.0.0-dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return finishCommand(runVersion(nil, stdout, stderr), nil, stderr)
	}
	if args[0] == "version" {
		return finishCommand(runVersion(args[1:], stdout, stderr), nil, stderr)
	}
	switch args[0] {
	case "setup":
		code, err := runSetup(args[1:], stdout, stderr)
		return finishCommand(code, err, stderr)
	case "task":
		code, err := runTask(args[1:], stdin, stdout, stderr)
		return finishCommand(code, err, stderr)
	case "review":
		code, err := runReview(reviewKind, args[1:], stdout, stderr)
		return finishCommand(code, err, stderr)
	case "adversarial-review":
		code, err := runReview(adversarialReviewKind, args[1:], stdout, stderr)
		return finishCommand(code, err, stderr)
	case "status":
		code, err := runStatus(args[1:], stdout, stderr)
		return finishCommand(code, err, stderr)
	case "result":
		code, err := runResult(args[1:], stdout, stderr)
		return finishCommand(code, err, stderr)
	case "cancel":
		code, err := runCancel(args[1:], stdout, stderr)
		return finishCommand(code, err, stderr)
	case "install-skills":
		code, err := runInstallSkills(args[1:], stdout, stderr)
		return finishCommand(code, err, stderr)
	}
	if len(args) >= 2 && args[0] == "handoff" && args[1] == "create" {
		if err := runHandoffCreate(args[2:], stdin, stdout, stderr); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		return 0
	}
	fmt.Fprintf(stderr, "unknown command %q\n", args[0])
	return 2
}

func finishCommand(code int, err error, stderr io.Writer) int {
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	return code
}

func runHandoffCreate(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("delegate handoff create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("delegate handoff create does not accept positional arguments")
	}
	if !*jsonOut {
		return errors.New("delegate handoff create requires --json")
	}
	result, err := handoff.Create(handoff.CreateOptions{Reader: stdin})
	if err != nil {
		return err
	}
	raw, err := handoff.MarshalCreateResult(result)
	if err != nil {
		return err
	}
	_, err = stdout.Write(raw)
	return err
}

func writeVersion(w io.Writer) {
	fmt.Fprintln(w, versionLine())
}

func versionLine() string {
	return "delegate " + Version
}

func runVersion(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("delegate version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "delegate version does not accept positional arguments")
		return 2
	}
	if *jsonOut {
		raw, err := json.Marshal(struct {
			Version string `json:"version"`
		}{Version: Version})
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		_, err = fmt.Fprintln(stdout, string(raw))
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		return 0
	}
	writeVersion(stdout)
	return 0
}
