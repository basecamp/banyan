package queue

import "fmt"

// Recording is a world and every answer its git gave the planner, which is all a tick read:
// replayed, it gives the same actions on any machine.
type Recording struct {
	World   World
	Config  Config
	Commits map[string]Answer
	Actions []Action
}

// Answer is what the git said of one commit the planner asked for.
type Answer struct {
	SHA  SHA
	OK   bool
	Seen bool
}

type recorder struct {
	repo Repo
	rec  *Recording
}

// Record plans a tick as Plan does, and returns it with everything it read.
func Record(w World, c Config) *Recording {
	rec := &Recording{World: w, Config: c, Commits: map[string]Answer{}}
	w.Repo = recorder{w.Repo, rec}
	rec.Actions = Plan(w, c)
	return rec
}

func (r recorder) Merge(parent SHA, pr PR, note string) (SHA, bool) {
	sha, ok := r.repo.Merge(parent, pr, note)
	r.rec.Commits[fmt.Sprintf("merge %s %d %s %q", parent, pr.Number, pr.Head, note)] = Answer{SHA: sha, OK: ok}
	return sha, ok
}

func (r recorder) Mark(parent SHA, note string) SHA {
	sha := r.repo.Mark(parent, note)
	r.rec.Commits[fmt.Sprintf("mark %s %q", parent, note)] = Answer{SHA: sha, OK: true}
	return sha
}

func (r recorder) Seen(sha SHA) bool {
	seen := r.repo.Seen(sha)
	r.rec.Commits["seen "+sha] = Answer{Seen: seen}
	return seen
}

// Replay plans the recorded tick again from the recording alone.
func (rec *Recording) Replay() []Action {
	w := rec.World
	w.Repo = replayer{rec}
	return Plan(w, rec.Config)
}

type replayer struct{ rec *Recording }

func (r replayer) answer(key string) Answer {
	a, ok := r.rec.Commits[key]
	if !ok {
		panic("the planner asked what the recording does not hold: " + key)
	}
	return a
}

func (r replayer) Merge(parent SHA, pr PR, note string) (SHA, bool) {
	a := r.answer(fmt.Sprintf("merge %s %d %s %q", parent, pr.Number, pr.Head, note))
	return a.SHA, a.OK
}

func (r replayer) Mark(parent SHA, note string) SHA {
	return r.answer(fmt.Sprintf("mark %s %q", parent, note)).SHA
}

func (r replayer) Seen(sha SHA) bool { return r.answer("seen " + sha).Seen }
