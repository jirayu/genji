package query_test

import (
	"context"
	"testing"

	"github.com/genjidb/genji"
	"github.com/genjidb/genji/database"
	"github.com/genjidb/genji/document"
	"github.com/stretchr/testify/require"
)

func TestDropTable(t *testing.T) {
	db, err := genji.Open(":memory:")
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()

	err = db.Exec(ctx, "CREATE TABLE test1; CREATE TABLE test2; CREATE TABLE test3")
	require.NoError(t, err)

	err = db.Exec(ctx, "DROP TABLE test1")
	require.NoError(t, err)

	err = db.Exec(ctx, "DROP TABLE IF EXISTS test1")
	require.NoError(t, err)

	// Dropping a table that doesn't exist without "IF EXISTS"
	// should return an error.
	err = db.Exec(ctx, "DROP TABLE test1")
	require.Error(t, err)

	// Assert that only the table `test1` has been dropped.
	res, err := db.Query(ctx, "SELECT table_name FROM __genji_tables")
	require.NoError(t, err)
	var tables []string
	err = res.Iterate(func(d document.Document) error {
		v, err := d.GetByField("table_name")
		if err != nil {
			return err
		}
		tables = append(tables, v.V.(string))
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, res.Close())

	require.Len(t, tables, 2)

	// Dropping a read-only table should fail.
	err = db.Exec(ctx, "DROP TABLE __genji_tables")
	require.Error(t, err)
}

func TestDropIndex(t *testing.T) {
	db, err := genji.Open(":memory:")
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()

	err = db.Exec(ctx, `
		CREATE TABLE test1(foo text); CREATE INDEX idx_test1_foo ON test1(foo);
		CREATE TABLE test2(bar text); CREATE INDEX idx_test2_bar ON test2(bar);
	`)
	require.NoError(t, err)

	err = db.Exec(ctx, "DROP INDEX idx_test2_bar")
	require.NoError(t, err)

	// Assert that the good index has been dropped.
	var indexes []*database.IndexConfig
	err = db.View(func(tx *genji.Tx) error {
		var err error
		indexes, err = tx.ListIndexes()
		return err
	})
	require.Len(t, indexes, 1)
	require.Equal(t, "test1", indexes[0].TableName)
	require.Equal(t, "idx_test1_foo", indexes[0].IndexName)
	require.Equal(t, false, indexes[0].Unique)
}
