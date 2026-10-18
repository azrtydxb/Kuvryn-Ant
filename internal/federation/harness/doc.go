// Package harness is the multi-instance federation test harness (WP-1.7).
//
// Each simulated instance gets its own PostgreSQL database in the shared
// fixture (scripts/test-postgres.sh), its own secret keyring, a real TLS 1.3
// mTLS federation listener on its own loopback port and its own management
// API server, whose requests and responses are validated against
// api/openapi.yaml. Scenarios drive the instances through the management API
// exactly as an operator would, and inspect peer behaviour on the federation
// listeners directly. Without ANT_TEST_DATABASE_URL the scenarios skip
// (ANT_REQUIRE_INTEGRATION=1 makes them fail instead).
package harness
