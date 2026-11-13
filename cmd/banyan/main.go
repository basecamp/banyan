// Banyan is a merge queue.
//
//	banyan tick [--dry-run] [--record <dir>]   read the queue, land what passed, build what is next
//
// A pull request joins the queue when it is labelled and leaves when the label goes. Banyan
// runs in a clone of the repository, reads .github/merge-queue.json from it (--config names
// another file), and acts on GitHub as GH_TOKEN, else GITHUB_TOKEN, else gh's login.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/basecamp/banyan/internal/run"
)

func main() {
	if err := command(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func command(args []string) error {
	flags := flag.NewFlagSet("banyan tick", flag.ExitOnError)
	config := flags.String("config", ".github/merge-queue.json", "the queue's config file")
	dry := flags.Bool("dry-run", false, "print what a tick would do, and do none of it")
	record := flags.String("record", "", "a folder each pass of the tick is recorded to")
	if len(args) == 0 || args[0] != "tick" || flags.Parse(args[1:]) != nil || flags.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: banyan tick [--config <file>] [--dry-run] [--record <dir>]")
		os.Exit(2)
	}
	c, err := run.Load(*config)
	if err != nil {
		return fmt.Errorf("%s: %w", *config, err)
	}
	if c.Repo == "" {
		c.Repo = repository(c.Remote)
	}
	q := run.New(c, ".", env("GITHUB_API_URL", "https://api.github.com"), token())
	q.DryRun, q.Record = *dry, *record
	return q.Tick()
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func token() string {
	if t := env("GH_TOKEN", os.Getenv("GITHUB_TOKEN")); t != "" {
		return t
	}
	out, _ := exec.Command("gh", "auth", "token").Output()
	return strings.TrimSpace(string(out))
}

// repository is owner/name, from GITHUB_REPOSITORY or the remote's URL.
func repository(remote string) string {
	if r := os.Getenv("GITHUB_REPOSITORY"); r != "" {
		return r
	}
	out, _ := exec.Command("git", "remote", "get-url", remote).Output()
	if m := regexp.MustCompile(`github\.com[:/]([^/]+/[^/\s]+?)(\.git)?\s*$`).FindStringSubmatch(string(out)); m != nil {
		return m[1]
	}
	return ""
}
