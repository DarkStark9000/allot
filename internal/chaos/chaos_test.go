package chaos_test

import (
	"os"
	"testing"

	"github.com/DarkStark9000/allot/internal/chaos"
)

// TestChaos runs a small end-to-end run with every fault on and fails on any violation.
// cmd/chaos runs the large version.
func TestChaos(t *testing.T) {
	url := os.Getenv("ALLOT_TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("ALLOT_REQUIRE_DB") != "" {
			t.Fatal("ALLOT_TEST_DATABASE_URL is not set and ALLOT_REQUIRE_DB is")
		}
		t.Skip("set ALLOT_TEST_DATABASE_URL to run the chaos test")
	}
	if testing.Short() {
		t.Skip("chaos run skipped in -short mode")
	}
	cfg := chaos.Defaults(url, 42)
	cfg.Orders = 80
	rep, err := chaos.Run(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Orders != cfg.Orders {
		t.Errorf("placed %d orders, want %d", rep.Orders, cfg.Orders)
	}
	for _, v := range rep.Violations {
		t.Errorf("%s %s: %s", v.Guarantee, v.OrderID, v.Detail)
	}
}
