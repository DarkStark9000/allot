package store

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

var schemaName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// CreateSchema creates an empty schema and returns url pointed at it. Tests and the
// chaos harness use it to give every run its own tables in one database.
func CreateSchema(ctx context.Context, url, schema string) (string, error) {
	if !schemaName.MatchString(schema) {
		return "", fmt.Errorf("store: invalid schema name %q", schema)
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return "", fmt.Errorf("store: connect: %w", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		return "", fmt.Errorf("store: create schema: %w", err)
	}
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	return url + sep + "search_path=" + schema, nil
}

// DropSchema removes a schema created by CreateSchema, with everything in it.
func DropSchema(ctx context.Context, url, schema string) error {
	if !schemaName.MatchString(schema) {
		return fmt.Errorf("store: invalid schema name %q", schema)
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	return err
}
