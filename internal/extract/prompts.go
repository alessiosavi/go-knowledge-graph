package extract

import "encoding/json"

// systemPrompt instructs the model to answer with the extraction schema.
const systemPrompt = `You extract entities and relations from text chunks.
Answer with a single JSON object:
{"results":[{"chunk_id":"<id>","entities":[{"label":"<label>","text":"<verbatim span>","start":<byte offset>,"end":<byte offset>,"score":<0..1>}],"relations":[{"label":"<label>","head":<entity index>,"tail":<entity index>,"score":<0..1>}]}]}
Rules:
- Only spans that appear verbatim in the chunk text, with byte offsets into that chunk.
- Relations only between entities of the same chunk; head/tail are 0-based indexes into that chunk's entities array.
- Use only the requested labels and respect each label's threshold.
- "score" is your confidence between 0 and 1 (0.5 when unsure).`

// userPayload is the JSON body carrying one batch.
type userPayload struct {
	Chunks    []RequestChunk `json:"chunks"`
	Entities  []Label        `json:"entities"`
	Relations []Label        `json:"relations"`
}

// buildUserPayload serializes one batch for the chat request.
func buildUserPayload(chunks []RequestChunk, entities, relations []Label) string {
	b, _ := json.Marshal(userPayload{Chunks: chunks, Entities: entities, Relations: relations})
	return string(b)
}

// wireResponse mirrors the JSON schema the model must answer with.
type wireResponse struct {
	Results []wireChunk `json:"results"`
}

type wireChunk struct {
	ChunkID   string         `json:"chunk_id"`
	Entities  []wireEntity   `json:"entities"`
	Relations []wireRelation `json:"relations"`
}

type wireEntity struct {
	Label string   `json:"label"`
	Text  string   `json:"text"`
	Start int      `json:"start"`
	End   int      `json:"end"`
	Score *float64 `json:"score"`
}

type wireRelation struct {
	Label string   `json:"label"`
	Head  int      `json:"head"`
	Tail  int      `json:"tail"`
	Score *float64 `json:"score"`
}
