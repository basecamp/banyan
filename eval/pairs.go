package main

import (
	"errors"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"

	"github.com/basecamp/banyan/internal/queue"
)

// clashingHeads asks git, pair by pair, which of the heads beside each held head it cannot merge
// with. A held head is answered where its commit is in the clone, every one of its pairs has an
// answer, and at least one of them clashes; only answered heads' pairs are kept, under both heads.
// Git reads here with none of the machine's own configuration, as the queue's merges do, with no
// lazy fetch, and writes to a scratch object store, never to the clone.
func clashingHeads(clone string, beside map[queue.SHA]map[queue.SHA]bool) (clashing map[queue.SHA]map[queue.SHA]bool, answered map[queue.SHA]bool, err error) {
	clashing, answered = map[queue.SHA]map[queue.SHA]bool{}, map[queue.SHA]bool{}
	if len(beside) == 0 {
		return clashing, answered, nil
	}
	objects, err := gitRead(clone, nil, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return nil, nil, err
	}
	scratch, err := os.MkdirTemp("", "eval-merge-tree-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(scratch)
	env := append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_ATTR_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_NO_LAZY_FETCH=1", "GIT_OBJECT_DIRECTORY="+scratch, "GIT_ALTERNATE_OBJECT_DIRECTORIES="+strings.TrimSpace(string(objects)))
	read := map[queue.SHA]bool{}
	readable := func(sha queue.SHA) bool {
		if _, ok := read[sha]; !ok {
			cmd := exec.Command("git", "--no-optional-locks", "cat-file", "-e", string(sha)+"^{commit}")
			cmd.Dir, cmd.Env = clone, env
			read[sha] = cmd.Run() == nil
		}
		return read[sha]
	}
	asked := map[[2]queue.SHA]*bool{}
	for _, held := range slices.Sorted(maps.Keys(beside)) {
		if !readable(held) {
			continue
		}
		var clashes []queue.SHA
		known := true
		for _, other := range slices.Sorted(maps.Keys(beside[held])) {
			pair := [2]queue.SHA{min(held, other), max(held, other)}
			if _, ok := asked[pair]; !ok && readable(other) {
				asked[pair] = mergeTree(clone, env, pair[0], pair[1])
			}
			switch answer := asked[pair]; {
			case answer == nil:
				known = false
			case *answer:
				clashes = append(clashes, other)
			}
		}
		if !known || len(clashes) == 0 {
			continue
		}
		answered[held] = true
		for _, other := range clashes {
			for _, ends := range [][2]queue.SHA{{held, other}, {other, held}} {
				if clashing[ends[0]] == nil {
					clashing[ends[0]] = map[queue.SHA]bool{}
				}
				clashing[ends[0]][ends[1]] = true
			}
		}
	}
	return clashing, answered, nil
}

// mergeTree says whether git finds a conflict merging the two commits, or nil where it gives no
// answer. It reads the attributes a versions and no others, as the queue's own merge reads its
// parent's. Git also exits 1 on a commit it cannot read, so an answer is one that names the
// merged tree.
func mergeTree(clone string, env []string, a, b queue.SHA) *bool {
	cmd := exec.Command("git", "--no-optional-locks", "-c", "core.attributesFile=/dev/null", "--attr-source="+string(a), "merge-tree", "--write-tree", "--no-messages", string(a)+"^{commit}", string(b)+"^{commit}")
	cmd.Dir, cmd.Env = clone, env
	out, err := cmd.Output()
	tree, _, _ := strings.Cut(string(out), "\n")
	if !objectID.MatchString(tree) {
		return nil
	}
	var exit *exec.ExitError
	switch {
	case err == nil:
		return new(false)
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return new(true)
	}
	return nil
}

var objectID = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func pairsIn(clashing map[queue.SHA]map[queue.SHA]bool) int {
	n := 0
	for _, others := range clashing {
		n += len(others)
	}
	return n / 2
}
