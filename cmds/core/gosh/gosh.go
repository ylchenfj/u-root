// Copyright 2023 the u-root Authors. All rights reserved
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//
// Derived work from Daniel Martí <mvdan@mvdan.cc>

//go:build !tinygo && !plan9
// +build !tinygo,!plan9

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"

	"golang.org/x/term"

	"github.com/knz/bubbline"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

const HISTFILE = "/tmp/bubble-sh.history" //TODO: make configurable

var interactive = flag.Bool("interactive", true, "interactive mode")

func main() {
	flag.Parse()
	err := run(os.Stdin, os.Stdout, os.Stderr, *interactive, flag.Args()...)

	if status, ok := interp.IsExitStatus(err); ok {
		os.Exit(int(status))
	}

	if err != nil {
		log.Fatal(err)
	}
}

func run(stdin io.Reader, stdout, stderr io.Writer, interactive bool, args ...string) error {
	runner, err := interp.New(interp.StdIO(stdin, stdout, stderr))
	if err != nil {
		return err
	}

	parser := syntax.NewParser()

	if len(args) > 0 {
		if strings.HasSuffix(args[0], "sh") {
			return runScript(runner, parser, args[0])
		}

		return runCmd(runner, parser, strings.NewReader(strings.Join(args, " ")), args[0])
	}

	if interactive {
		if r, ok := stdin.(*os.File); ok && term.IsTerminal(int(r.Fd())) {
			return runInteractive(runner, parser, os.Stdout, os.Stderr)
		}
	}

	return runCmd(runner, parser, stdin, "")
}

func runScript(runner *interp.Runner, parser *syntax.Parser, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()

	prog, err := parser.Parse(f, file)
	if err != nil {
		return err
	}

	runner.Reset()

	return runner.Run(context.Background(), prog)
}

func runCmd(runner *interp.Runner, parser *syntax.Parser, command io.Reader, name string) error {
	scanner := bufio.NewScanner(command)
	defer runner.Reset()

	for scanner.Scan() {
		h := scanner.Text()
		if err := scanner.Err(); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		prog, err := parser.Parse(bytes.NewBuffer([]byte(h)), name)
		if err != nil {
			return err
		}

		if err := runner.Run(context.Background(), prog); err != nil {
			return err
		}
	}
	return nil
}

func runInteractive(runner *interp.Runner, parser *syntax.Parser, stdout, stderr io.Writer) error {
	input := bubbline.New()

	if err := input.LoadHistory(HISTFILE); err != nil {
		return err
	}

	input.SetAutoSaveHistory(HISTFILE, true)

	input.AutoComplete = autocomplete

	var runErr error

	// The following code is used to intercept SIGINT signals.
	// Calling signal.Ignore wouldn't work as child prcesses inherit this trait.
	// We only want to catch SIGINTs that are propagated from a child,
	// the child itself should get the signal as per usual.
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	go func(ch chan os.Signal) {
		for {
			<-ch
		}
	}(ch)

	for {
		if runErr != nil {
			fmt.Fprintf(stdout, "error: %s\n", runErr.Error())
			runErr = nil
		}

		line, err := input.GetLine()

		if err != nil {
			if err == io.EOF {
				break // maybe we should continue instead of break
			}
			if errors.Is(err, bubbline.ErrInterrupted) {
				fmt.Fprintf(stdout, "^C\n")
			} else {
				fmt.Fprintf(stderr, "error: %s\n", err.Error())
			}
			continue
		}

		if line == "exit" {
			break
		}

		// check if we want to execute a shell script
		fields := strings.Fields(line)
		if len(fields) > 0 && strings.HasSuffix(fields[0], "sh") {
			if err := runScript(runner, parser, fields[0]); err != nil {
				fmt.Fprintf(stderr, "error: %s\n", err.Error())
			}

			continue
		}

		if line != "" {
			if err := input.AddHistory(line); err != nil {
				fmt.Fprintf(stdout, "unable to add %s to history: %v\n", line, err)
			}
		}

		if err := parser.Stmts(strings.NewReader(line), func(stmt *syntax.Stmt) bool {
			if parser.Incomplete() {
				fmt.Fprintf(stdout, "-> ")

				return true
			}

			runErr = runner.Run(context.Background(), stmt)

			return !runner.Exited()
		}); err != nil {
			fmt.Fprintf(stderr, "error: %s\n", err.Error())
		}
	}

	return nil
}
