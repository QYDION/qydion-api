package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestWithSQLiteConcurrencyParams guards the deployment invariant: pointing
// SQLITE_PATH at another file must not silently drop WAL, the busy timeout, or
// BEGIN IMMEDIATE, because losing them turns concurrent writes into
// SQLITE_BUSY failures on a database that otherwise looks healthy.
func TestWithSQLiteConcurrencyParams(t *testing.T) {
	bare := WithSQLiteConcurrencyParams("/var/lib/new-api/qydion-api.db")
	assert.Equal(t, "/var/lib/new-api/qydion-api.db?"+sqliteConcurrencyParams, bare)
	for _, param := range []string{"_pragma=busy_timeout(30000)", "_pragma=journal_mode(WAL)", "_txlock=immediate"} {
		assert.Contains(t, bare, param)
	}

	// An operator who stated connection parameters owns them; do not guess over
	// an explicit query string.
	explicit := WithSQLiteConcurrencyParams("/data/api.db?_pragma=busy_timeout(5000)")
	assert.Equal(t, "/data/api.db?_pragma=busy_timeout(5000)", explicit)

	// In-memory DSNs have no journal and no on-disk writer to wait for.
	assert.Equal(t, ":memory:", WithSQLiteConcurrencyParams(":memory:"))
	assert.Equal(t, "file:shared?mode=memory&cache=shared", WithSQLiteConcurrencyParams("file:shared?mode=memory&cache=shared"))

	// The packaged default must keep carrying all three, or the fix is only
	// half a fix for anyone who never sets SQLITE_PATH.
	for _, param := range []string{"busy_timeout(30000)", "journal_mode(WAL)", "_txlock=immediate"} {
		assert.Contains(t, SQLitePath, param)
	}
}
