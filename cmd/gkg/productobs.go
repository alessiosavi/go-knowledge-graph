package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

const (
	featuresCheckUsage   = "usage: gkg features check R.json"
	featuresImportUsage  = "usage: gkg features import R.json"
	productsEmitUsage    = "usage: gkg products emit -registry R.json [-release CANDIDATES.json] [-out PATH] FILE.jsonl"
	releaseUsage         = "usage: gkg products release {create|activate|list}"
	releaseCreateUsage   = "usage: gkg products release create -registry REV -candidates FILE -store-domain S=D [-store-domain S=D …] -approved-by NAME -pin name=FILE [-pin name=FILE …] [-id ID]"
	releaseActivateUsage = "usage: gkg products release activate ID"
	releaseListUsage     = "usage: gkg products release list"
)

func runFeatures(args []string, stdout, stderr io.Writer) int {
	switch {
	case len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help"):
		fmt.Fprintln(stdout, featuresCheckUsage)
		fmt.Fprintln(stdout, featuresImportUsage)
		return 0
	case len(args) > 0 && args[0] == "import":
		return runFeatureImport(args[1:], stdout, stderr)
	case len(args) == 0 || args[0] != "check":
		fmt.Fprintln(stderr, featuresCheckUsage)
		fmt.Fprintln(stderr, featuresImportUsage)
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
		fmt.Fprintln(stdout, releaseUsage)
		return 0
	case len(args) > 0 && args[0] == "release":
		return runProductRelease(args[1:], stdout, stderr)
	case len(args) == 0 || args[0] != "emit":
		fmt.Fprintln(stderr, productsEmitUsage)
		fmt.Fprintln(stderr, releaseUsage)
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

func runFeatureImport(args []string, stdout, stderr io.Writer) int {
	fs := productFlags("features import", featuresImportUsage, stderr)
	if err := fs.Parse(args); err != nil {
		return productFlagExit(err)
	}
	if fs.NArg() != 1 || fs.Arg(0) == "" {
		fs.Usage()
		return 2
	}
	data, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return productCommandError(fs.Name(), stderr, fmt.Errorf("read registry: %w", err))
	}
	return withProductStore(fs.Name(), stderr, func(ctx context.Context, st *graph.Store) error {
		result, err := st.ImportFeatureRegistry(ctx, data)
		if err != nil {
			return err
		}
		action := "imported"
		if result.Unchanged {
			action = "unchanged"
		}
		_, err = fmt.Fprintf(stdout, "%s rev=%s digest=%s\n", action, result.Rev, result.Digest)
		return err
	})
}

func runProductRelease(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, releaseUsage)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, releaseUsage)
		return 0
	case "create":
		return runProductReleaseCreate(args[1:], stdout, stderr)
	case "activate":
		fs := productFlags("products release activate", releaseActivateUsage, stderr)
		if slices.ContainsFunc(args[1:], func(arg string) bool {
			name, _, _ := strings.Cut(arg, "=")
			return name == "-materialize" || name == "--materialize"
		}) {
			fmt.Fprintln(stderr, "gkg products release activate: -materialize is not yet supported")
			return 2
		}
		if err := fs.Parse(args[1:]); err != nil {
			return productFlagExit(err)
		}
		if fs.NArg() != 1 || fs.Arg(0) == "" {
			fs.Usage()
			return 2
		}
		return withProductStore(fs.Name(), stderr, func(ctx context.Context, st *graph.Store) error {
			if err := st.ActivateProductRelease(ctx, fs.Arg(0)); err != nil {
				return err
			}
			_, err := fmt.Fprintf(stdout, "activated id=%s\n", fs.Arg(0))
			return err
		})
	case "list":
		fs := productFlags("products release list", releaseListUsage, stderr)
		if err := fs.Parse(args[1:]); err != nil {
			return productFlagExit(err)
		}
		if fs.NArg() != 0 {
			fs.Usage()
			return 2
		}
		return withProductStore(fs.Name(), stderr, func(ctx context.Context, st *graph.Store) error {
			releases, err := st.ListProductReleases(ctx)
			if err != nil {
				return err
			}
			return json.NewEncoder(stdout).Encode(releases)
		})
	default:
		fmt.Fprintln(stderr, releaseUsage)
		return 2
	}
}

