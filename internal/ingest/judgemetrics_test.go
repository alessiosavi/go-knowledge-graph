package ingest_test

import (
	"context"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/obs"
)

type cacheAwareJudge struct{ m ingest.JudgeCacheMetrics }

func (*cacheAwareJudge) VerifyFact(context.Context, ingest.FactInput) (ingest.Verdict, error) {
	return ingest.Verdict{}, nil
}

func (j *cacheAwareJudge) SetCacheMetrics(m ingest.JudgeCacheMetrics) { j.m = m }

// TestPipelinePassesCacheMetricsToJudge pins the wiring of the verdict-cache
// counter: whichever of SetJudge and SetMetrics comes first, a judge that
// reports cache results receives the metrics sink.
func TestPipelinePassesCacheMetricsToJudge(t *testing.T) {
	for _, judgeFirst := range []bool{true, false} {
		p := ingest.NewPipeline(config.Config{}, nil, nil, nil, nil)
		j := &cacheAwareJudge{}
		m := obs.New()
		if judgeFirst {
			p.SetJudge(j)
			p.SetMetrics(m)
		} else {
			p.SetMetrics(m)
			p.SetJudge(j)
		}
		if j.m != m {
			t.Errorf("judgeFirst=%v: judge got metrics %v, want the pipeline's", judgeFirst, j.m)
		}
	}
}
