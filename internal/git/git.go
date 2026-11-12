// Package git builds the queue's commits and reads them back.
//
// Every commit is a function of its parents, the head it merges and its note alone, with a
// fixed author and the parents' dates, so the same queue gives the same SHAs on any machine
// and a later tick finds what an earlier one pushed.
package git

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/basecamp/banyan/internal/queue"
)

// Repo is a clone of the repository the queue works in.
type Repo struct {
	Dir    string
	Remote string // origin
	Trunk  string // main
	Lane   string // the branches the queue pushes, <Lane>/<Trunk>/<sha12>
	Name   string // the author and committer of the queue's commits
	Email  string
	// Token, where set, is what git presents to GitHub over https. The machine's own
	// credential helpers are not read, as none of its configuration is.
	Token string
	// Pushed says whether the queue ever pushed a commit for CI, as the forge remembers it.
	Pushed func(queue.SHA) bool

	commits map[string]commit
	made    map[string]queue.SHA
}

type commit struct {
	parents  []string
	tree     string
	date     string
	trailers map[string]string
}

func (r *Repo) git(args ...string) (string, error) { return r.dated("", args...) }

// dated runs git with the queue's identity, and with date as the time of any commit it makes.
func (r *Repo) dated(date string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	// Nothing of the machine's own configuration may change a merge or a commit.
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_ATTR_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME="+r.Name, "GIT_AUTHOR_EMAIL="+r.Email, "GIT_COMMITTER_NAME="+r.Name, "GIT_COMMITTER_EMAIL="+r.Email,
		"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	if r.Token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + r.Token))
		cmd.Env = append(cmd.Env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.https://github.com/.extraheader", "GIT_CONFIG_VALUE_0=AUTHORIZATION: basic "+basic)
	}
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(out.String()), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func (r *Repo) ref(sha queue.SHA) string {
	return fmt.Sprintf("refs/heads/%s/%s/%.12s", r.Lane, r.Trunk, sha)
}

// Fetch brings the trunk, the queue's branches and the heads of the pull requests named, and
// returns the trunk's tip and each head as the remote has it.
func (r *Repo) Fetch(prs []int) (trunk queue.SHA, heads map[int]queue.SHA, err error) {
	specs := []string{"fetch", "--quiet", "--no-tags", "--prune"}
	// A merge base is found in the whole history, which a CI checkout of one commit lacks.
	if shallow, _ := r.git("rev-parse", "--is-shallow-repository"); shallow == "true" {
		specs = append(specs, "--unshallow")
	}
	specs = append(specs, r.Remote, "+refs/heads/"+r.Trunk+":refs/banyan/trunk",
		fmt.Sprintf("+refs/heads/%s/%s/*:refs/banyan/lane/*", r.Lane, r.Trunk))
	for _, n := range prs {
		specs = append(specs, fmt.Sprintf("+refs/pull/%d/head:refs/banyan/pull/%d", n, n))
	}
	if _, err = r.git(specs...); err != nil {
		return "", nil, err
	}
	if trunk, err = r.git("rev-parse", "refs/banyan/trunk"); err != nil {
		return "", nil, err
	}
	heads = map[int]queue.SHA{}
	for _, n := range prs {
		if heads[n], err = r.git("rev-parse", fmt.Sprintf("refs/banyan/pull/%d", n)); err != nil {
			return "", nil, err
		}
	}
	return trunk, heads, nil
}

func (r *Repo) read(sha string) (commit, error) {
	if c, ok := r.commits[sha]; ok {
		return c, nil
	}
	// The parents come last of the three, since a root commit has none and an empty first
	// line would be trimmed away.
	out, err := r.git("show", "--no-patch", "--format=%T%n%cI%n%P%n%B", sha)
	if err != nil {
		return commit{}, err
	}
	lines := strings.Split(out, "\n")
	c := commit{tree: lines[0], date: lines[1], parents: strings.Fields(lines[2]), trailers: map[string]string{}}
	for _, l := range lines[3:] {
		if k, v, ok := strings.Cut(l, ": "); ok && strings.HasPrefix(k, "Banyan-") {
			c.trailers[k] = v
		}
	}
	if r.commits == nil {
		r.commits = map[string]commit{}
	}
	r.commits[sha] = c
	return c, nil
}

// commitTree writes a commit whose date is the later of its parents', so it depends on
// nothing but its arguments.
func (r *Repo) commitTree(tree string, parents []string, message string) (queue.SHA, error) {
	key := tree + " " + strings.Join(parents, " ") + "\n" + message
	if sha, ok := r.made[key]; ok {
		return sha, nil
	}
	date := ""
	args := []string{"-c", "commit.gpgsign=false", "commit-tree", tree, "-m", message}
	for _, p := range parents {
		c, err := r.read(p)
		if err != nil {
			return "", err
		}
		if c.date > date {
			date = c.date
		}
		args = append(args, "-p", p)
	}
	sha, err := r.dated(date, args...)
	if err != nil {
		return "", err
	}
	if r.made == nil {
		r.made = map[string]queue.SHA{}
	}
	r.made[key] = sha
	return sha, nil
}

func message(number int, note string) string {
	s := fmt.Sprintf("Merge #%d\n\nBanyan-Merges: %d", number, number)
	if note != "" {
		s += "\n" + note
	}
	return s
}

// Merge is git's merge of the pull request's head onto parent, read with the attributes
// parent versions and no others. It is false on a conflict.
func (r *Repo) Merge(parent queue.SHA, pr queue.PR, note string) (queue.SHA, bool) {
	tree, err := r.git("-c", "core.attributesFile=/dev/null", "--attr-source="+parent, "merge-tree", "--write-tree", "--no-messages", parent, pr.Head)
	// Only git's own word for a conflict is one: it exits 1 and still names the tree it
	// reached. Any other failure is the queue's, and ends the tick before it can cost a
	// pull request its place.
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 && tree != "" {
		return "", false
	}
	return r.must(r.commitTree(r.must(tree, err), []string{parent, pr.Head}, message(pr.Number, note))), true
}

func (r *Repo) must(out string, err error) string {
	if err != nil {
		panic(err)
	}
	return out
}

// Mark is an empty commit on parent that carries note.
func (r *Repo) Mark(parent queue.SHA, note string) queue.SHA {
	c, err := r.read(parent)
	if err != nil {
		panic(err)
	}
	return r.must(r.commitTree(c.tree, []string{parent}, fmt.Sprintf("Check %.12s\n\n%s", parent, note)))
}

// Seen says whether the queue ever pushed this commit: its branch stands, or the forge
// remembers it.
func (r *Repo) Seen(sha queue.SHA) bool {
	if _, err := r.git("rev-parse", "--verify", "--quiet", fmt.Sprintf("refs/banyan/lane/%.12s", sha)); err == nil {
		return true
	}
	return r.Pushed != nil && r.Pushed(sha)
}

// Builds reads back every commit on the queue's branches as the build it was pushed as. Its
// verdict and the time it was pushed are the forge's to fill in.
func (r *Repo) Builds(trunk queue.SHA) ([]queue.Build, error) {
	out, err := r.git("for-each-ref", "--format=%(objectname) %(refname)", "refs/banyan/lane/")
	if err != nil {
		return nil, err
	}
	var builds []queue.Build
	for _, line := range strings.Split(out, "\n") {
		sha, name, _ := strings.Cut(line, " ")
		if sha == "" || !strings.HasSuffix(name, "/"+sha[:12]) {
			continue // not a branch the queue named
		}
		b, err := r.build(sha, trunk)
		if err != nil {
			return nil, err
		}
		builds = append(builds, b)
	}
	return builds, nil
}

func (r *Repo) build(sha, trunk queue.SHA) (queue.Build, error) {
	top, err := r.read(sha)
	if err != nil {
		return queue.Build{}, err
	}
	b := queue.Build{SHA: sha, Kind: queue.Candidate, Of: top.trailers["Banyan-Of"]}
	b.Round, _ = strconv.Atoi(top.trailers["Banyan-Round"])
	at := sha
	switch {
	case top.trailers["Banyan-Probe-Of"] != "":
		b.Kind, at = queue.Probe, top.parents[0]
	case top.trailers["Banyan-Check"] != "":
		b.Kind, at = queue.Check, top.parents[0]
	case top.trailers["Banyan-Try"] != "":
		// A later try of a merge: the chain knows it by the merge that carries no try.
		n, _ := strconv.Atoi(top.trailers["Banyan-Merges"])
		plain, err := r.commitTree(top.tree, top.parents, message(n, ""))
		if err != nil {
			return b, err
		}
		b.Chain = []queue.Member{{Number: n, Head: top.parents[1], Merge: plain}}
		at = top.parents[0]
	}
	// The base is the first commit under the build that the trunk holds: the tip, or where the
	// tip was when a red candidate was set aside.
	for !r.Holds(trunk, at) {
		c, err := r.read(at)
		if err != nil {
			return b, err
		}
		n, _ := strconv.Atoi(c.trailers["Banyan-Merges"])
		if n == 0 || len(c.parents) != 2 {
			break
		}
		b.Chain = append([]queue.Member{{Number: n, Head: c.parents[1], Merge: at}}, b.Chain...)
		at = c.parents[0]
	}
	b.Base = at
	return b, nil
}

// Holds says whether the trunk at this tip already holds the commit.
func (r *Repo) Holds(trunk, sha queue.SHA) bool {
	_, err := r.git("merge-base", "--is-ancestor", sha, trunk)
	return err == nil
}

// Push sends a build's commit to its branch, which starts its CI.
func (r *Repo) Push(sha queue.SHA) error {
	_, err := r.git("push", "--quiet", r.Remote, sha+":"+r.ref(sha))
	return err
}

// Delete removes a build's branch, if it still points at the build.
func (r *Repo) Delete(sha queue.SHA) error {
	_, err := r.git("push", "--quiet", "--force-with-lease="+r.ref(sha)+":"+sha, r.Remote, ":"+r.ref(sha))
	if err != nil {
		if out, gone := r.git("ls-remote", r.Remote, r.ref(sha)); gone == nil && out == "" {
			return nil
		}
	}
	return err
}

// Land moves the trunk to a candidate by fast-forward and deletes the candidate's branch, in
// one transaction that the remote refuses when the branch no longer points at the candidate.
func (r *Repo) Land(sha queue.SHA) error {
	_, err := r.git("push", "--quiet", "--atomic", "--force-with-lease="+r.ref(sha)+":"+sha, r.Remote,
		sha+":refs/heads/"+r.Trunk, ":"+r.ref(sha))
	return err
}