func runProductReleaseCreate(args []string, stdout, stderr io.Writer) int {
	fs := productFlags("products release create", releaseCreateUsage, stderr)
	rev := fs.String("registry", "", "imported registry revision")
	candidates := fs.String("candidates", "", "release candidates file")
	approvedBy := fs.String("approved-by", "", "release approver")
	id := fs.String("id", "", "release ID (default: derived from release inputs)")
	storeDomains := make(map[string]string)
	pinPaths := make(map[string]string)
	fs.Func("store-domain", "store=domain binding (repeatable)", productAssignments(storeDomains))
	fs.Func("pin", "name=file SHA-256 pin (repeatable)", productAssignments(pinPaths))
	if err := fs.Parse(args); err != nil {
		return productFlagExit(err)
	}
	missingPins := graph.MissingProductReleasePins(pinPaths)
	if fs.NArg() != 0 || *rev == "" || *candidates == "" || strings.TrimSpace(*approvedBy) == "" ||
		len(storeDomains) == 0 || len(missingPins) != 0 {
		if len(missingPins) != 0 {
			fmt.Fprintf(stderr, "missing required -pin: %s\n", strings.Join(missingPins, ", "))
		}
		fs.Usage()
		return 2
	}
	data, err := os.ReadFile(*candidates)
	if err != nil {
		return productCommandError(fs.Name(), stderr, fmt.Errorf("read release candidates: %w", err))
	}
	pins := make(map[string]graph.ProductReleasePin, len(pinPaths))
	for name, path := range pinPaths {
		pin, err := readProductPin(name, path)
		if err != nil {
			return productCommandError(fs.Name(), stderr, fmt.Errorf("read pin %q: %w", name, err))
		}
		pins[name] = pin
	}
	input := graph.ProductReleaseInput{ID: *id, RegistryRev: *rev, Candidates: data,
		StoreDomain: storeDomains, ApprovedBy: *approvedBy, Pins: pins}
	return withProductStore(fs.Name(), stderr, func(ctx context.Context, st *graph.Store) error {
		result, err := st.CreateProductRelease(ctx, input)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "id=%s manifest_digest=%s\n", result.ID, result.ManifestDigest)
		return err
	})
}

func productAssignments(dst map[string]string) func(string) error {
	return func(arg string) error {
		key, value, ok := strings.Cut(arg, "=")
		if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			return errors.New("expected a non-empty key=value pair")
		}
		if _, exists := dst[key]; exists {
			return fmt.Errorf("duplicate key %q", key)
		}
		dst[key] = value
		return nil
	}
}

func readProductPin(name, path string) (graph.ProductReleasePin, error) {
	switch name {
	case "freeze.json", "score.json", "report.json":
		data, err := os.ReadFile(path)
		return graph.ProductReleasePin{Digest: fmt.Sprintf("%x", sha256.Sum256(data)), Metadata: data}, err
	default:
		file, err := os.Open(path)
		if err != nil {
			return graph.ProductReleasePin{}, err
		}
		defer file.Close()
		hash := sha256.New()
		_, err = io.Copy(hash, file)
		return graph.ProductReleasePin{Digest: fmt.Sprintf("%x", hash.Sum(nil))}, err
	}
}

func productFlags(name, usage string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, usage) }
	return fs
}

func productFlagExit(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}

func withProductStore(command string, stderr io.Writer, run func(context.Context, *graph.Store) error) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := loadConfig()
	if err != nil {
		return productCommandError(command, stderr, err)
	}
	st, err := graph.Open(ctx, cfg)
	if err != nil {
		return productCommandError(command, stderr, err)
	}
	defer st.Close(context.WithoutCancel(ctx))
	return productCommandError(command, stderr, run(ctx, st))
}

func productCommandError(command string, stderr io.Writer, err error) int {
	if err == nil {
		return 0
	}
	fmt.Fprintf(stderr, "gkg %s: %v\n", command, err)
	if errors.Is(err, graph.ErrProductData) {
		return 5
	}
	return 1
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
