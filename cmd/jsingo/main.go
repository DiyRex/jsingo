// Command jsingo builds and inspects jsingo modules.
//
// The build step is what makes a consumer's Go binary self-contained: it
// resolves each module's npm dependencies, bundles them into a single file,
// and leaves that file where go:embed can reach it. Nothing is resolved at
// runtime.
//
//	jsingo build     bundle every module in the tree
//	jsingo doctor    check the environment and the embed hazards
//	jsingo version   print the version
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/DiyRex/jsingo"
)

func main() {
	log := newLogger()

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	// A build can shell out to a bundler; Ctrl-C must reach it rather than
	// leaving an orphan behind.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "build":
		err = runBuild(ctx, log, os.Args[2:])
	case "doctor":
		err = runDoctor(ctx, log, os.Args[2:])
	case "version":
		fmt.Println(jsingo.Version)
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}

	if err != nil {
		log.error("%v", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `jsingo - build and inspect jsingo modules

Usage:
  jsingo build [flags]     bundle every module found in the tree
  jsingo doctor [flags]    check the environment for problems
  jsingo version           print the version

Run "jsingo <command> -h" for the flags of a command.
`)
}

// flagSet returns a flag set that prints to stderr and does not exit on error.
func flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}
