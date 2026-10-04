module github.com/shadow-ai-capture/aggregator

go 1.27.0

require github.com/jackc/pgx/v5 v5.11.0

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	// pgx v5.11.0 asks for older x/* versions than this repository pins; ordinary minimal
	// version selection is what lets the tagged build resolve entirely offline. They are inert
	// for the default (untagged) build, which carries no third-party dependency at all.
	golang.org/x/crypto v0.56.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.41.0 // indirect
)
