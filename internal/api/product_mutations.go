package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
)

func (s *Server) handleProductPut(w http.ResponseWriter, r *http.Request) {
	if !s.storeOr503(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, productobs.MaxPushEventBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", fmt.Sprintf("request body exceeds %d bytes", tooLarge.Limit))
		} else {
			writeError(w, http.StatusBadRequest, "invalid_request", "could not read request body")
		}
		return
	}
	seq, record, err := productobs.ParsePushEvent(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	result, err := s.store.PushProduct(r.Context(), seq, record)
	writeProductMutation(w, result, err)
}

func (s *Server) handleProductDelete(w http.ResponseWriter, r *http.Request) {
	if !s.storeOr503(w) {
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	ids, seqs := query["product_id"], query["seq"]
	if err != nil || len(query) != 2 || len(ids) != 1 || ids[0] == "" || len(seqs) != 1 {
		writeError(w, http.StatusBadRequest, "invalid_request", "provide exactly one product_id and one seq")
		return
	}
	seq, err := strconv.ParseInt(seqs[0], 10, 64)
	if err != nil || seq < 1 {
		writeError(w, http.StatusBadRequest, "invalid_request", "seq must be a positive int64")
		return
	}
	result, err := s.store.RemoveProduct(r.Context(), ids[0], seq)
	writeProductMutation(w, result, err)
}

func writeProductMutation(w http.ResponseWriter, result graph.ProductMutationResult, err error) {
	if err == nil {
		writeJSON(w, http.StatusOK, result)
		return
	}
	var mutation *graph.ProductMutationError
	var invalid argerr.Error
	switch {
	case errors.As(err, &mutation):
		var code int
		var seenSeq *int64
		switch mutation.Status {
		case "stale", "seq_conflict":
			code, seenSeq = http.StatusConflict, mutation.SeenSeq
		case "release_changed", "release_not_materialized", "store_not_bound", "store_cap", "retry", "reimport_required":
			code = http.StatusConflict
		case "unknown_outcome":
			code = http.StatusServiceUnavailable
		default:
			writeStoreErrorStatus(w, http.StatusInternalServerError, err, "mutate product")
			return
		}
		writeJSON(w, code, struct {
			errorBody
			SeenSeq *int64 `json:"seen_seq,omitempty"`
		}{errorBody: errorBody{Error: mutation.Status, Detail: mutation.Error()}, SeenSeq: seenSeq})
	case errors.As(err, &invalid):
		writeError(w, http.StatusBadRequest, "invalid_request", invalid.Error())
	default:
		writeStoreError(w, err, "mutate product")
	}
}
