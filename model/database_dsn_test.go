package model

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type dsnProbeRow struct {
	Id   int
	Kind string
}

func (dsnProbeRow) TableName() string { return "dsn_probe_rows" }

// openDSNProbeDB opens a real file database through the same driver the
// deployment uses, with one connection so the writer lock is held by this pool
// alone.
func openDSNProbeDB(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// TestSQLitePathDSNKeepsConcurrencyProtections is the regression for the
// deployment defect where SQLITE_PATH set to a bare file path replaced the whole
// default DSN, silently dropping WAL, the 30s busy timeout and BEGIN IMMEDIATE.
// A read-then-write transaction then failed with SQLITE_BUSY_SNAPSHOT the
// instant another connection committed, no matter how long the timeout was.
func TestSQLitePathDSNKeepsConcurrencyProtections(t *testing.T) {
	file := filepath.Join(t.TempDir(), "qydion-api.db")
	db := openDSNProbeDB(t, common.WithSQLiteConcurrencyParams(file))

	require.NoError(t, db.AutoMigrate(&dsnProbeRow{}))

	var journalMode string
	require.NoError(t, db.Raw("PRAGMA journal_mode").Scan(&journalMode).Error)
	assert.Equal(t, "wal", strings.ToLower(journalMode),
		"a custom database file must still get WAL, not the DELETE journal of a bare path")

	var busyTimeout int
	require.NoError(t, db.Raw("PRAGMA busy_timeout").Scan(&busyTimeout).Error)
	assert.Equal(t, 30000, busyTimeout,
		"the busy timeout must reach the driver as a DSN pragma; the plain _busy_timeout form is ignored")

	// Two independent connections each running "read then write" in one
	// transaction: with the parameters dropped this surfaces as SQLITE_BUSY.
	writerB := openDSNProbeDB(t, common.WithSQLiteConcurrencyParams(file))
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dbx := db
			if i == 1 {
				dbx = writerB
			}
			for n := 0; n < 10; n++ {
				err := dbx.Transaction(func(tx *gorm.DB) error {
					var row dsnProbeRow
					if err := tx.First(&row).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
						return err
					}
					return tx.Create(&dsnProbeRow{Kind: fmt.Sprintf("writer-%d", i)}).Error
				})
				if err != nil {
					errs[i] = err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		assert.NoError(t, err, "concurrent read-then-write transaction %d failed", i)
	}

	var count int64
	require.NoError(t, db.Model(&dsnProbeRow{}).Count(&count).Error)
	assert.Equal(t, int64(20), count)
}
