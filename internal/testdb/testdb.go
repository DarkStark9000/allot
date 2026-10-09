// Package testdb gives each test its own migrated schema in a real PostgreSQL database.
//
// Set ALLOT_TEST_DATABASE_URL to run database tests. Without it they are skipped,
// unless ALLOT_REQUIRE_DB is set, as it is in CI, which turns the skip into a failure.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/DarkStark9000/allot/internal/store"
)

// New returns a migrated database in a fresh schema that is dropped when the test ends.
func New(t testing.TB) *store.DB {
	t.Helper()
	url := os.Getenv("ALLOT_TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("ALLOT_REQUIRE_DB") != "" {
			t.Fatal("ALLOT_TEST_DATABASE_URL is not set and ALLOT_REQUIRE_DB is")
		}
		t.Skip("set ALLOT_TEST_DATABASE_URL to run database tests")
	}
	ctx := context.Background()
	schema := "t_" + hex.EncodeToString(randomBytes(6))
	scoped, err := store.CreateSchema(ctx, url, schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.DropSchema(context.Background(), url, schema); err != nil {
			t.Errorf("drop schema: %v", err)
		}
	})
	db, err := store.Open(ctx, scoped)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return db
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return b
}
