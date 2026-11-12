// Banyan is a merge queue.
//
//	banyan add <pr> [--first]        queue a pull request, or say in one line why not
//	banyan remove <pr> <who> <why>   take one out of the queue, saying who and why on it
//	banyan status [<pr>] [--json]    the queue, or one pull request's place or outcome
//	banyan tick [--dry-run]          read the queue, land what passed, build what is next
//	banyan replay <recording>        plan a recorded tick again, and print its actions
//
// It runs in a clone of the repository, reads .github/merge-queue.json from it (--config
// names another file), and acts on GitHub as GH_TOKEN, else GITHUB_TOKEN, else gh's login.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/basecamp/banyan/internal/queue"
	"github.com/basecamp/banyan/internal/run"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: banyan add <pr> [--first] | remove <pr> <who> <why> | status [<pr>] [--json] | tick [--dry-run] [--record <dir>] | replay <recording>")
	os.Exit(2)
}

func main() {
	if err := command(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// flag takes a boolean flag out of the arguments, and option one with a value.
func flag(args *[]string, name string) bool {
	i := slices.Index(*args, name)
	if i >= 0 {
		*args = slices.Delete(*args, i, i+1)
	}
	return i >= 0
}

func option(args *[]string, name, def string) string {
	if i := slices.Index(*args, name); i >= 0 && i+1 < len(*args) {
		def = (*args)[i+1]
		*args = slices.Delete(*args, i, i+2)
	}
	return def
}

func command(args []string) error {
	if len(args) == 0 {
		usage()
	}
	verb, args := args[0], args[1:]
	if verb == "replay" {
		return replay(args)
	}
	config := option(&args, "--config", ".github/merge-queue.json")
	c, err := run.Load(config)
	if err != nil {
		return fmt.Errorf("%s: %w", config, err)
	}
	if c.Repo == "" {
		c.Repo = repository(c.Remote)
	}
	q := run.New(c, ".", env("GITHUB_API_URL", "https://api.github.com"), token())
	q.DryRun, q.Record = flag(&args, "--dry-run"), option(&args, "--record", "")
	asJSON, first := flag(&args, "--json"), flag(&args, "--first")
	number := 0
	if len(args) > 0 {
		if number, err = strconv.Atoi(strings.TrimPrefix(args[0], "#")); err != nil {
			usage()
		}
	}

	switch {
	case verb == "tick" && len(args) == 0:
		return q.Tick()
	case verb == "add" && len(args) == 1:
		said, err := q.Add(number, first)
		if err == nil {
			fmt.Println(said)
		}
		return err
	case verb == "remove" && len(args) == 3 && args[1] != "" && args[2] != "":
		return q.Remove(number, args[1], args[2])
	case verb == "status" && len(args) <= 1:
		entries, err := q.Entries(number)
		if err != nil {
			return err
		}
		if asJSON {
			data, _ := json.MarshalIndent(entries, "", "  ")
			fmt.Println(string(data))
			return nil
		}
		for _, e := range entries {
			fmt.Printf("#%d\t%s\t%.12s\t%s\t%s\n", e.Number, e.State, e.Head, e.Note, e.URL)
		}
		return nil
	}
	usage()
	return nil
}

func replay(args []string) error {
	if len(args) != 1 {
		usage()
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	var rec queue.Recording
	if err := json.Unmarshal(data, &rec); err != nil {
		return err
	}
	for _, a := range rec.Replay() {
		line, _ := json.Marshal(a)
		fmt.Println(string(line))
	}
	return nil
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
