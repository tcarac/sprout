package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Run with SPROUT_TEST_ADMIN_URL pointing to a disposable PostgreSQL server.
func TestConcurrentWorktreesAndSeedRefresh(t *testing.T) {
	adminURL := os.Getenv("SPROUT_TEST_ADMIN_URL")
	if adminURL == "" {
		t.Skip("set SPROUT_TEST_ADMIN_URL for PostgreSQL integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())

	base := t.TempDir()
	source := "pwt_test_" + shortHash(base)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{source}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	sourceURL, err := withDatabase(adminURL, source)
	if err != nil {
		t.Fatal(err)
	}
	sourceConn, err := pgx.Connect(ctx, sourceURL)
	if err != nil {
		t.Fatal(err)
	}
	defer sourceConn.Close(context.Background())
	if _, err := sourceConn.Exec(ctx, "CREATE TABLE items (id integer PRIMARY KEY, value text); INSERT INTO items VALUES (1, 'base')"); err != nil {
		t.Fatal(err)
	}

	main := filepath.Join(base, "main")
	if err := os.Mkdir(main, 0755); err != nil {
		t.Fatal(err)
	}
	runGit(t, main, "init")
	runGit(t, main, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "initial")
	m, err := Init(main, sourceURL)
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 7)
	defer func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		for _, path := range paths {
			if path != "" {
				_ = m.Remove(cleanupCtx, path)
			}
		}
		_ = m.DropSeed(cleanupCtx)
		_, _ = admin.Exec(cleanupCtx, "DROP DATABASE IF EXISTS "+pgx.Identifier{source}.Sanitize()+" WITH (FORCE)")
	}()
	for i := 0; i < 5; i++ {
		paths[i] = filepath.Join(base, fmt.Sprintf("feature-%d", i))
		runGit(t, main, "worktree", "add", "-b", fmt.Sprintf("feature-%d", i), paths[i])
	}

	// Keep a source connection open: a direct template copy cannot use it.
	var group sync.WaitGroup
	errs := make([]error, 4)
	for i := 0; i < 4; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			_, errs[i] = m.Ensure(ctx, paths[i])
		}(i)
	}
	group.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worktree %d: %v", i, err)
		}
	}
	entries, err := m.List(ctx, main)
	if err != nil || len(entries) != 6 {
		t.Fatalf("list returned %d worktrees: %v", len(entries), err)
	}
	for i := 0; i < 4; i++ {
		if count := countRows(t, ctx, m, paths[i], 1); count != 1 {
			t.Fatalf("worktree %d missing source data", i)
		}
	}
	// Agents may issue complete create commands at the same time.
	createErrs := make([]error, 2)
	for i := range createErrs {
		index := i + 5
		paths[index] = filepath.Join(base, fmt.Sprintf("feature-%d", index))
		group.Add(1)
		go func(i, index int) {
			defer group.Done()
			_, _, createErrs[i] = m.Create(ctx, main, fmt.Sprintf("feature-%d", index), paths[index])
		}(i, index)
	}
	group.Wait()
	for i, err := range createErrs {
		if err != nil || countRows(t, ctx, m, paths[i+5], 1) != 1 {
			t.Fatalf("concurrent create %d: %v", i, err)
		}
	}
	firstURL, _ := m.URL(paths[0])
	first, err := pgx.Connect(ctx, firstURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Exec(ctx, "INSERT INTO items VALUES (3, 'only-first')"); err != nil {
		t.Fatal(err)
	}
	first.Close(ctx)
	if count := countRows(t, ctx, m, paths[1], 3); count != 0 {
		t.Fatal("worktree databases share data")
	}
	if _, err := sourceConn.Exec(ctx, "INSERT INTO items VALUES (2, 'new-source')"); err != nil {
		t.Fatal(err)
	}
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", t.TempDir())
	if err := m.Prepare(ctx, true); err == nil {
		t.Fatal("refresh unexpectedly succeeded without pg_dump")
	}
	t.Setenv("PATH", oldPath)
	seed, comment, err := databaseComment(ctx, admin, m.SeedName())
	if err != nil || !seed || comment != m.seedMarker() {
		t.Fatalf("failed refresh destroyed the previous seed: %v", err)
	}
	if err := m.Prepare(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure(ctx, paths[4]); err != nil {
		t.Fatal(err)
	}
	if countRows(t, ctx, m, paths[4], 2) != 1 || countRows(t, ctx, m, paths[0], 2) != 0 {
		t.Fatal("refresh did not affect only new worktrees")
	}
	if err := m.DropSeed(ctx); err != nil {
		t.Fatal(err)
	}
	if countRows(t, ctx, m, paths[4], 2) != 1 {
		t.Fatal("dropping the seed affected an existing worktree")
	}
	if err := m.Prepare(ctx, false); err != nil {
		t.Fatalf("rebuild seed after drop: %v", err)
	}
}

func countRows(t *testing.T, ctx context.Context, m *Manager, path string, id int) int {
	t.Helper()
	url, err := m.URL(path)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var count int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM items WHERE id=$1", id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
