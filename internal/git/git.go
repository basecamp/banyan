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
	Keep   bool // leave a landed candidate's branch for the planner to delete
	// Token, where set, is what git presents to GitHub over https. The machine's own
	// credential helpers are not read, as none of its configuration is.
	Token string
	// Pushed says whether the queue ever pushed a commit for CI, as the forge remembers it.
	Pushed func(queue.SHA) bool

	commits map[string]commit
	made    map[string]queue.SHA
	said    map[merged]string
}

// merged is a pull request at the head a commit of the queue's merged.
type merged struct {
	number int
	head   queue.SHA
}

type commit struct {
	parents  []string
	tree     string
	date     string
	says     string // its message above the last paragraph, which is the queue's own to write
	merges   int    // the pull request it merges, by its last line
	trailer  bool   // an older release wrote the number as a Banyan-Merges trailer instead
	trailers map[string]string
}

func (r *Repo) git(args ...string) (string, error) { return r.run("", "", args...) }

// run runs git with the queue's identity, with date as the time of any commit it makes, and
// with input to read.
func (r *Repo) run(date, input string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Stdin = r.Dir, strings.NewReader(input)
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

// FetchBranch brings a branch from the remote and returns its tip, empty if it is gone.
func (r *Repo) FetchBranch(branch string) (queue.SHA, error) {
	ref := "refs/banyan/head"
	if _, err := r.git("fetch", "--quiet", "--no-tags", r.Remote, "+refs/heads/"+branch+":"+ref); err != nil {
		if out, gone := r.git("ls-remote", r.Remote, "refs/heads/"+branch); gone == nil && out == "" {
			return "", nil
		}
		return "", err
	}
	return r.git("rev-parse", ref)
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
	lines := strings.SplitN(out, "\n", 4)
	c := commit{tree: lines[0], date: lines[1], parents: strings.Fields(lines[2]), trailers: map[string]string{}}
	if len(lines) > 3 {
		c.says = lines[3]
	}
	// Only the last paragraph is read as the queue's, so a description that quotes a trailer
	// or a merge line is never taken for one.
	var own []string
	if at := strings.LastIndex(c.says, "\n\n"); at >= 0 {
		c.says, own = c.says[:at], strings.Split(c.says[at+2:], "\n")
	}
	if len(own) > 0 {
		if rest, ok := strings.CutPrefix(own[len(own)-1], "Merges #"); ok {
			if n, err := strconv.Atoi(rest); err == nil && n > 0 && strconv.Itoa(n) == rest {
				c.merges = n
			}
		}
	}
	for _, l := range own {
		if k, v, ok := strings.Cut(l, ": "); ok && strings.HasPrefix(k, "Banyan-") {
			c.trailers[k] = v
		}
	}
	if n, err := strconv.Atoi(c.trailers["Banyan-Merges"]); err == nil && c.merges == 0 {
		c.merges, c.trailer = n, true
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
	args := []string{"-c", "commit.gpgsign=false", "commit-tree", tree, "-F", "-"}
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
	// The message is read rather than passed, since a description may be longer than one
	// argument may be.
	sha, err := r.run(date, message+"\n", args...)
	if err != nil {
		return "", err
	}
	if r.made == nil {
		r.made = map[string]queue.SHA{}
	}
	r.made[key] = sha
	return sha, nil
}

// says is what a merge says above the queue's own last paragraph. A commit the queue still
// holds that merges this head has the say, so a title or description edited since changes no
// commit the queue names. Else it is the pull request's own words where the queue carries
// them, and GitHub's first line where the branch is known, so a history the queue wrote reads
// like one the merge button wrote.
func (r *Repo) says(pr queue.PR) string {
	if said := r.said[merged{pr.Number, pr.Head}]; said != "" {
		return said
	}
	if pr.Message != "" {
		return pr.Message
	}
	if pr.Branch == "" {
		return fmt.Sprintf("Merge #%d", pr.Number)
	}
	return fmt.Sprintf("Merge pull request #%d from %s", pr.Number, pr.Branch)
}

func message(says string, number int, note string) string {
	s := says + "\n"
	if note != "" {
		s += "\n" + note
	}
	return fmt.Sprintf("%s\nMerges #%d", s, number)
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
	return r.must(r.commitTree(r.must(tree, err), []string{parent, pr.Head}, message(r.says(pr), pr.Number, note))), true
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
	r.said = map[merged]string{}
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
	case top.trailers["Banyan-Try"] != "" && top.merges > 0 && len(top.parents) == 2:
		r.keep(top)
		// A later try of a merge: the chain knows it by the merge that carries no try.
		said := message(top.says, top.merges, "")
		if top.trailer {
			said = fmt.Sprintf("%s\n\nBanyan-Merges: %d", top.says, top.merges)
		}
		plain, err := r.commitTree(top.tree, top.parents, said)
		if err != nil {
			return b, err
		}
		b.Chain = []queue.Member{{Number: top.merges, Head: top.parents[1], Merge: plain}}
		at = top.parents[0]
	}
	// The base is the first commit under the build that the trunk holds: the tip, or where the
	// tip was when a red candidate was set aside.
	for !r.Holds(trunk, at) {
		c, err := r.read(at)
		if err != nil {
			return b, err
		}
		if c.merges == 0 || len(c.parents) != 2 {
			break
		}
		r.keep(c)
		b.Chain = append([]queue.Member{{Number: c.merges, Head: c.parents[1], Merge: at}}, b.Chain...)
		at = c.parents[0]
	}
	b.Base = at
	return b, nil
}

// keep notes what a merge the queue holds says, for the next merge of that head to say.
func (r *Repo) keep(c commit) {
	if of := (merged{c.merges, c.parents[1]}); r.said[of] == "" {
		r.said[of] = c.says
	}
}

// Holds says whether the trunk at this tip already holds the commit.
func (r *Repo) Holds(trunk, sha queue.SHA) bool {
	return r.answers("cat-file", "-e", sha) && r.answers("merge-base", "--is-ancestor", sha, trunk)
}

// answers asks git a question it answers no to by exiting 1.
func (r *Repo) answers(args ...string) bool {
	_, err := r.git(args...)
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
		panic(err)
	}
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
// With Keep the branch stays: it is compared before the push, and the remote checks nothing
// about it, so a removal that deletes the branch in between no longer refuses the landing.
func (r *Repo) Land(sha queue.SHA) error {
	if !r.Keep {
		_, err := r.git("push", "--quiet", "--atomic", "--force-with-lease="+r.ref(sha)+":"+sha, r.Remote,
			sha+":refs/heads/"+r.Trunk, ":"+r.ref(sha))
		return err
	}
	out, err := r.git("ls-remote", r.Remote, r.ref(sha))
	if err != nil {
		return err
	}
	if at := strings.Fields(out); len(at) != 2 || at[0] != sha {
		return fmt.Errorf("%.12s is no longer on its branch", sha)
	}
	_, err = r.git("push", "--quiet", r.Remote, sha+":refs/heads/"+r.Trunk)
	return err
}
