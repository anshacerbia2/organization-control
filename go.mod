module github.com/anshacerbia2/organization-control

// The language floor matches foundation-platform rather than the toolchain CI builds
// with. Those are different numbers on purpose: this line is a compatibility statement,
// and GO_VERSION in ci.yml is a testing choice. See foundation-platform README §Go versions.
go 1.25.0

require github.com/anshacerbia2/foundation-platform v0.2.2

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.10.0 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/text v0.39.0 // indirect
)
