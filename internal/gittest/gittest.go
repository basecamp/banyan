// Package gittest makes a repository for tests: a bare origin with a trunk, pull requests
// that each write files on a branch off it, and a clone for the queue to work in.
package gittest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Origin is the bare repository, the author's clone and the queue's clone.
type Origin struct {
	T                 *testing.T
	Bare, Work, Clone string
	Heads             map[int]string
}

// Sh runs a command in dir with a fixed git identity and date, and fails the test if it fails.
func Sh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := Try(dir, args...)
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// Try runs a command as Sh does and returns its failure.
func Try(dir string, args ...string) (string, error) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=A", "GIT_AUTHOR_EMAIL=a@example.com", "GIT_COMMITTER_NAME=A", "GIT_COMMITTER_EMAIL=a@example.com",
		"GIT_AUTHOR_DATE=2026-01-02T03:04:05Z", "GIT_COMMITTER_DATE=2026-01-02T03:04:05Z")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func New(t *testing.T) *Origin {
	dir := t.TempDir()
	o := &Origin{T: t, Bare: filepath.Join(dir, "origin.git"), Work: filepath.Join(dir, "author"), Clone: filepath.Join(dir, "queue"), Heads: map[int]string{}}
	Sh(t, dir, "git", "init", "--quiet", "--bare", "--initial-branch=main", o.Bare)
	Sh(t, dir, "git", "clone", "--quiet", o.Bare, o.Work)
	o.commit("README", "trunk\n", "trunk")
	Sh(t, o.Work, "git", "push", "--quiet", "origin", "HEAD:main")
	Sh(t, dir, "git", "clone", "--quiet", o.Bare, o.Clone)
	return o
}

func (o *Origin) commit(file, content, message string) string {
	if err := os.WriteFile(filepath.Join(o.Work, file), []byte(content), 0o644); err != nil {
		o.T.Fatal(err)
	}
	Sh(o.T, o.Work, "git", "add", ".")
	Sh(o.T, o.Work, "git", "commit", "--quiet", "-m", message)
	return Sh(o.T, o.Work, "git", "rev-parse", "HEAD")
}

// PR opens pull request n off the trunk's tip, writing content to file, and returns its head.
func (o *Origin) PR(n int, file, content string) string {
	Sh(o.T, o.Work, "git", "fetch", "--quiet", "origin")
	Sh(o.T, o.Work, "git", "checkout", "--quiet", "-B", fmt.Sprintf("pr%d", n), "origin/main")
	return o.push(n, file, content)
}

// Amend pushes one more commit to pull request n.
func (o *Origin) Amend(n int, file, content string) string {
	Sh(o.T, o.Work, "git", "checkout", "--quiet", fmt.Sprintf("pr%d", n))
	return o.push(n, file, content)
}

func (o *Origin) push(n int, file, content string) string {
	head := o.commit(file, content, fmt.Sprintf("pr %d writes %s", n, file))
	Sh(o.T, o.Work, "git", "push", "--quiet", "--force", "origin", fmt.Sprintf("HEAD:refs/pull/%d/head", n))
	o.Heads[n] = head
	return head
}

// Outside commits straight to the trunk, as a merge that did not come through the queue.
func (o *Origin) Outside(file, content string) string {
	Sh(o.T, o.Work, "git", "fetch", "--quiet", "origin")
	Sh(o.T, o.Work, "git", "checkout", "--quiet", "-B", "main", "origin/main")
	sha := o.commit(file, content, "outside")
	Sh(o.T, o.Work, "git", "push", "--quiet", "origin", "HEAD:main")
	return sha
}

// Trunk is the trunk's tip in the origin.
func (o *Origin) Trunk() string { return Sh(o.T, o.Bare, "git", "rev-parse", "refs/heads/main") }

// Holds says whether the commit's tree in the origin has the file.
func (o *Origin) Holds(sha, file string) bool {
	_, err := Try(o.Bare, "git", "cat-file", "-e", sha+":"+file)
	return err == nil
}

// Branches is the origin's branches under prefix, by name, with the commit each points at.
func (o *Origin) Branches(prefix string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(Sh(o.T, o.Bare, "git", "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/"+prefix), "\n") {
		if name, sha, ok := strings.Cut(line, " "); ok {
			out[name] = sha
		}
	}
	return out
}

// Landed says whether the commit is on the trunk.
func (o *Origin) Landed(sha string) bool {
	_, err := Try(o.Bare, "git", "merge-base", "--is-ancestor", sha, "refs/heads/main")
	return err == nil
}
