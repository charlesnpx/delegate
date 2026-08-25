package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
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
	if args[0] == "version" || args[0] == "--version" || args[0] == "-version" || args[0] == "-V" {
		return finishCommand(runVersion(args[1:], stdout, stderr), nil, stderr)
	}
	switch args[0] {
	case "config":
		code, err := runConfig(args[1:], stdout, stderr)
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
	case "configure-codex-sandbox":
		code, err := runConfigureCodexSandbox(args[1:], stdout, stderr)
		return finishCommand(code, err, stderr)
	}
	if args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		printUsage(stdout)
		return 0
	}
	fmt.Fprintf(stderr, "unknown command %q\n", args[0])
	printUsage(stderr)
	return 2
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `usage: delegate <command> [flags]

commands:
  version         print the delegate version (also: --version, -version, -V)
  config          list/get/set/unset user model and effort defaults
	  task            submit one backend turn and return its receipt
  review          delegate a sanitized code review
  adversarial-review  delegate a refute-first review
  status          check a delegated job
  result          fetch a delegated job result (--wait to block)
  cancel          cancel a delegated job
	  install-skills  plan/install/uninstall the managed skill matrices

run 'delegate <command> -h' for command flags.
`)
}

func finishCommand(code int, err error, stderr io.Writer) int {
	if err != nil {
		fmt.Fprintln(stderr, err)
		if code != 0 {
			return code
		}
		return 2
	}
	return code
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
