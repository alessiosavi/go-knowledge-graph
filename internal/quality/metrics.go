package quality

import (
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"unicode"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/hygiene"
	"golang.org/x/text/unicode/norm"
)

func ratio(n, total int) *float64 {
	if total == 0 {
		return nil
	}
	v := float64(n) / float64(total)
	return &v
}

func gini(values []int) *float64 {
	values = slices.Clone(values)
	slices.Sort(values)
	var sum, weighted float64
	for i, v := range values {
		sum += float64(v)
		weighted += float64(i+1) * float64(v)
	}
	if sum == 0 {
		return nil
	}
	n := float64(len(values))
	v := 2*weighted/(n*sum) - (n+1)/n
	return &v
}

func entropy(counts []int) *float64 {
	total := 0
	for _, n := range counts {
		total += n
	}
	if total == 0 {
		return nil
	}
	v := 0.0
	for _, n := range counts {
		if n > 0 {
			p := float64(n) / float64(total)
			v -= p * math.Log2(p)
		}
	}
	return &v
}

type stringSet map[string]bool

func add(m map[string]stringSet, key, value string) {
	if m[key] == nil {
		m[key] = make(stringSet)
	}
	m[key][value] = true
}

// selectedDocuments applies a private seed to the full source, before scoping.
// A seed match overrides a stored domain; unmatched sources retain that domain.
func selectedDocuments(docs []document, opt Options) map[string]document {
	out := make(map[string]document)
	for _, d := range docs {
		if opt.Seed != nil {
			if name, ok := opt.Seed.DomainFor(d.source); ok {
				d.domain = name
			}
		}
		if opt.Domain == "" || d.domain == opt.Domain {
			out[d.id] = d
		}
	}
	return out
}

func compute(d dataset, opt Options, presets map[string]hygiene.Preset) Report {
	report, _ := computeWithCensus(d, opt, presets, false)
	return report
}

func computeWithCensus(d dataset, opt Options, presets map[string]hygiene.Preset, census bool) (Report, map[string][]duplicateGroup) {
	docs := selectedDocuments(d.documents, opt)
	chunks := make(map[string]string)
	for _, c := range d.chunks {
		if _, ok := docs[c.docID]; ok {
			chunks[c.id] = c.docID
		}
	}
	entities := make(map[string]entity)
	for _, e := range d.entities {
		entities[e.key] = e
	}
	active := make(stringSet)
	entityChunks := make(map[string]stringSet)
	entityDocs := make(map[string]stringSet)
	chunkEntities := make(map[string]stringSet)
	for _, m := range d.mentions {
		doc, ok := chunks[m.chunk]
		if !ok {
			continue
		}
		if _, ok := entities[m.entity]; !ok {
			continue
		}
		active[m.entity] = true
		add(entityChunks, m.entity, m.chunk)
		add(entityDocs, m.entity, doc)
		add(chunkEntities, m.chunk, m.entity)
	}
	facts := make([]fact, 0, len(d.facts))
	for _, f := range d.facts {
		if _, ok := docs[f.doc]; !ok {
			continue
		}
		if doc, ok := chunks[f.chunk]; !ok || doc != f.doc {
			continue
		}
		if _, ok := entities[f.head]; !ok {
			continue
		}
		if _, ok := entities[f.tail]; !ok {
			continue
		}
		facts = append(facts, f)
		for _, key := range []string{f.head, f.tail} {
			active[key] = true
			add(entityDocs, key, f.doc)
		}
	}
	r := Report{Metrics: MetricsVersion, Documents: len(docs), Chunks: len(chunks), Entities: len(active), HygienePolicy: HygienePolicy,
		Facts: FactCounts{Typed: len(facts), Rejected: d.rejected}}
	if d.version >= graph.SchemaVersionV4 {
		r.Facts.CandidatesUnjudged = &d.unjudged
		r.Facts.CandidatesUnclear = &d.unclear
	}
	counts := make([]int, 0, len(active))
	singletons, singleDoc := 0, 0
	resolvedDocuments := 0
	for _, doc := range docs {
		if doc.domain != "" {
			resolvedDocuments++
		}
	}
	r.DomainCoverage = ratio(resolvedDocuments, len(docs))
	cross := 0
	for key := range active {
		n := len(entityChunks[key])
		r.Mentions += n
		counts = append(counts, n)
		if n == 1 {
			singletons++
		}
		if len(entityDocs[key]) == 1 {
			singleDoc++
		}
		domains := make(stringSet)
		for doc := range entityDocs[key] {
			if name := docs[doc].domain; name != "" {
				domains[name] = true
			}
		}
		if len(domains) > 1 {
			cross++
		}
	}
	if opt.Domain == "" && resolvedDocuments > 0 {
		r.CrossDomainEntities = &cross
	}
	r.SingletonShare = ratio(singletons, r.Entities)
	r.SingleDocumentShare = ratio(singleDoc, r.Entities)
	r.MentionGini = gini(counts)
	slices.Sort(counts)
	// The top ceil(1% * E) entities; include all ties only up to that count.
	top := 0
	for i := 0; i < (len(counts)+99)/100; i++ {
		top += counts[len(counts)-1-i]
	}
	r.Top1PctMentionShare = ratio(top, r.Mentions)
	factMetrics(&r, facts)
	duplicateMetrics(&r, active, entities, entityDocs, docs)
	hygieneMetrics(&r, entities, docs, entityDocs, presets, opt)
	coMentionMetrics(&r, chunkEntities)
	if census {
		return r, duplicateCensusGroups(entities, entityDocs)
	}
	return r, nil
}

