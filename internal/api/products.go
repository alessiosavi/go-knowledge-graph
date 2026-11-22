package api

import (
	"errors"
	"net/http"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

func (s *Server) handleProduct(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	params := r.URL.Query()
	lookup := graph.ProductLookup{ID: id, Domain: params.Get("domain")}
	if id == "" {
		lookup.Store, lookup.ProductID = params.Get("store"), params.Get("product_id")
	}
	if !s.storeOr503(w) {
		return
	}
	product, err := s.store.GetProduct(r.Context(), lookup)
	if err != nil {
		writeProductReadError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, product)
}

func (s *Server) handleProductStores(w http.ResponseWriter, r *http.Request) {
	if !s.storeOr503(w) {
		return
	}
	stores, err := s.store.ListProductStores(r.Context())
	if err != nil {
		writeReadError(w, r.Context(), err, "list product stores")
		return
	}
	writeJSON(w, http.StatusOK, stores)
}

func (s *Server) handleProductSearch(w http.ResponseWriter, r *http.Request) {
	if !s.storeOr503(w) {
		return
	}
	var req graph.ProductSearchRequest
	if !readJSON(w, r, jsonBodyCap, &req) {
		return
	}
	result, err := s.store.FindProducts(r.Context(), req)
	if err != nil {
		writeProductReadError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleProductFeatures(w http.ResponseWriter, r *http.Request) {
	if !s.storeOr503(w) {
		return
	}
	result, err := s.store.ListProductFeatures(r.Context())
	if err != nil {
		writeReadError(w, r.Context(), err, "list product features")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func writeProductReadError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, graph.ErrProductReadInput):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, graph.ErrProductNotNumeric):
		writeError(w, http.StatusBadRequest, "not_numeric", err.Error())
	case errors.Is(err, graph.ErrProductCursor):
		writeError(w, http.StatusBadRequest, "stale_cursor", err.Error())
	default:
		writeReadError(w, r.Context(), err, "read products")
	}
}
