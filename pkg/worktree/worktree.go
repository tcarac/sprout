// Package worktree gives each Git worktree an isolated PostgreSQL database.
package worktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const EnvFile = ".sprout.env"

type Config struct {
	SourceURL string `json:"source_url"`
}

type Manager struct {
	CommonDir string
	Config    Config
}

type Entry struct {
	Path     string
	Branch   string
	Database string
	Exists   bool
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func Root(dir string) (string, error) {
	root, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(root)
}

func commonDir(dir string) (string, error) {
	root, err := Root(dir)
	if err != nil {
		return "", err
	}
	path, err := git(root, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	return filepath.EvalSymlinks(path)
}

func statePath(common string) string { return filepath.Join(common, "sprout", "config.json") }

func Init(dir, sourceURL string) (*Manager, error) {
	common, err := commonDir(dir)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(statePath(common)); err == nil {
		return nil, errors.New("sprout is already initialized")
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if _, err := sourceDatabase(sourceURL); err != nil {
		return nil, err
	}
	stateDir := filepath.Dir(statePath(common))
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(Config{SourceURL: sourceURL}, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(statePath(common), append(data, '\n'), 0600); err != nil {
		return nil, err
	}
	return &Manager{CommonDir: common, Config: Config{SourceURL: sourceURL}}, nil
}

func Open(dir string) (*Manager, error) {
	common, err := commonDir(dir)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(statePath(common))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errors.New("sprout is not initialized; run sprout init")
		}
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if _, err := sourceDatabase(cfg.SourceURL); err != nil {
		return nil, err
	}
	return &Manager{CommonDir: common, Config: cfg}, nil
}

func sourceDatabase(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return "", errors.New("source URL must be a postgres:// or postgresql:// URL with a host")
	}
	db := strings.TrimPrefix(u.Path, "/")
	if db == "" || strings.Contains(db, "/") {
		return "", errors.New("source URL must name one database")
	}
	return db, nil
}

func withDatabase(raw, database string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	u.Path = "/" + database
	u.RawPath = ""
	return u.String(), nil
}

var unsafeName = regexp.MustCompile(`[^a-z0-9_]+`)

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}

func (m *Manager) DatabaseName(path string) (string, error) {
	root, err := Root(path)
	if err != nil {
		return "", err
	}
	common, err := commonDir(root)
	if err != nil {
		return "", err
	}
	if common != m.CommonDir {
		return "", errors.New("path belongs to another Git repository")
	}
	return m.databaseNameForRoot(root), nil
}

func (m *Manager) databaseNameForRoot(root string) string {
	return m.databasePrefix(47) + "_wt_" + shortHash(root)
}

func (m *Manager) databasePrefix(max int) string {
	source, _ := sourceDatabase(m.Config.SourceURL)
	prefix := unsafeName.ReplaceAllString(strings.ToLower(source), "_")
	prefix = strings.Trim(prefix, "_")
	if prefix == "" {
		prefix = "db"
	}
	if len(prefix) > max {
		prefix = prefix[:max]
	}
	return prefix
}

// SeedName is the shared, immutable template used by new worktree databases.
func (m *Manager) SeedName() string {
	return m.databasePrefix(42) + "_wt_seed_" + shortHash(m.CommonDir)
}

func (m *Manager) stageName() string {
	return m.databasePrefix(41) + "_wt_stage_" + shortHash(m.CommonDir)
}

func (m *Manager) seedMarker() string {
	return "sprout:seed:" + shortHash(m.CommonDir)
}

func (m *Manager) marker(path string) (string, error) {
	root, err := Root(path)
	if err != nil {
		return "", err
	}
	return "sprout:" + shortHash(m.CommonDir) + ":" + shortHash(root), nil
}

func (m *Manager) admin(ctx context.Context) (*pgx.Conn, error) {
	adminURL, err := withDatabase(m.Config.SourceURL, "postgres")
	if err != nil {
		return nil, err
	}
	return pgx.Connect(ctx, adminURL)
}

func databaseComment(ctx context.Context, conn *pgx.Conn, name string) (bool, string, error) {
	var comment *string
	err := conn.QueryRow(ctx, "SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname=$1", name).Scan(&comment)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	if comment == nil {
		return true, "", nil
	}
	return true, *comment, nil
}

func (m *Manager) inspect(ctx context.Context, path string) (string, string, bool, error) {
	name, err := m.DatabaseName(path)
	if err != nil {
		return "", "", false, err
	}
	marker, err := m.marker(path)
	if err != nil {
		return "", "", false, err
	}
	conn, err := m.admin(ctx)
	if err != nil {
		return "", "", false, err
	}
	defer conn.Close(ctx)
	exists, comment, err := databaseComment(ctx, conn, name)
	if err != nil {
		return "", "", false, err
	}
	if exists && comment != marker {
		return "", "", false, fmt.Errorf("database %q already exists but is not owned by this worktree", name)
	}
	return name, marker, exists, nil
}

func (m *Manager) URL(path string) (string, error) {
	name, err := m.DatabaseName(path)
	if err != nil {
		return "", err
	}
	return withDatabase(m.Config.SourceURL, name)
}

func (m *Manager) Ensure(ctx context.Context, path string) (string, error) {
	root, err := Root(path)
	if err != nil {
		return "", err
	}
	name, err := m.DatabaseName(root)
	if err != nil {
		return "", err
	}
	lock, err := m.admin(ctx)
	if err != nil {
		return "", err
	}
	defer lock.Close(ctx)
	if _, err := lock.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", name); err != nil {
		return "", err
	}
	name, marker, exists, err := m.inspect(ctx, root)
	if err != nil {
		return "", err
	}
	if !exists {
		if err := m.withSeed(ctx, func() error {
			return m.createFromTemplate(ctx, name, m.SeedName(), marker)
		}); err != nil {
			return "", err
		}
	}
	url, err := m.URL(root)
	if err != nil {
		return "", err
	}
	if err := writeEnv(root, url); err != nil {
		return "", err
	}
	return url, nil
}