type triple struct{ head, rel, tail string }
type pair struct{ a, b string }

func factMetrics(r *Report, facts []fact) {
	adj := make(map[string]stringSet)
	supportChunks := make(map[triple]stringSet)
	supportDocs := make(map[triple]stringSet)
	pairTypes := make(map[pair]stringSet)
	triples := make(map[triple]bool)
	relCounts := make(map[string]int)
	low, lt04 := 0, 0
	for _, f := range facts {
		add(adj, f.head, f.tail)
		add(adj, f.tail, f.head)
		key := triple{f.head, f.rel, f.tail}
		triples[key] = true
		if supportChunks[key] == nil {
			supportChunks[key] = make(stringSet)
			supportDocs[key] = make(stringSet)
		}
		supportChunks[key][f.chunk] = true
		supportDocs[key][f.doc] = true
		relCounts[f.rel]++
		if f.low {
			low++
		}
		if f.confidence < .4 {
			lt04++
		}
	}
	for key := range triples {
		p := pair{key.head, key.tail}
		if p.a > p.b {
			p.a, p.b = p.b, p.a
		}
		if pairTypes[p] == nil {
			pairTypes[p] = make(stringSet)
		}
		pairTypes[p][key.rel] = true
		if key.head < key.tail && triples[triple{key.tail, key.rel, key.head}] {
			r.InversePairs++
		}
		if key.head == key.tail {
			r.SelfLoops++
		}
	}
	for _, types := range pairTypes {
		if len(types) > 1 {
			r.MultiTypePairs++
		}
	}
	componentsCount, giant := components(adj)
	r.FactComponents = componentsCount
	r.GiantShareOfFactNodes = ratio(giant, len(adj))
	r.GiantShareOfEntities = ratio(giant, r.Entities)
	r.IsolatedShare = ratio(r.Entities-len(adj), r.Entities)
	r.LowConfidenceShare = ratio(low, len(facts))
	r.ConfidenceLT04Share = ratio(lt04, len(facts))
	oneChunk, multiDoc := 0, 0
	for key, cs := range supportChunks {
		if len(cs) == 1 {
			oneChunk++
		}
		if len(supportDocs[key]) > 1 {
			multiDoc++
		}
	}
	r.Support = SupportMetrics{Triples: len(triples), OneChunkShare: ratio(oneChunk, len(triples)), MultiDocumentShare: ratio(multiDoc, len(triples))}
	counts := make([]int, 0, len(relCounts))
	for _, n := range relCounts {
		counts = append(counts, n)
	}
	slices.Sort(counts) // Stable floating-point reduction, independent of map order.
	r.RelationTypes = len(relCounts)
	r.RelationEntropyBits = entropy(counts)
	if r.RelationTypes > 0 {
		max := math.Log2(float64(r.RelationTypes))
		r.MaxEntropyBits = &max
	}
}

func components(adj map[string]stringSet) (count, giant int) {
	seen := make(stringSet)
	for start := range adj {
		if seen[start] {
			continue
		}
		count++
		seen[start] = true
		stack := []string{start}
		size := 0
		for len(stack) > 0 {
			at := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			size++
			for next := range adj[at] {
				if !seen[next] {
					seen[next] = true
					stack = append(stack, next)
				}
			}
		}
		if size > giant {
			giant = size
		}
	}
	return
}

