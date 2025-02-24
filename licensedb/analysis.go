package licensedb

import (
	"net/url"
	"os"
	"sort"
	"sync"

	"gopkg.in/src-d/go-license-detector.v2/licensedb/filer"
)

// Analyse runs license analysis on each item in `args`
func Analyse(args ...string) []Result {
	nargs := len(args)
	results := make([]Result, nargs)
	var wg sync.WaitGroup
	wg.Add(nargs)
	for i, arg := range args {
		go func(i int, arg string) {
			defer wg.Done()
			matches, err := process(arg)
			res := Result{Arg: arg, Matches: matches}
			if err != nil {
				res.ErrStr = err.Error()
			}
			results[i] = res
		}(i, arg)
	}
	wg.Wait()

	return results
}

// Result gathers license detection results for a project path
type Result struct {
	Arg     string  `json:"project,omitempty"`
	Matches []Match `json:"matches,omitempty"`
	ErrStr  string  `json:"error,omitempty"`
}

// Match describes the level of confidence for the detected License
type Match struct {
	License    string  `json:"license"`
	Confidence float32 `json:"confidence"`
}

func process(arg string) ([]Match, error) {
	newFiler := filer.FromDirectory
	fi, err := os.Stat(arg)
	if err != nil {
		if _, err := url.Parse(arg); err == nil {
			newFiler = filer.FromGitURL
		}
	} else if !fi.IsDir() {
		newFiler = filer.FromSiva
	}

	resolvedFiler, err := newFiler(arg)
	if err != nil {
		return nil, err
	}

	ls, err := Detect(resolvedFiler)
	if err != nil {
		return nil, err
	}

	var matches []Match
	for k, v := range ls {
		matches = append(matches, Match{k, v.Confidence})
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Confidence > matches[j].Confidence })
	return matches, nil
}
