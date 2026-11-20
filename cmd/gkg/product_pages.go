package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

const productsPagesReportUsage = "usage: gkg products pages-report [-store S] [-json]"

func runProductPagesReport(args []string, stdout, stderr io.Writer) int {
	fs := productFlags("products pages-report", productsPagesReportUsage, stderr)
	store := fs.String("store", "", "bound store ID (default: all bound stores)")
	asJSON := fs.Bool("json", false, "write the report as JSON")
	if err := fs.Parse(args); err != nil {
		return productFlagExit(err)
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	return withProductStore(fs.Name(), stderr, func(ctx context.Context, st *graph.Store) error {
		reports, err := st.ReportProductPages(ctx, *store)
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(stdout).Encode(reports)
		}
		if len(reports) == 0 {
			_, err := fmt.Fprintln(stdout, "No bound product stores.")
			return err
		}
		for _, r := range reports {
			if _, err := fmt.Fprintf(stdout, "store=%s domain=%s zero=%d one=%d multiple=%d missing_product_ids=%v\n",
				r.Store, r.Domain, r.Zero, r.One, r.Multiple, r.MissingProductIDs); err != nil {
				return err
			}
		}
		return nil
	})
}
