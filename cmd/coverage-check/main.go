// coverage-check enforces coverage thresholds on a Go coverage profile.
//
// Reports total coverage and, optionally, patch coverage against a git
// base ref. Counts partial lines (multiple blocks on one line, only
// some hit) as misses — matches codecov's per-line metric so the CI
// number stays a lower bound on what codecov displays.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"golang.org/x/tools/cover"
)

const modulePrefix = "github.com/cynkra/blockyard/"

type lineStatus int

const (
	miss lineStatus = iota
	partial
	hit
)

type fileCov map[int]lineStatus

func buildCoverage(profiles []*cover.Profile) map[string]fileCov {
	out := map[string]fileCov{}
	for _, p := range profiles {
		path := strings.TrimPrefix(p.FileName, modulePrefix)
		counts := map[int][]int{}
		for _, b := range p.Blocks {
			for l := b.StartLine; l <= b.EndLine; l++ {
				counts[l] = append(counts[l], b.Count)
			}
		}
		status := make(fileCov, len(counts))
		for l, cs := range counts {
			h := 0
			for _, c := range cs {
				if c > 0 {
					h++
				}
			}
			switch {
			case h == len(cs):
				status[l] = hit
			case h == 0:
				status[l] = miss
			default:
				status[l] = partial
			}
		}
		out[path] = status
	}
	return out
}

func matchIgnore(path, pattern string) bool {
	if strings.HasSuffix(pattern, "/**") {
		return strings.HasPrefix(path, strings.TrimSuffix(pattern, "/**")+"/")
	}
	return path == pattern
}

func applyIgnores(cov map[string]fileCov, ignores []string) {
	for path := range cov {
		for _, p := range ignores {
			if matchIgnore(path, p) {
				delete(cov, path)
				break
			}
		}
	}
}

type totals struct{ hit, miss, partial int }

func (t totals) pct() float64 {
	den := t.hit + t.miss + t.partial
	if den == 0 {
		return 100
	}
	return 100 * float64(t.hit) / float64(den)
}

func sumTotals(cov map[string]fileCov) totals {
	var t totals
	for _, f := range cov {
		for _, s := range f {
			switch s {
			case hit:
				t.hit++
			case miss:
				t.miss++
			case partial:
				t.partial++
			}
		}
	}
	return t
}

var hunkRE = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// gitAddedLines returns, per file, the line numbers added on HEAD
// relative to base. Only .go files are included; *_test.go is
// excluded.
func gitAddedLines(base string) (map[string]map[int]bool, error) {
	cmd := exec.Command("git", "diff", "-U0", "--no-color", base+"...HEAD")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff %s...HEAD: %w", base, err)
	}
	added := map[string]map[int]bool{}
	var curFile string
	var curLine int
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "+++ b/"):
			f := strings.TrimPrefix(line, "+++ b/")
			if strings.HasSuffix(f, ".go") && !strings.HasSuffix(f, "_test.go") {
				curFile = f
				added[curFile] = map[int]bool{}
			} else {
				curFile = ""
			}
		case strings.HasPrefix(line, "+++ /dev/null"):
			curFile = ""
		case strings.HasPrefix(line, "@@"):
			if curFile == "" {
				continue
			}
			m := hunkRE.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			fmt.Sscanf(m[1], "%d", &curLine)
		case curFile != "" && strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			added[curFile][curLine] = true
			curLine++
		}
	}
	return added, sc.Err()
}

func patchTotals(cov map[string]fileCov, added map[string]map[int]bool, ignores []string) totals {
	var t totals
	for path, lines := range added {
		skip := false
		for _, p := range ignores {
			if matchIgnore(path, p) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		fc, ok := cov[path]
		if !ok {
			continue
		}
		for l := range lines {
			s, ok := fc[l]
			if !ok {
				continue
			}
			switch s {
			case hit:
				t.hit++
			case miss:
				t.miss++
			case partial:
				t.partial++
			}
		}
	}
	return t
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "coverage-check: "+format+"\n", args...)
	os.Exit(2)
}

func main() {
	profile := flag.String("profile", "", "coverage profile path (required)")
	minTotal := flag.Float64("min-total", 0, "total coverage threshold (0 = no check)")
	minPatch := flag.Float64("min-patch", 0, "patch coverage threshold (0 = no check)")
	patchBase := flag.String("patch-base", "", "git ref to diff against for patch coverage")
	var ignore stringList
	flag.Var(&ignore, "ignore", "path (or prefix/**) to ignore; repeatable")
	flag.Parse()

	if *profile == "" {
		die("-profile is required")
	}

	profiles, err := cover.ParseProfiles(*profile)
	if err != nil {
		die("parsing %s: %v", *profile, err)
	}

	cov := buildCoverage(profiles)
	applyIgnores(cov, ignore)

	t := sumTotals(cov)
	pct := t.pct()
	fmt.Printf("Total coverage: %.2f%% (hit=%d miss=%d partial=%d)\n",
		pct, t.hit, t.miss, t.partial)

	fail := false
	if *minTotal > 0 && pct < *minTotal {
		fmt.Fprintf(os.Stderr,
			"::error::Total coverage %.2f%% is below %.2f%% threshold\n",
			pct, *minTotal)
		fail = true
	}

	if *patchBase != "" {
		added, err := gitAddedLines(*patchBase)
		if err != nil {
			die("%v", err)
		}
		pt := patchTotals(cov, added, ignore)
		den := pt.hit + pt.miss + pt.partial
		if den == 0 {
			fmt.Println("Patch coverage: n/a (no instrumented lines in diff)")
		} else {
			ppct := pt.pct()
			fmt.Printf("Patch coverage: %.2f%% (hit=%d miss=%d partial=%d)\n",
				ppct, pt.hit, pt.miss, pt.partial)
			if *minPatch > 0 && ppct < *minPatch {
				fmt.Fprintf(os.Stderr,
					"::error::Patch coverage %.2f%% is below %.2f%% threshold\n",
					ppct, *minPatch)
				fail = true
			}
		}
	}

	if fail {
		os.Exit(1)
	}
}
