package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/search"
)

type productMentionsReader interface {
	GetProductWithMentions(context.Context, graph.ProductLookup) (graph.ProductDetailWithMentions, graph.ProductMentionRead, error)
}

func (s *Server) handleProduct(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	params := r.URL.Query()
	var withMentions bool
	if params.Has("with_mentions") {
		enabled, err := strconv.ParseBool(params.Get("with_mentions"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "with_mentions must be a boolean")
			return
		}
		withMentions = enabled
	}
	var limit int
	if params.Has("mentions_limit") {
		n, err := strconv.Atoi(params.Get("mentions_limit"))
		if err != nil || !graph.ValidMentionsLimit(n) {
			writeError(w, http.StatusBadRequest, "invalid_request", "mentions_limit must be an integer between 1 and 20")
			return
		}
		limit = n
	}
	lookup := graph.ProductLookup{ID: id, Domain: params.Get("domain"), TextKeys: params["text_keys"]}
	if params.Has("with_texts") {
		enabled, err := strconv.ParseBool(params.Get("with_texts"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "with_texts must be a boolean")
			return
		}
		lookup.WithTexts = enabled
	}
	if id == "" {
		lookup.Store, lookup.ProductID = params.Get("store"), params.Get("product_id")
	}
	if !s.storeOr503(w) {
		return
	}
	if reader, ok := s.store.(productMentionsReader); withMentions && ok {
		product, _, err := reader.GetProductWithMentions(r.Context(), lookup)
		if err != nil {
			writeProductReadError(w, r, err)
			return
		}
		product.LimitMentions(limit)
		writeJSON(w, http.StatusOK, product)
		return
	}
	product, err := s.store.GetProduct(r.Context(), lookup)
	if err != nil {
		writeProductReadError(w, r, err)
		return
	}
	if withMentions {
		writeJSON(w, http.StatusOK, graph.ProductDetailWithMentions{ProductDetail: product, MentionsError: graph.MentionsIndexUnavailable})
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
	var result any
	var err error
	if req.MeaningQuery() != "" {
		result, err = search.FindProductsQuery(r.Context(), s.store, s.emb, s.cfg.ProductText, req)
	} else {
		result, err = s.store.FindProducts(r.Context(), req)
	}
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
	case errors.Is(err, search.ErrProductMeaningUnavailable):
		writeError(w, http.StatusBadGateway, "meaning_unavailable", search.ErrProductMeaningUnavailable.Error())
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
