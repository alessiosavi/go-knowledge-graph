package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
)

// GenerationSettings applies to every provider turn of a single ask.
type GenerationSettings struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"top_p,omitempty"`
	TopK            *int     `json:"top_k,omitempty"`
	ReasoningEffort *string  `json:"reasoning_effort,omitempty"`
}

func (g *GenerationSettings) UnmarshalJSON(data []byte) error {
	// Restrict provider options while preserving the outer ask decoder's
	// compatibility with additional fields.
	type fields GenerationSettings
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode((*fields)(g))
}

type generationCapabilities struct {
	TopK             bool     `json:"top_k"`
	ReasoningEfforts []string `json:"reasoning_efforts"`
}

func (s *Server) generationCapabilities() generationCapabilities {
	return configuredGeneration(s.cfg.Agent, s.llm.model)
}

func configuredGeneration(cfg config.AgentConfig, model string) generationCapabilities {
	efforts := cfg.ReasoningEfforts
	if efforts == nil {
		efforts = []string{}
		if strings.EqualFold(model, "DeepSeek-V4.1-Flash-1017") {
			efforts = []string{"none", "high", "max"}
		}
	}
	return generationCapabilities{TopK: cfg.TopKEnabled, ReasoningEfforts: efforts}
}

func (g *GenerationSettings) validate(caps generationCapabilities) error {
	if g == nil {
		return nil
	}
	switch {
	case g.Temperature != nil && (math.IsNaN(*g.Temperature) || *g.Temperature < 0 || *g.Temperature > 2):
		return errors.New("temperature must be between 0 and 2")
	case g.TopP != nil && (math.IsNaN(*g.TopP) || *g.TopP < 0 || *g.TopP > 1):
		return errors.New("top_p must be between 0 and 1")
	case g.TopK != nil && !caps.TopK:
		return errors.New("top_k is unavailable on this deployment")
	case g.TopK != nil && *g.TopK < 1:
		return errors.New("top_k must be a positive integer")
	case g.ReasoningEffort != nil && !slices.Contains(caps.ReasoningEfforts, *g.ReasoningEffort):
		return errors.New("unsupported reasoning_effort for this deployment")
	}
	return nil
}

func (s *Server) chatRequest(g *GenerationSettings, messages []chatMessage) chatRequest {
	req := chatRequest{Model: s.llm.model, Messages: messages, MaxTokens: s.cfg.Agent.MaxTokens}
	if s.cfg.Agent.Temperature != nil {
		req.Temperature = *s.cfg.Agent.Temperature
	}
	if g != nil {
		if g.Temperature != nil {
			req.Temperature = *g.Temperature
		}
		req.TopP, req.TopK, req.ReasoningEffort = g.TopP, g.TopK, g.ReasoningEffort
		req.customGeneration = *g != (GenerationSettings{})
	}
	return req
}

var (
	generationField = regexp.MustCompile(`(?i)\b(temperature|top[-_ ]?[pk]|reasoning[-_ ]?effort)\b`)
	negotiatedField = regexp.MustCompile(`(?i)\b(tools?|tool[-_ ](choice|calls?|calling)|functions?|function[-_ ]?call(s|ing)?|stream(ing)?)\b`)
)

// A 400/422 with overrides can describe sampling, tools, or streaming.
// Only negotiate a fallback when the provider actually identifies the latter
// capabilities. Ambiguous rejections must reach the caller on the first try.
type generationRejection struct{ *providerError }

func (req chatRequest) classifyRejection(err error) error {
	var pe *providerError
	if !req.customGeneration || !errors.As(err, &pe) || pe.status < 400 {
		return err
	}
	badRequest := pe.status == http.StatusBadRequest || pe.status == http.StatusUnprocessableEntity
	if (badRequest || pe.inBand) && generationField.MatchString(pe.detail) {
		return &generationRejection{pe}
	}
	if !badRequest || negotiatedField.MatchString(pe.detail) {
		return err
	}
	return &generationRejection{pe}
}
