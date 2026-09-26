// Package pgx stands in for the driver: grantcheck recognises SQL calls by this import path.
package pgx

import "context"

type Row interface{ Scan(dest ...any) error }

type Tx interface {
	Exec(ctx context.Context, sql string, args ...any) (any, error)
	QueryRow(ctx context.Context, sql string, args ...any) Row
}
