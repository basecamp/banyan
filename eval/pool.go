package main

import (
	"math"
	"math/rand"
	"slices"
	"time"
)

const never = time.Duration(math.MaxInt64)

// pool is a CI's agents and the builds waiting for one, in the order they came. The other
// builds arrive by its own dice, so two pools of one seed see the same ones whatever the
// queue does.
type pool struct {
	shape   Shape
	rng     *rand.Rand
	running []*job
	waiting []*job
	next    time.Duration // when the next of the other builds come

	Waited []float64 // how long each of the other builds waited for an agent, in minutes
	Busy   []int     // the agents busy at each look
	Came   []int     // the other builds that came in each hour so far
}

// job is a build in the pool: the queue's, or one of the others when build is nil.
type job struct {
	build      *simBuild
	came, ends time.Duration
	hold       time.Duration
}

func newPool(shape Shape, seed int64) *pool {
	p := &pool{shape: shape, rng: rand.New(rand.NewSource(seed + 1<<32))}
	p.draw(0)
	return p
}

// draw sets when the next of the other builds come after this time.
func (p *pool) draw(after time.Duration) {
	if p.next = never; p.shape.Others > 0 {
		p.next = after + time.Duration(p.rng.ExpFloat64()/p.shape.Others*max(p.shape.Burst, 1)*float64(time.Hour))
	}
}

// advance plays the pool up to a time: builds end, the queue's builds and the other builds
// come, and each free agent goes to the build that came first.
func (p *pool) advance(to time.Duration) {
	for {
		first := -1
		for i, j := range p.running {
			if first < 0 || j.ends < p.running[first].ends {
				first = i
			}
		}
		at, ends, came := p.next, first >= 0 && p.running[first].ends <= p.next, false
		if ends {
			at = p.running[first].ends
		}
		if len(p.waiting) > 0 && len(p.running) < p.shape.Agents && p.waiting[0].came < at {
			at, ends, came = p.waiting[0].came, false, true
		}
		switch {
		case at > to:
			p.Busy = append(p.Busy, len(p.running))
			return
		case ends:
			p.running = slices.Delete(p.running, first, first+1)
		case !came:
			for n := 0; n == 0 || p.rng.Float64() < 1-1/max(p.shape.Burst, 1); n++ {
				for len(p.Came) <= int(p.next/time.Hour) {
					p.Came = append(p.Came, 0)
				}
				p.Came[len(p.Came)-1]++
				p.line(&job{came: p.next, hold: p.shape.Hold})
			}
			p.draw(p.next)
		}
		p.start(at)
	}
}

// line puts a build in the line for an agent, behind every build that came before it.
func (p *pool) line(j *job) {
	i := len(p.waiting)
	for i > 0 && p.waiting[i-1].came > j.came {
		i--
	}
	p.waiting = slices.Insert(p.waiting, i, j)
}

func (p *pool) start(at time.Duration) {
	for len(p.waiting) > 0 && p.waiting[0].came <= at && len(p.running) < p.shape.Agents {
		j := p.waiting[0]
		p.waiting = p.waiting[1:]
		j.ends = at + j.hold
		if j.build == nil {
			p.Waited = append(p.Waited, (at - j.came).Minutes())
		} else {
			j.build.start, j.build.end = at, j.ends
		}
		p.running = append(p.running, j)
	}
}

// add puts one of the queue's builds in the line from the moment the CI takes it up.
func (p *pool) add(b *simBuild, hold, at time.Duration) {
	p.line(&job{build: b, came: at, hold: hold})
}

// cancel stops one of the queue's builds, wherever it is, and frees its agent.
func (p *pool) cancel(b *simBuild, at time.Duration) {
	its := func(j *job) bool { return j.build == b }
	p.waiting, p.running = slices.DeleteFunc(p.waiting, its), slices.DeleteFunc(p.running, its)
	p.start(at)
}
