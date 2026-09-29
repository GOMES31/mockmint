package template

import (
	"fmt"
	"math/rand/v2"
	"strings"
)

// Fake produces plausible fake data from the request RNG, exposed to
// templates as .Fake (e.g. {{ .Fake.Email }}). Word lists are small and
// built in to avoid a large faker dependency.
type Fake struct {
	rng *rand.Rand
}

var (
	firstNames = []string{"Ada", "Alan", "Grace", "Linus", "Margaret", "Dennis", "Barbara", "Ken", "Frances", "Edsger", "Radia", "John", "Hedy", "Tim", "Katherine", "Donald"}
	lastNames  = []string{"Lovelace", "Turing", "Hopper", "Torvalds", "Hamilton", "Ritchie", "Liskov", "Thompson", "Allen", "Dijkstra", "Perlman", "McCarthy", "Lamarr", "Berners-Lee", "Johnson", "Knuth"}
	cities     = []string{"Lisbon", "Porto", "Berlin", "Paris", "Madrid", "Oslo", "Tokyo", "Toronto", "Austin", "Sydney", "Nairobi", "Santiago"}
	countries  = []string{"Portugal", "Germany", "France", "Spain", "Norway", "Japan", "Canada", "United States", "Australia", "Kenya", "Chile", "Brazil"}
	companies  = []string{"Acme", "Globex", "Initech", "Umbrella", "Hooli", "Stark", "Wayne", "Tyrell", "Cyberdyne", "Soylent"}
	suffixes   = []string{"Inc", "Ltd", "GmbH", "SA", "LLC"}
	streets    = []string{"Main St", "High St", "Oak Ave", "Maple Rd", "Elm St", "Park Ln", "Cedar Blvd", "River Rd"}
	domains    = []string{"example.com", "example.org", "example.net"}
	words      = []string{"alpha", "bravo", "cobalt", "delta", "ember", "fjord", "garnet", "harbor", "indigo", "juniper", "kelp", "lumen", "mosaic", "nimbus", "orbit", "prism", "quartz", "raven", "sierra", "tundra"}
)

func (f Fake) pick(list []string) string { return list[f.rng.IntN(len(list))] }

// FirstName returns a first name.
func (f Fake) FirstName() string { return f.pick(firstNames) }

// LastName returns a last name.
func (f Fake) LastName() string { return f.pick(lastNames) }

// Name returns "First Last".
func (f Fake) Name() string { return f.FirstName() + " " + f.LastName() }

// Email returns an address on a reserved example domain.
func (f Fake) Email() string {
	local := strings.ToLower(f.FirstName() + "." + strings.ReplaceAll(f.LastName(), "-", ""))
	return fmt.Sprintf("%s%d@%s", local, f.rng.IntN(100), f.pick(domains))
}

// Username returns a lowercase handle.
func (f Fake) Username() string {
	return fmt.Sprintf("%s_%s%d", strings.ToLower(f.FirstName()), f.pick(words), f.rng.IntN(1000))
}

// City returns a city name.
func (f Fake) City() string { return f.pick(cities) }

// Country returns a country name.
func (f Fake) Country() string { return f.pick(countries) }

// Company returns a company name.
func (f Fake) Company() string { return f.pick(companies) + " " + f.pick(suffixes) }

// Street returns a street address.
func (f Fake) Street() string { return fmt.Sprintf("%d %s", 1+f.rng.IntN(999), f.pick(streets)) }

// Zip returns a five-digit postal code.
func (f Fake) Zip() string { return fmt.Sprintf("%05d", f.rng.IntN(100000)) }

// Phone returns a phone number in the reserved +1-555-01xx range.
func (f Fake) Phone() string { return fmt.Sprintf("+1-555-01%02d", f.rng.IntN(100)) }

// Word returns a single word.
func (f Fake) Word() string { return f.pick(words) }

// Sentence returns n words (default 6) as a capitalized sentence.
func (f Fake) Sentence(n ...int) string {
	count := 6
	if len(n) > 0 && n[0] > 0 {
		count = min(n[0], 64)
	}
	ws := make([]string, count)
	for i := range ws {
		ws[i] = f.pick(words)
	}
	s := strings.Join(ws, " ")
	return strings.ToUpper(s[:1]) + s[1:] + "."
}
