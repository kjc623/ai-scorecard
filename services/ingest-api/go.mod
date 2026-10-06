module github.com/shadow-ai-capture/ingest-api

go 1.27.0

require (
	github.com/jackc/pgx/v5 v5.11.0
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3
	github.com/shadow-ai-capture/contracts v0.0.0
	github.com/shadow-ai-capture/device/protocol v0.0.0
	github.com/shadow-ai-capture/platform v0.0.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/text v0.29.0 // indirect
)

replace (
	github.com/shadow-ai-capture/contracts => ../../contracts/generated/go
	github.com/shadow-ai-capture/device/protocol => ../../device/protocol
	github.com/shadow-ai-capture/platform => ../platform
)
