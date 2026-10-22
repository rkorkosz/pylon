KSeF documentation url: https://api.ksef.mf.gov.pl/docs/v2/index.html
This is a go project that uses go 1.27.
Go best practices and modern features MUST be used.
For Go code navigation, use LSP tools first: lsp_definition for declarations, lsp_references for callers, and lsp_diagnostics after edits. If LSP doesn’t return a result, explain that and fall back to text search
After implementation is done run `go test ./...` check if lsp_diagnostics is not reporting anything, run gofmt, golangci-lint run
