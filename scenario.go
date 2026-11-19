// The contest's fixed scenario seed, replicated from the kit's public generator
// (sdk/conformance/wire.ts): FNV-1a + splitmix64 seeding, xorshift64* stream, and
// the user generator. The seed and the name lists are the contest's fixed public
// data, so the logins they produce are the same in every trial (live and replay).
// This is how the forge knows which login the bearer token names: the suite's
// viewer is users[0], the stranger users[2]. No clock, no random, no secrets.

package main

import "strings"

const scenarioSeed uint64 = 0x6a09e667f3bcc909

func fnv1a64(text string) uint64 {
	hash := uint64(0xcbf29ce484222325)
	for i := 0; i < len(text); i++ {
		hash ^= uint64(text[i])
		hash *= 0x100000001b3
	}
	return hash
}

func mix64(value uint64) uint64 {
	z := value + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func scenarioSeedFor(label string) uint64 {
	return mix64(scenarioSeed ^ fnv1a64(label))
}

// Rng is xorshift64*: the scenario's deterministic stream.
type Rng struct {
	state uint64
}

func NewRng(seed uint64) *Rng {
	if seed == 0 {
		seed = scenarioSeed
	}
	return &Rng{state: seed}
}

func (r *Rng) NextU64() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 0x2545f4914f6cdd1d
}

func (r *Rng) NextU32() uint32 {
	return uint32(r.NextU64() >> 32)
}

func (r *Rng) NextInt(maxExclusive int) int {
	if maxExclusive == 1 {
		return 0
	}
	const range32 = uint64(4294967296)
	limit := range32 - (range32 % uint64(maxExclusive))
	for {
		v := r.NextU32()
		if uint64(v) < limit {
			return int(v % uint32(maxExclusive))
		}
	}
}

func (r *Rng) Pick(items []string) string {
	return items[r.NextInt(len(items))]
}

var firstNames = []string{
	"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo", "iris", "jonas", "kira", "lars",
	"mira", "nico", "olga", "pavel", "quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane",
}

var lastNames = []string{
	"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia", "holm", "iversen", "jansen", "keller",
	"lind", "moreau", "novak", "olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich", "vega", "weber",
	"xu", "yilmaz", "zimmer",
}

// scenarioUsers generates the scenario's users, in order.
func scenarioUsers(count int) []string {
	rng := NewRng(scenarioSeedFor("users"))
	used := map[string]bool{}
	logins := make([]string, 0, count)
	for i := 0; i < count; i++ {
		first := rng.Pick(firstNames)
		last := rng.Pick(lastNames)
		base := strings.Map(func(r rune) rune {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				return r
			}
			return -1
		}, first+last)
		login := base
		suffix := 2
		for used[login] {
			login = base + "-" + itoa(suffix)
			suffix++
		}
		used[login] = true
		logins = append(logins, login)
	}
	return logins
}

var users = scenarioUsers(12)

// ViewerLogin is the login the bearer token names (the suite's viewer).
var ViewerLogin = users[0]

// TeammateLogin is the suite's teammate (users[1]).
var TeammateLogin = users[1]

// StrangerLogin is the suite's stranger (users[2]); a repo under the viewer
// must 404 under this owner.
var StrangerLogin = users[2]

// DefaultBranch is the default branch every scenario repository is born with.
const DefaultBranch = "main"

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