// Prepare builds the shared seed once. Refresh rebuilds it from the current
// source, leaving existing worktree databases unchanged.
func (m *Manager) Prepare(ctx context.Context, refresh bool) error {
	name := m.SeedName()
	conn, err := m.admin(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", name); err != nil {
		return err
	}
	exists, comment, err := databaseComment(ctx, conn, name)
	if err != nil {
		return err
	}
	if exists && comment != m.seedMarker() {
		return fmt.Errorf("seed database %q exists but is not owned by this repository", name)
	}
	if exists && !refresh {
		return nil
	}
	stage := m.stageName()
	stageExists, stageComment, err := databaseComment(ctx, conn, stage)
	if err != nil {
		return err
	}
	if stageExists && stageComment != m.seedMarker() {
		return fmt.Errorf("staging database %q exists but is not owned by this repository", stage)
	}
	if !exists && stageExists {
		if _, err := conn.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{stage}.Sanitize()+" RENAME TO "+pgx.Identifier{name}.Sanitize()); err != nil {
			return err
		}
		exists = true
		stageExists = false
		if !refresh {
			return nil
		}
	}
	if !exists {
		return m.createFromSource(ctx, name, m.seedMarker())
	}
	if stageExists {
		if _, err := conn.Exec(ctx, "DROP DATABASE "+pgx.Identifier{stage}.Sanitize()+" WITH (FORCE)"); err != nil {
			return fmt.Errorf("drop stale staging database: %w", err)
		}
	}
	if err := m.createFromSource(ctx, stage, m.seedMarker()); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("drop old seed: %w", err)
	}
	_, err = conn.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{stage}.Sanitize()+" RENAME TO "+pgx.Identifier{name}.Sanitize())
	return err
}

// DropSeed reclaims the shared snapshot. Existing worktree databases remain.
func (m *Manager) DropSeed(ctx context.Context) error {
	name := m.SeedName()
	conn, err := m.admin(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", name); err != nil {
		return err
	}
	var owned []string
	for _, database := range []string{name, m.stageName()} {
		exists, comment, err := databaseComment(ctx, conn, database)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if comment != m.seedMarker() {
			return fmt.Errorf("database %q exists but is not owned by this repository", database)
		}
		owned = append(owned, database)
	}
	for _, database := range owned {
		if _, err := conn.Exec(ctx, "DROP DATABASE "+pgx.Identifier{database}.Sanitize()+" WITH (FORCE)"); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) withSeed(ctx context.Context, run func() error) error {
	name := m.SeedName()
	conn, err := m.admin(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock_shared(hashtext($1))", name); err != nil {
			return err
		}
		exists, comment, err := databaseComment(ctx, conn, name)
		if err != nil {
			return err
		}
		if exists && comment != m.seedMarker() {
			return fmt.Errorf("seed database %q exists but is not owned by this repository", name)
		}
		if exists {
			return run()
		}
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock_shared(hashtext($1))", name); err != nil {
			return err
		}
		if err := m.Prepare(ctx, false); err != nil {
			return err
		}
	}
	return errors.New("seed database disappeared during preparation")
}

