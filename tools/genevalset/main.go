// genevalset deterministically generates the committed routing eval set
// (assets/eval/evalset_v1.jsonl). Synthetic by construction — provenance
// and limits are documented in assets/eval/README.md. Regenerating with
// the same seed reproduces the file byte-for-byte:
//
//	go run ./tools/genevalset > assets/eval/evalset_v1.jsonl
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"

	"github.com/llmrelay/relay/internal/evalx"
)

const (
	version = "v1"
	seed    = 1
)

type tmpl struct {
	domain     string
	category   string
	difficulty [2]float64 // [lo, hi]
	outTokens  [2]int
	prompts    []string
}

var banks = []tmpl{
	{"chitchat", "chitchat", [2]float64{0.05, 0.20}, [2]int{20, 80}, []string{
		"hey! how's it going?",
		"good morning! any fun plans for the weekend?",
		"thanks so much for the help earlier, really appreciate it",
		"lol that's great. anyway what's up",
		"can you say that in a friendlier way? \"Meeting moved to 3pm.\"",
		"wish me luck, big presentation today!",
		"what's a good icebreaker for a team meeting?",
		"my cat just knocked my coffee over again. cats, right?",
	}},
	{"qa", "simple-qa", [2]float64{0.12, 0.32}, [2]int{30, 120}, []string{
		"What is the capital of Australia?",
		"How many milliliters are in a US cup?",
		"Who wrote 'One Hundred Years of Solitude'?",
		"What year did the Berlin Wall fall?",
		"What's the difference between a latte and a flat white?",
		"What does HTTP 429 mean?",
		"How long does it take sunlight to reach Earth?",
		"What's the boiling point of water at sea level in Fahrenheit?",
	}},
	{"extraction", "extraction", [2]float64{0.20, 0.42}, [2]int{60, 160}, []string{
		"Extract every email address from this text and return them as a JSON array: \"Contact ana.reyes@example.com or sales@acme.io; for escalations use cto@acme.io.\"",
		"Return JSON {\"name\":..., \"date\":..., \"amount\":...} from: \"Invoice INV-2214 issued to Marta Kim on 2026-03-14 for $1,180.50.\"",
		"List the ticker symbols mentioned: \"AAPL dipped while MSFT and NVDA rallied; TSM was flat.\" Reply as a comma-separated list.",
		"From this log line, extract the status code and path as JSON: '10.0.0.7 - - [18/Jul/2026] \"GET /v1/models HTTP/1.1\" 200 512'",
		"Pull out all dates in ISO format from: \"We met March 5th, 2026, followed up on 03/09/26, and closed on the twelfth of March 2026.\"",
		"Extract the action items from this standup note as a JSON list: \"Sam to fix the flaky test; Ana reviews the RFC by Friday; deploy freeze starts Monday.\"",
	}},
	{"summarize", "summarize", [2]float64{0.30, 0.55}, [2]int{80, 220}, []string{
		"Summarize in two sentences: \"The city council voted 7-2 to extend the pilot program converting two downtown blocks to pedestrian-only zones through December. Local businesses reported mixed effects: cafes saw foot traffic rise roughly 20 percent, while several service businesses dependent on curbside parking petitioned for exemptions. A final evaluation, including delivery-access data, is due in January.\"",
		"Give me a one-paragraph summary of this changelog: \"v2.3 adds streaming responses, per-key rate limits, and a new audit log. Breaking: the /v1/query endpoint now requires an explicit version header. Deprecated: XML output, to be removed in v3. Numerous fixes to pagination and timezone handling.\"",
		"Condense this email to three bullets: \"Team — following yesterday's incident review, we're making three changes. First, all deploys now require a second approver during business hours. Second, the rollback runbook moves into the repo so it's versioned with the code. Third, we're adding a synthetic canary that exercises checkout every five minutes. None of this blocks the Q3 roadmap, but the canary work will borrow Dana for a sprint.\"",
		"TL;DR this abstract: \"We study the effect of retrieval augmentation on factual accuracy in small language models. Across three QA benchmarks, retrieval reduces hallucination rates by 18-31 percent, with the largest gains on questions about entities in the long tail of the training distribution. Gains diminish as model scale increases, suggesting retrieval partially substitutes for parametric knowledge.\"",
	}},
	{"code", "code-write", [2]float64{0.45, 0.72}, [2]int{150, 500}, []string{
		"Write a Python function that merges overlapping intervals. Input: list of [start, end] pairs. Include type hints and a couple of doctests.",
		"Write a Go function that reads a CSV file and returns the rows grouped by the value of the first column, as a map[string][][]string. Handle the header row.",
		"Implement debounce in TypeScript with correct typing for the wrapped function's arguments and return type.",
		"Write a SQL query that finds the top 3 products by revenue per region, given tables orders(product_id, region, amount) and products(id, name). Explain the window function you use.",
		"Write a bash script that watches a directory and prints any file that hasn't been modified in the last 24 hours. It should handle spaces in filenames.",
	}},
	{"code", "code-debug", [2]float64{0.55, 0.80}, [2]int{150, 450}, []string{
		"This Python raises 'RuntimeError: dictionary changed size during iteration'. Fix it and explain why:\n```python\nfor k in cache:\n    if expired(cache[k]):\n        del cache[k]\n```",
		"Why does this Go code deadlock?\n```go\nch := make(chan int)\nch <- 1\nfmt.Println(<-ch)\n```",
		"This JS sometimes logs stale state. Explain and fix:\n```js\nconst [n, setN] = useState(0);\nfunction onClick() { setN(n + 1); setN(n + 1); }\n```",
		"My SQL returns duplicate rows after I added a JOIN to tags. Schema: posts(id), tags(post_id, tag). Query: SELECT posts.* FROM posts JOIN tags ON tags.post_id = posts.id WHERE tags.tag IN ('a','b'). Fix it.",
	}},
	{"math", "math-word", [2]float64{0.60, 0.88}, [2]int{150, 400}, []string{
		"A tank fills at 12 L/min and drains at 7 L/min through a valve that opens only when the tank is above 40 L. Starting empty, how long until it holds 100 L? Show your work.",
		"Two trains 210 km apart head toward each other at 60 km/h and 80 km/h. A bird flies between them at 100 km/h until they meet. How far does the bird fly? Explain the shortcut.",
		"I invest $2,000 at 4.5% annual interest compounded monthly. How many whole months until the balance first exceeds $2,500? Show the calculation.",
		"A bag has 5 red, 4 blue, 3 green marbles. Draw three without replacement: what's the probability of one of each color? Give an exact fraction.",
	}},
	{"reasoning", "multi-step", [2]float64{0.68, 0.95}, [2]int{200, 600}, []string{
		"Alice, Bob, Carol, and Dan each ordered a different drink (tea, coffee, juice, water). Alice didn't order tea or coffee. Bob sits next to the person with juice. Carol ordered water only if Dan ordered tea. Dan didn't order juice. Bob ordered coffee. Who ordered what? Walk through the deductions.",
		"Design a rate limiter for a multi-tenant API where tenants have different quotas, bursts must be tolerated briefly, and the service runs on 6 stateless nodes. Compare at least two approaches and recommend one with justification.",
		"You have 8 identical-looking balls; one is heavier. Using a balance scale only twice, how do you find the heavy ball? Then generalize: how many weighings for n balls?",
		"Our checkout conversion dropped 14% the week we launched a redesign, but the drop is only on mobile Safari, and only for logged-out users. Lay out a systematic debugging plan, ordered by expected information gain.",
	}},
	{"math", "short-hard", [2]float64{0.72, 0.95}, [2]int{200, 500}, []string{
		"Prove that the square root of 2 is irrational.",
		"Prove there are infinitely many primes.",
		"Show that 0.999... equals 1, rigorously.",
		"Why does the halting problem have no general solution? Sketch the diagonalization.",
	}},
	{"chitchat", "long-easy", [2]float64{0.08, 0.22}, [2]int{20, 60}, []string{
		"I'm going to paste my notes so I don't lose them, just reply 'saved'. Notes: Monday - dentist 9am, pick up dry cleaning, call plumber about the kitchen tap, the part number is KX-2231. Tuesday - standup moved to 10, lunch with Priya at the noodle place on 5th, remember she's vegetarian now. Wednesday - gym, then review Marco's draft, he wants comments by Thursday morning at the latest. Also random: the wifi password at the coworking space is sunflower2026, parking validation is at the front desk, and the good coffee is on floor 3.",
		"Just venting, no advice needed, okay? Today started with the train being 25 minutes late, then the elevator at work was out so I hauled my bike bag up six floors, then the meeting that could have been an email ran 90 minutes and we still didn't decide anything, then someone microwaved fish in the small kitchen, and to top it off it started raining exactly when I left. Anyway. Rant over. You can just say 'that sounds rough'.",
	}},
}