func coMentionMetrics(r *Report, chunks map[string]stringSet) {
	weights := make(map[pair]int)
	for _, entities := range chunks {
		keys := make([]string, 0, len(entities))
		for key := range entities {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for i, a := range keys {
			for _, b := range keys[i+1:] {
				weights[pair{a, b}]++
			}
		}
	}
	adj := make(map[string]stringSet)
	one := 0
	for p, w := range weights {
		add(adj, p.a, p.b)
		add(adj, p.b, p.a)
		if w == 1 {
			one++
		}
	}
	_, giant := components(adj)
	r.CoMention = CoMentionMetrics{Nodes: len(adj), Edges: len(weights), Weight1Share: ratio(one, len(weights)), GiantShare: ratio(giant, len(adj))}
}

func duplicateFold(name string) string {
	// NFKD first, so compatibility spaces and full-width letters reach the
	// suffix rule as ASCII.
	name, _ = hygiene.StripCompanySuffix(norm.NFKD.String(strings.TrimSpace(name)))
	name = strings.ToLower(name)
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return -1
	}, name)
}

func documentScoped(key string) bool {
	// v4 domain-prefixed keys wrap the existing #document:name key.
	if strings.HasPrefix(key, "@") {
		_, key, _ = strings.Cut(key, ":")
	}
	return strings.HasPrefix(key, "#")
}

func duplicateMetrics(r *Report, active stringSet, entities map[string]entity, entityDocs map[string]stringSet, docs map[string]document) {
	type groupKey struct{ domain, document, fold string }
	groups := make(map[groupKey]stringSet)
	for key := range active {
		fold := duplicateFold(entities[key].name)
		if fold == "" {
			continue
		}
		for doc := range entityDocs[key] {
			scope := groupKey{domain: docs[doc].domain, fold: fold}
			if documentScoped(key) {
				scope.document = doc
			}
			if groups[scope] == nil {
				groups[scope] = make(stringSet)
			}
			groups[scope][key] = true
		}
	}
	duplicates := make(stringSet)
	for _, group := range groups {
		if len(group) < 2 {
			continue
		}
		r.DuplicateGroups++
		types := make(stringSet)
		for key := range group {
			duplicates[key] = true
			types[entities[key].typ] = true
		}
		if len(types) > 1 {
			r.CrossTypeGroups++
		}
	}
	r.DuplicateEntities = len(duplicates)
}

func hygieneMetrics(r *Report, entities map[string]entity, docs map[string]document, entityDocs map[string]stringSet, presets map[string]hygiene.Preset, opt Options) {
	dropped, renamed := []SampleName{}, []SampleName{}
	noise := 0
	seen := make(stringSet)
	var presetNames []string
	for key, support := range entityDocs {
		e := entities[key]
		var normalized string
		var rule hygiene.Rule
		clear(seen)
		presetNames = presetNames[:0]
		for docID := range support {
			name := docs[docID].preset
			if seen[name] {
				continue
			}
			seen[name] = true
			presetNames = append(presetNames, name)
		}
		slices.Sort(presetNames)
		for _, name := range presetNames {
			cleaned, _, rejected := hygiene.NameOnly(e.name, e.typ, presets[name])
			if rejected == "" {
				normalized, rule = cleaned, ""
				break
			}
			if rule == "" {
				rule = rejected
			}
		}
		if rule != "" {
			noise++
		}
		if opt.HygieneSample <= 0 {
			continue
		}
		item := SampleName{Key: e.key, Type: e.typ, Original: e.name, Normalized: normalized, Rule: string(rule)}
		if rule != "" {
			dropped = append(dropped, item)
		} else if normalized != e.name {
			item.Rule = "normalize"
			renamed = append(renamed, item)
		}
	}
	r.NoiseRuleShare = ratio(noise, r.Entities)
	if opt.HygieneSample <= 0 {
		return
	}
	r.Sample = HygieneSample{Metrics: MetricsVersion, HygienePolicy: HygienePolicy, SampleSeed: opt.SampleSeed, Requested: opt.HygieneSample, AvailableDropped: len(dropped), AvailableRenamed: len(renamed)}
	rng := rand.New(rand.NewPCG(opt.SampleSeed, 0))
	take := func(items []SampleName) []SampleName {
		slices.SortFunc(items, func(a, b SampleName) int { return strings.Compare(a.Key, b.Key) })
		rng.Shuffle(len(items), func(i, j int) { items[i], items[j] = items[j], items[i] })
		if len(items) > opt.HygieneSample {
			items = items[:opt.HygieneSample]
		}
		return items
	}
	r.Sample.Dropped = take(dropped)
	r.Sample.Renamed = take(renamed)
}
