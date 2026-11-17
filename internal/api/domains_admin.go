package api

import (
	"context"
	"errors"
	"net"
	"net/http"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

type domainOperator interface {
	RunDomainOp(context.Context, ingest.DomainOpRequest) (graph.DomainOp, error)
	UndoDomainOp(context.Context, string) (graph.DomainOp, error)
}

func (s *Server) handleDomainAdmin(w http.ResponseWriter, r *http.Request) {
	// Trust the socket peer, never proxy headers. Edge traffic has a non-loopback
	// peer and these paths are also absent from its public allowlist.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() {
		writeError(w, http.StatusForbidden, "loopback_only", "domain admin routes require a loopback peer")
		return
	}
	var req struct {
		DocumentIDs []string `json:"document_ids"`
		Domain      string   `json:"domain"`
		OpID        string   `json:"op_id"`
		From        string   `json:"from"`
		Into        string   `json:"into"`
		Name        string   `json:"name"`
	}
	if !readJSON(w, r, jsonBodyCap, &req) {
		return
	}
	action := r.PathValue("action")
	var op graph.DomainOp
	if action == "rename" {
		if !graph.ValidDomain(req.Domain) || req.Name == "" {
			err = graph.ErrInvalidDomainOp
		} else if s.store == nil {
			err = ingest.ErrStore
		} else {
			op, err = s.store.RenameDomain(r.Context(), req.Domain, req.Name)
		}
	} else {
		p, ok := s.in.(domainOperator)
		if !ok {
			writeError(w, 501, "domain_ops_disabled", "no domain admin pipeline wired")
			return
		}
		switch action {
		case "move":
			op, err = p.RunDomainOp(r.Context(), ingest.DomainOpRequest{OpID: req.OpID, Action: action, DocIDs: req.DocumentIDs, Target: req.Domain})
		case "merge":
			op, err = p.RunDomainOp(r.Context(), ingest.DomainOpRequest{OpID: req.OpID, Action: action, From: req.From, Target: req.Into})
		case "undo":
			if req.OpID == "" {
				err = graph.ErrInvalidDomainOp
			} else {
				op, err = p.UndoDomainOp(r.Context(), req.OpID)
			}
		}
	}
	if err != nil {
		switch {
		case errors.Is(err, graph.ErrSchemaNotV4):
			writeError(w, 409, "schema_not_v4", err.Error())
		case errors.Is(err, graph.ErrNotDone):
			writeError(w, 409, "not_done", err.Error())
		case errors.Is(err, graph.ErrNotFound):
			writeError(w, 404, "not_found", err.Error())
		case errors.Is(err, graph.ErrInvalidDomainOp):
			writeError(w, 400, "invalid_domain_op", err.Error())
		default:
			writeStoreError(w, err, "domain operation")
		}
		return
	}
	writeJSON(w, 202, struct {
		OpID      string   `json:"op_id"`
		State     string   `json:"state"`
		Remaining []string `json:"remaining,omitempty"`
		Skipped   []string `json:"skipped,omitempty"`
	}{op.OpID, op.State, op.Remaining, op.Skipped})
}
