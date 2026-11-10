package ingest

// SetBeforeSupersede installs a test hook between listing older live versions
// and superseding them. Set it before calling Process.
func SetBeforeSupersede(p *Pipeline, f func(olds []string)) {
	p.beforeSupersede = f
}
