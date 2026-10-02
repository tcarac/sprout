package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tcarac/sprout/pkg/worktree"
)

const help = `sprout gives every Git worktree its own PostgreSQL database.

Usage:
  sprout init --source-url URL     Configure this Git repository
  sprout create BRANCH [PATH]      Create a Git worktree and database
  sprout prepare                   Prepare the shared seed database
  sprout refresh                   Rebuild seed from the current source
  sprout seed-drop                 Remove the shared seed to reclaim space
  sprout ensure                    Create/reuse this worktree's database
  sprout env                       Print a shell export for DATABASE_URL
  sprout url                       Print this worktree's database URL
  sprout list                      List worktrees and their databases
  sprout drop                      Drop this worktree's database only
  sprout remove PATH               Drop database and remove a clean worktree
  sprout agent-install             Add instructions to AGENTS.md

The source URL can also be supplied via SPROUT_SOURCE_URL during init.
The generated .sprout.env is ignored through Git's shared info/exclude.
`

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sprout:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Print(help)
		return nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if args[0] == "init" {
		fs := flag.NewFlagSet("init", flag.ContinueOnError)
		sourceURL := fs.String("source-url", os.Getenv("SPROUT_SOURCE_URL"), "Postgres URL of the source database")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 || *sourceURL == "" {
			return errors.New("usage: sprout init --source-url postgres://USER@HOST/DB")
		}
		if _, err := worktree.Init(cwd, *sourceURL); err != nil {
			return err
		}
		if err := installAgentInstructions(cwd); err != nil {
			return err
		}
		fmt.Println("Initialized sprout and added agent instructions. Run sprout ensure in each worktree.")
		return nil
	}
	if args[0] == "agent-install" {
		if len(args) != 1 {
			return errors.New("usage: sprout agent-install")
		}
		return installAgentInstructions(cwd)
	}
	m, err := worktree.Open(cwd)
	if err != nil {
		return err
	}
	command := args[0]
	commandArgs := args[1:]
	switch command {
	case "seed-drop":
		if len(commandArgs) != 0 {
			return errors.New("usage: sprout seed-drop")
		}
		if err := m.DropSeed(ctx); err != nil {
			return err
		}
		fmt.Println("Shared seed removed. The next new worktree will rebuild it.")
	case "prepare", "refresh":
		if len(commandArgs) != 0 {
			return fmt.Errorf("usage: sprout %s", command)
		}
		if err := m.Prepare(ctx, command == "refresh"); err != nil {
			return err
		}
		fmt.Println("Seed database ready:", m.SeedName())
	case "create":
		if len(commandArgs) < 1 || len(commandArgs) > 2 {
			return errors.New("usage: sprout create BRANCH [PATH]")
		}
		path := ""
		if len(commandArgs) == 2 {
			path = commandArgs[1]
		}
		path, _, err := m.Create(ctx, cwd, commandArgs[0], path)
		if err != nil {
			return err
		}
		fmt.Printf("Created %s\nDatabase connection: %s\n", path, filepath.Join(path, worktree.EnvFile))
	case "ensure":
		if len(commandArgs) != 0 {
			return errors.New("usage: sprout ensure")
		}
		if _, err := m.Ensure(ctx, cwd); err != nil {
			return err
		}
		root, _ := worktree.Root(cwd)
		fmt.Println(filepath.Join(root, worktree.EnvFile))
	case "env", "url":
		if len(commandArgs) != 0 {
			return fmt.Errorf("usage: sprout %s", command)
		}
		url, err := m.Ensure(ctx, cwd)
		if err != nil {
			return err
		}
		if command == "env" {
			fmt.Printf("export DATABASE_URL='%s'\n", strings.ReplaceAll(url, "'", "'\\''"))
		} else {
			fmt.Println(url)
		}
	case "list":
		if len(commandArgs) != 0 {
			return errors.New("usage: sprout list")
		}
		entries, err := m.List(ctx, cwd)
		if err != nil {
			return err
		}
		for _, e := range entries {
			state := "pending"
			if e.Exists {
				state = "ready"
			}
			fmt.Printf("%s\t%s\t%s\t%s\n", state, e.Database, e.Branch, e.Path)
		}
	case "drop":
		if len(commandArgs) != 0 {
			return errors.New("usage: sprout drop")
		}
		if err := m.Remove(ctx, cwd); err != nil {
			return err
		}
		fmt.Println("Dropped this worktree's database.")
	case "remove":
		if len(commandArgs) != 1 {
			return errors.New("usage: sprout remove PATH")
		}
		if err := m.RemoveWorktree(ctx, cwd, commandArgs[0]); err != nil {
			return err
		}
		fmt.Println("Removed worktree and database.")
	default:
		return fmt.Errorf("unknown command %q\n\n%s", command, help)
	}
	return nil
}

const agentStart = "<!-- sprout:begin -->"
const agentEnd = "<!-- sprout:end -->"
const agentInstructions = agentStart + `
## PostgreSQL for feature worktrees

When a feature needs a fresh PostgreSQL database, create its Git worktree with
` + "`sprout create <branch>`" + ` and work in the path it prints. This also creates
an isolated copy of the configured source database. In an existing worktree,
run ` + "`sprout ensure`" + ` before migrations or tests. Read
` + "`.sprout.env`" + ` for ` + "`DATABASE_URL`" + `; do not use the source database for
feature migrations. ` + "`sprout env`" + ` prints a shell export if needed.
Run ` + "`sprout refresh`" + ` after the clean source database changes so new
worktrees start from that updated state; existing worktrees are unchanged.
Use ` + "`sprout list`" + ` to see active worktrees. When a feature is finished
and its worktree is clean, ` + "`sprout remove <path>`" + ` removes that
worktree and its database. Never remove a worktree with uncommitted work.
` + agentEnd + "\n"

func installAgentInstructions(dir string) error {
	root, err := worktree.Root(dir)
	if err != nil {
		return err
	}
	path := filepath.Join(root, "AGENTS.md")
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	content := string(data)
	if strings.Contains(content, agentStart) {
		start := strings.Index(content, agentStart)
		end := strings.Index(content[start:], agentEnd)
		if end < 0 {
			return errors.New("AGENTS.md has an unterminated sprout section")
		}
		end += start + len(agentEnd)
		content = content[:start] + agentInstructions + strings.TrimPrefix(content[end:], "\n")
	} else {
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		if content != "" {
			content += "\n"
		}
		content += agentInstructions
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return err
	}
	// info/exclude is shared by all linked worktrees and keeps credentials local.
	common, err := gitCommon(root)
	if err != nil {
		return err
	}
	exclude := filepath.Join(common, "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(exclude), 0755); err != nil {
		return err
	}
	old, err := os.ReadFile(exclude)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if !strings.Contains("\n"+string(old)+"\n", "\n"+worktree.EnvFile+"\n") {
		text := string(old)
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += worktree.EnvFile + "\n"
		if err := os.WriteFile(exclude, []byte(text), 0644); err != nil {
			return err
		}
	}
	fmt.Println("Agent instructions installed at", path)
	return nil
}

func gitCommon(dir string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--git-common-dir")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(string(out))
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	return filepath.EvalSymlinks(path)
}
