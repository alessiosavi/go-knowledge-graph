package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

const (
	productsPushUsage   = "usage: gkg products push FILE.jsonl"
	productsRemoveUsage = "usage: gkg products remove -product P -seq N"
)

func runProductPush(args []string, stdout, stderr io.Writer) int {
	fs := productFlags("products push", productsPushUsage, stderr)
	if err := fs.Parse(args); err != nil {
		return productFlagExit(err)
	}
	if fs.NArg() != 1 || fs.Arg(0) == "" {
		fs.Usage()
		return 2
	}
	file, err := os.Open(fs.Arg(0))
	if err != nil {
		return productCommandError(fs.Name(), stderr, fmt.Errorf("open push events: %w", err))
	}
	defer file.Close()
	return withProductStore(fs.Name(), stderr, func(ctx context.Context, store *graph.Store) error {
		output := json.NewEncoder(stdout)
		reader := bufio.NewReaderSize(file, productobs.MaxPushEventBytes+1)
		for line := 1; ; line++ {
			raw, readErr := reader.ReadSlice('\n')
			if errors.Is(readErr, bufio.ErrBufferFull) {
				return fmt.Errorf("line %d: push event exceeds 128 KiB", line)
			}
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return fmt.Errorf("read line %d: %w", line, readErr)
			}
			if len(raw) != 0 {
				seq, record, err := productobs.ParsePushEvent(bytes.TrimSuffix(raw, []byte{'\n'}))
				if err != nil {
					return fmt.Errorf("line %d: %w", line, err)
				}
				result, err := store.PushProduct(ctx, seq, record)
				if err != nil {
					return fmt.Errorf("line %d: %w", line, err)
				}
				if err := output.Encode(result); err != nil {
					return fmt.Errorf("write line %d result (%s at seq %d): %w", line, result.Status, seq, err)
				}
			}
			if errors.Is(readErr, io.EOF) {
				return nil
			}
		}
	})
}

func runProductRemove(args []string, stdout, stderr io.Writer) int {
	fs := productFlags("products remove", productsRemoveUsage, stderr)
	product := fs.String("product", "", "full source product ID")
	seq := fs.String("seq", "", "positive store sequence")
	if err := fs.Parse(args); err != nil {
		return productFlagExit(err)
	}
	value, err := strconv.ParseInt(*seq, 10, 64)
	if err != nil || value <= 0 || fs.NArg() != 0 || *product == "" {
		fs.Usage()
		return 2
	}
	return withProductStore(fs.Name(), stderr, func(ctx context.Context, store *graph.Store) error {
		result, err := store.RemoveProduct(ctx, *product, value)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(result)
	})
}
