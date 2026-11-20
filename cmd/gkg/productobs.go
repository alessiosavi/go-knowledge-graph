package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

const (
	featuresCheckUsage = "usage: gkg features check R.json"
	productsEmitUsage  = "usage: gkg products emit -registry R.json [-release CANDIDATES.json] [-out PATH] FILE.jsonl"
)

func runFeatures(args []string, stdout, stderr io.Writer) int {
	switch {
	case len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help"):
		fmt.Fprintln(stdout, featuresCheckUsage)
		return 0
	case len(args) == 0 || args[0] != "check":
		fmt.Fprintln(stderr, featuresCheckUsage)
		return 2
	}
	fs := flag.NewFlagSet("features check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(fs.Output(), featuresCheckUsage) }
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 || fs.Arg(0) == "" {
		fs.Usage()
		return 2
	}
	registry, err := readProductRegistry(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "gkg features check: %v\n", err)
		return 1
	}
	stores, features, mapped := registry.Counts()
	fmt.Fprintf(stdout, "sha256 %s registry\nstores=%d features=%d mapped=%d\n", registry.Digest(), stores, features, mapped)
	return 0
}

func runProducts(args []string, stdout, stderr io.Writer) int {
	switch {
	case len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help"):
		fmt.Fprintln(stdout, productsEmitUsage)
		return 0
	case len(args) == 0 || args[0] != "emit":
		fmt.Fprintln(stderr, productsEmitUsage)
		return 2
	}
	fs := flag.NewFlagSet("products emit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), productsEmitUsage)
		fs.PrintDefaults()
	}
	registryPath := fs.String("registry", "", "gkg-features/v1 registry")
	releasePath := fs.String("release", "", "release candidates for this registry")
	outPath := fs.String("out", "", "write to a private file (default: stdout)")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *registryPath == "" || fs.NArg() != 1 || fs.Arg(0) == "" {
		fs.Usage()
		return 2
	}
	if err := emitProducts(stdout, *registryPath, *releasePath, fs.Arg(0), *outPath); err != nil {
		fmt.Fprintf(stderr, "gkg products emit: %v\n", err)
		return 1
	}
	return 0
}

func readProductRegistry(path string) (*productobs.Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read registry: %w", err)
	}
	return productobs.ParseRegistry(data)
}

func emitProducts(stdout io.Writer, registryPath, releasePath, recordsPath, outPath string) error {
	registry, err := readProductRegistry(registryPath)
	if err != nil {
		return err
	}
	var release *productobs.Release
	if releasePath != "" {
		data, err := os.ReadFile(releasePath)
		if err != nil {
			return fmt.Errorf("read release candidates: %w", err)
		}
		release, err = registry.ParseRelease(data)
		if err != nil {
			return err
		}
	}

	file, err := os.Open(recordsPath)
	if err != nil {
		return fmt.Errorf("open records: %w", err)
	}
	defer file.Close()
	records, err := productobs.ReadRecords(file)
	if err != nil {
		return err
	}

	if outPath == "" {
		return registry.Emit(stdout, records, release)
	}

	inputs := []string{registryPath, recordsPath}
	if releasePath != "" {
		inputs = append(inputs, releasePath)
	}
	return writeProductObsFile(outPath, inputs, func(out io.Writer) error {
		return registry.Emit(out, records, release)
	})
}

func writeProductObsFile(path string, inputs []string, emit func(io.Writer) error) error {
	existing, err := os.Lstat(path)
	if err == nil {
		if !existing.Mode().IsRegular() {
			return errors.New("-out must name a regular file")
		}
		for _, input := range inputs {
			info, err := os.Stat(input)
			if err != nil {
				return fmt.Errorf("check input path for -out: %w", err)
			}
			if os.SameFile(existing, info) {
				return errors.New("-out must differ from every input file")
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check -out: %w", err)
	}

	temp, err := os.CreateTemp(filepath.Dir(path), ".gkg-product-obs-*")
	if err != nil {
		return fmt.Errorf("stage output: %w", err)
	}
	defer os.Remove(temp.Name())
	writeErr := emit(temp)
	closeErr := temp.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return fmt.Errorf("close output: %w", closeErr)
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return fmt.Errorf("publish output: %w", err)
	}
	return nil
}
