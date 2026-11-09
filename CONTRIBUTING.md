# Contributing

Thanks for improving gkg. This project uses the simplest governance that
keeps the history clean and the licenses clear.

## Developer Certificate of Origin (DCO)

Every commit must be signed off — the full text is at
<https://developercertificate.org>. Signing off certifies that you wrote
the change or have the right to pass the project's license (Apache-2.0) on:

```sh
git commit -s -m "fix(api): reject oversized bodies before decoding"
```

The `Signed-off-by: Your Name <you@example.com>` trailer must match the
commit author. The project follows the DCO; until the sign-off check lands
in CI, maintainers verify it by hand during review.

## Development setup

- Go 1.27+ (the module declares the toolchain; no cgo).
- Python 3.12+ only for the sidecar (`sidecar/`).
- Docker (optional) for the full stack and the acceptance run.
- A live Memgraph for the integration tests (the quickstart's compose
  Memgraph works; point the tests at it):

```sh
docker run --rm -d --name gkg-dev -p 7688:7687 memgraph/memgraph:3.14.0
export GKG_TEST_MEMGRAPH_URI=bolt://localhost:7688
```

## Checks before a pull request

```sh
go build ./... && go vet ./...
go test ./...                            # offline unit tests
GKG_TEST_MEMGRAPH_URI=bolt://localhost:7688 go test -p 1 ./... -count=1
pytest sidecar/                          # sidecar contract tests
bash scripts/acceptance.sh               # end-to-end (mock extractor)
```

`-p 1` matters: the integration tests share one Memgraph and use per-run
unique sources so the accumulated corpus never breaks them.

## Pull requests

- One topic per PR; keep diffs reviewable and describe the behavior change
  plus the evidence (commands run, output).
- New behavior needs tests that fail without the change first.
- CI must be green: `build`, `integration`, `sidecar`, `acceptance`.
- Commit messages follow `type(scope): summary` (feat, fix, docs, test,
  refactor, chore); squash merges keep main linear.

## License and provenance

- Code contributions are Apache-2.0 (see [LICENSE](LICENSE)).
- Do not vendor model weights or datasets without a license review; the
  documented model options and their terms are in the README license table.
- New dependencies need a permissive license compatible with Apache-2.0;
  note them in the PR.

## Reporting problems

Open an issue with: what you ran, what happened, what you expected, and the
relevant logs (the server logs JSON to stderr; `/metrics` and `/readyz`
help pin service state).