// quality maps (band, difficulty, domain) to a synthetic quality label.
// The model: all bands share one base and diverge with difficulty — band
// gaps vanish as difficulty approaches zero (on trivial traffic every
// model is fine, which is the premise of routing at all), and cheap
// models fall off steepest, more so on code/math/reasoning. Assumptions
// and their limits are documented in assets/eval/README.md; the gate
// verdict is only as good as this curve until relay train / --live
// replaces it with measurements on real traffic.
func quality(band evalx.Band, difficulty float64, domain string, jitter float64) float64 {
	var q float64
	switch band {
	case evalx.BandFrontier:
		q = 0.96 - 0.10*difficulty
	case evalx.BandMid:
		q = 0.96 - 0.30*math.Pow(difficulty, 1.3)
	case evalx.BandCheap:
		q = 0.96 - 0.55*math.Pow(difficulty, 1.5)
	}
	if band != evalx.BandFrontier && (domain == "code" || domain == "math" || domain == "reasoning") && difficulty > 0.5 {
		q -= 0.04
	}
	q += jitter
	return math.Round(math.Max(0.05, math.Min(0.99, q))*1000) / 1000
}

func main() {
	rng := rand.New(rand.NewSource(seed))
	out := json.NewEncoder(os.Stdout)
	fmt.Printf("{\"version\":%q,\"seed\":%d}\n", version, seed)
	n := 0
	for _, bank := range banks {
		for i, prompt := range bank.prompts {
			d := bank.difficulty[0] + rng.Float64()*(bank.difficulty[1]-bank.difficulty[0])
			d = math.Round(d*1000) / 1000
			outTok := bank.outTokens[0] + rng.Intn(bank.outTokens[1]-bank.outTokens[0]+1)
			row := evalx.Row{
				ID:         fmt.Sprintf("%s-%02d", bank.category, i+1),
				Prompt:     prompt,
				Domain:     bank.domain,
				Category:   bank.category,
				Difficulty: d,
				OutTokens:  outTok,
				Quality: map[evalx.Band]float64{
					evalx.BandCheap:    quality(evalx.BandCheap, d, bank.domain, rng.Float64()*0.04-0.02),
					evalx.BandMid:      quality(evalx.BandMid, d, bank.domain, rng.Float64()*0.04-0.02),
					evalx.BandFrontier: quality(evalx.BandFrontier, d, bank.domain, rng.Float64()*0.04-0.02),
				},
			}
			if err := out.Encode(row); err != nil {
				panic(err)
			}
			n++
		}
	}
	fmt.Fprintf(os.Stderr, "generated %d rows (version %s, seed %d)\n", n, version, seed)
}
