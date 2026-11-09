package api

import (
	"net/http"
)

// chunkRefDTO is one entry of the reader's chunk index.
type chunkRefDTO struct {
	ID    string `json:"id"`
	Seq   int    `json:"seq"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

// contentResponse is the reader payload: the document's metadata, its
// cleaned text rendered to HTML, and the ordered chunk index.
type contentResponse struct {
	Document documentDTO   `json:"document"`
	HTML     string        `json:"html"`
	Chunks   []chunkRefDTO `json:"chunks"`
}

// handleDocumentContent reconstructs a document's cleaned text from its
// stored chunks and renders it as markdown for the built-in reader.
func (s *Server) handleDocumentContent(w http.ResponseWriter, r *http.Request) {
	if !s.storeOr503(w) {
		return
	}
	doc, err := s.store.Document(r.Context(), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err, "document")
		return
	}
	chunks, err := s.store.Chunks(r.Context(), doc.ID)
	if err != nil {
		writeStoreError(w, err, "chunks")
		return
	}
	html, err := markdownHTML(foldChunks(chunks))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "render_error", err.Error())
		return
	}
	refs := make([]chunkRefDTO, 0, len(chunks))
	for _, c := range chunks {
		refs = append(refs, chunkRefDTO{ID: c.ID, Seq: c.Seq, Start: c.Start, End: c.End})
	}
	writeJSON(w, http.StatusOK, contentResponse{Document: toDocumentDTO(*doc), HTML: html, Chunks: refs})
}