func (m *Manager) createFromSource(ctx context.Context, name, marker string) error {
	conn, err := m.admin(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	source, _ := sourceDatabase(m.Config.SourceURL)
	var busy bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname=$1)", source).Scan(&busy); err != nil {
		return err
	}
	created := false
	if !busy {
		create := fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", pgx.Identifier{name}.Sanitize(), pgx.Identifier{source}.Sanitize())
		_, err = conn.Exec(ctx, create)
		created = err == nil
	}
	if !created {
		// A dump works while applications continue to use the source.
		if _, err = conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
			return fmt.Errorf("create database: %w", err)
		}
		if err = m.copyByDump(ctx, name); err != nil {
			m.dropFailedCreate(name)
			return err
		}
	}
	return m.markCreated(ctx, conn, name, marker)
}

func (m *Manager) createFromTemplate(ctx context.Context, name, template, marker string) error {
	conn, err := m.admin(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	query := fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", pgx.Identifier{name}.Sanitize(), pgx.Identifier{template}.Sanitize())
	if _, err := conn.Exec(ctx, query); err != nil {
		return fmt.Errorf("clone seed database: %w", err)
	}
	return m.markCreated(ctx, conn, name, marker)
}

func (m *Manager) markCreated(ctx context.Context, conn *pgx.Conn, name, marker string) error {
	_, err := conn.Exec(ctx, "COMMENT ON DATABASE "+pgx.Identifier{name}.Sanitize()+" IS '"+marker+"'")
	if err != nil {
		m.dropFailedCreate(name)
		return fmt.Errorf("mark database ownership: %w", err)
	}
	return nil
}

func (m *Manager) dropFailedCreate(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := m.admin(ctx)
	if err != nil {
		return
	}
	defer conn.Close(ctx)
	_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
}

func (m *Manager) copyByDump(ctx context.Context, name string) error {
	target, err := withDatabase(m.Config.SourceURL, name)
	if err != nil {
		return err
	}
	sourceEnv, err := pgEnv(m.Config.SourceURL)
	if err != nil {
		return err
	}
	targetEnv, err := pgEnv(target)
	if err != nil {
		return err
	}
	dump := exec.CommandContext(ctx, "pg_dump", "--format=custom", "--no-owner", "--no-acl")
	dump.Env = append(os.Environ(), sourceEnv...)
	pipe, err := dump.StdoutPipe()
	if err != nil {
		return err
	}
	restore := exec.CommandContext(ctx, "pg_restore", "--dbname", name, "--no-owner", "--no-acl", "--exit-on-error")
	restore.Env = append(os.Environ(), targetEnv...)
	restore.Stdin = pipe
	var dumpErr, restoreErr strings.Builder
	dump.Stderr = &dumpErr
	restore.Stderr = &restoreErr
	if err := restore.Start(); err != nil {
		return fmt.Errorf("start pg_restore: %w", err)
	}
	if err := dump.Start(); err != nil {
		_ = restore.Process.Kill()
		_ = restore.Wait()
		return fmt.Errorf("start pg_dump: %w", err)
	}
	dErr := dump.Wait()
	rErr := restore.Wait()
	if dErr != nil {
		return fmt.Errorf("pg_dump: %s: %w", strings.TrimSpace(dumpErr.String()), dErr)
	}
	if rErr != nil {
		return fmt.Errorf("pg_restore: %s: %w", strings.TrimSpace(restoreErr.String()), rErr)
	}
	return nil
}

func pgEnv(raw string) ([]string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	db, err := sourceDatabase(raw)
	if err != nil {
		return nil, err
	}
	env := []string{"PGHOST=" + u.Hostname(), "PGDATABASE=" + db}
	if u.Port() != "" {
		env = append(env, "PGPORT="+u.Port())
	}
	if u.User != nil {
		env = append(env, "PGUSER="+u.User.Username())
		if password, ok := u.User.Password(); ok {
			env = append(env, "PGPASSWORD="+password)
		}
	}
	for key, variable := range map[string]string{
		"sslmode": "PGSSLMODE", "sslrootcert": "PGSSLROOTCERT",
		"sslcert": "PGSSLCERT", "sslkey": "PGSSLKEY",
		"connect_timeout": "PGCONNECT_TIMEOUT", "application_name": "PGAPPNAME",
		"options": "PGOPTIONS",
	} {
		if value := u.Query().Get(key); value != "" {
			env = append(env, variable+"="+value)
		}
	}
	return env, nil
}

func writeEnv(root, databaseURL string) error {
	path := filepath.Join(root, EnvFile)
	if data, err := os.ReadFile(path); err == nil && !strings.HasPrefix(string(data), "# Generated by sprout\n") {
		return fmt.Errorf("refusing to replace existing %s", path)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	content := "# Generated by sprout\nDATABASE_URL='" + strings.ReplaceAll(databaseURL, "'", "'\\''") + "'\n"
	return os.WriteFile(path, []byte(content), 0600)
}

func (m *Manager) Remove(ctx context.Context, path string) error {
	name, err := m.DatabaseName(path)
	if err != nil {
		return err
	}
	lock, err := m.admin(ctx)
	if err != nil {
		return err
	}
	defer lock.Close(ctx)
	if _, err := lock.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", name); err != nil {
		return err
	}
	name, _, exists, err := m.inspect(ctx, path)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	conn, err := m.admin(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	if err != nil {
		return fmt.Errorf("drop database %s: %w", name, err)
	}
	root, err := Root(path)
	if err == nil {
		file := filepath.Join(root, EnvFile)
		if data, err := os.ReadFile(file); err == nil && strings.HasPrefix(string(data), "# Generated by sprout\n") {
			_ = os.Remove(file)
		}
	}
	return nil
}

func (m *Manager) List(ctx context.Context, dir string) ([]Entry, error) {
	out, err := git(dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	conn, err := m.admin(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, "SELECT datname, shobj_description(oid, 'pg_database') FROM pg_database")
	if err != nil {
		return nil, err
	}
	comments := make(map[string]string)
	for rows.Next() {
		var name string
		var comment *string
		if err := rows.Scan(&name, &comment); err != nil {
			rows.Close()
			return nil, err
		}
		if comment != nil {
			comments[name] = *comment
		} else {
			comments[name] = ""
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	var entries []Entry
	for _, block := range strings.Split(out, "\n\n") {
		if block == "" {
			continue
		}
		var e Entry
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "worktree ") {
				e.Path = strings.TrimPrefix(line, "worktree ")
			} else if strings.HasPrefix(line, "branch refs/heads/") {
				e.Branch = strings.TrimPrefix(line, "branch refs/heads/")
			}
		}
		if e.Path == "" {
			continue
		}
		root, err := filepath.EvalSymlinks(e.Path)
		if err != nil {
			// Git can still list an unpruned worktree whose directory is gone.
			continue
		}
		e.Database = m.databaseNameForRoot(root)
		comment, found := comments[e.Database]
		if found && comment != "sprout:"+shortHash(m.CommonDir)+":"+shortHash(root) {
			return nil, fmt.Errorf("database %q already exists but is not owned by this worktree", e.Database)
		}
		e.Exists = found
		entries = append(entries, e)
	}
	return entries, nil
}

func (m *Manager) Create(ctx context.Context, dir, branch, path string) (string, string, error) {
	if branch == "" || strings.HasPrefix(branch, "-") {
		return "", "", errors.New("a Git branch name is required")
	}
	root, err := Root(dir)
	if err != nil {
		return "", "", err
	}
	if path == "" {
		slug := unsafeName.ReplaceAllString(strings.ToLower(branch), "-")
		path = filepath.Join(filepath.Dir(root), filepath.Base(root)+"-"+strings.Trim(slug, "-"))
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	if _, err := git(root, "check-ref-format", "--branch", branch); err != nil {
		return "", "", err
	}
	if _, err := git(root, "worktree", "add", "-b", branch, path); err != nil {
		return "", "", err
	}
	url, err := m.Ensure(ctx, path)
	if err != nil {
		return path, "", fmt.Errorf("worktree created at %s, but database setup failed: %w", path, err)
	}
	return path, url, nil
}

func (m *Manager) RemoveWorktree(ctx context.Context, dir, path string) error {
	root, err := Root(path)
	if err != nil {
		return err
	}
	mainRoot, err := Root(dir)
	if err != nil {
		return err
	}
	if root == mainRoot {
		return errors.New("refusing to remove the current worktree")
	}
	status, err := git(root, "status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("worktree has uncommitted files; commit or clean it before removal")
	}
	if err := m.Remove(ctx, root); err != nil {
		return err
	}
	_, err = git(dir, "worktree", "remove", root)
	return err
}
