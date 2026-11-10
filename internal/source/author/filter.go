package author

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/gluck/ebac/internal/forge"
	"github.com/gluck/ebac/internal/model"
)

// The relevance filter (cheroot work order 2026-11-09, Boss via the Minister:
// only Chiqun's PRs related to the effort work). A PR in the recent window is
// TRACKED when its title matches title_match OR any file it changes matches
// one of paths; neither key tracks every PR. An untracked PR never wakes and
// leaves only its number and head in the skip list.

// filter is a compiled AuthorFilter; nil tracks everything.
type filter struct {
	title *regexp.Regexp
	paths []*regexp.Regexp
}

func (f *filter) all() bool { return f == nil || (f.title == nil && len(f.paths) == 0) }

func (f *filter) titleHit(title string) bool { return f.title != nil && f.title.MatchString(title) }

// pathHit says whether any file matches a glob. A glob matches the file's path
// or any directory above it, so "**/system1*" catches a file under system1/.
func (f *filter) pathHit(files []string) bool {
	for _, file := range files {
		for p := file; p != ""; {
			for _, re := range f.paths {
				if re.MatchString(p) {
					return true
				}
			}
			i := strings.LastIndex(p, "/")
			if i < 0 {
				break
			}
			p = p[:i]
		}
	}
	return false
}

// specFromKeys reads the role-form keys: paths is a JSON list of globs or one
// comma-separated string; title_match is a string. Absent or empty means unset.
func specFromKeys(keys map[string]json.RawMessage) (model.AuthorFilter, error) {
	var spec model.AuthorFilter
	if raw := keys[model.AuthorPathsKey]; len(raw) > 0 && string(raw) != "null" {
		var list []string
		var one string
		switch {
		case json.Unmarshal(raw, &list) == nil:
		case json.Unmarshal(raw, &one) == nil:
			list = strings.Split(one, ",")
		default:
			return spec, fmt.Errorf("%s: want a list of globs or a comma-separated string, got %s", model.AuthorPathsKey, raw)
		}
		for _, g := range list {
			if g = strings.TrimSpace(g); g != "" {
				spec.Paths = append(spec.Paths, g)
			}
		}
	}
	if raw := keys[model.AuthorTitleKey]; len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &spec.TitleMatch); err != nil {
			return spec, fmt.Errorf("%s: want a string, got %s", model.AuthorTitleKey, raw)
		}
		spec.TitleMatch = strings.TrimSpace(spec.TitleMatch)
	}
	return spec, nil
}

func compileFilter(spec model.AuthorFilter) (*filter, error) {
	f := &filter{}
	if spec.TitleMatch != "" {
		re, err := regexp.Compile(spec.TitleMatch)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", model.AuthorTitleKey, err)
		}
		f.title = re
	}
	for _, g := range spec.Paths {
		re, err := globRegexp(g)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", model.AuthorPathsKey, err)
		}
		f.paths = append(f.paths, re)
	}
	return f, nil
}

// globRegexp compiles a path glob: ** spans directories ("**/" also matches
// none), * and ? stay within one. Brackets and braces are refused, so a glob
// never means something other than it reads.
func globRegexp(g string) (*regexp.Regexp, error) {
	if strings.ContainsAny(g, "[]{}\\") {
		return nil, fmt.Errorf("glob %q: brackets, braces and backslashes are not supported", g)
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(g); {
		switch {
		case strings.HasPrefix(g[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 3
		case strings.HasPrefix(g[i:], "**"):
			b.WriteString(".*")
			i += 2
		case g[i] == '*':
			b.WriteString("[^/]*")
			i++
		case g[i] == '?':
			b.WriteString("[^/]")
			i++
		default:
			b.WriteString(regexp.QuoteMeta(g[i : i+1]))
			i++
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// resolveFilter is the filter in force and the error to show on the form ("" when the keys are absent or valid).
// A valid filter becomes the last good one; a broken one leaves the last good one in force (else every PR is
// tracked, as before filters existed).
func resolveFilter(st *model.AuthorState, keys map[string]json.RawMessage) (*filter, model.AuthorFilter, string) {
	spec, err := specFromKeys(keys)
	var f *filter
	if err == nil {
		f, err = compileFilter(spec)
	}
	if err == nil {
		st.Filter = nil
		if spec.TitleMatch != "" || len(spec.Paths) > 0 {
			st.Filter = &spec
		}
		return f, spec, ""
	}
	in := "no filter (every PR tracked)"
	var good model.AuthorFilter
	if st.Filter != nil {
		if g, gerr := compileFilter(*st.Filter); gerr == nil {
			f, good, in = g, *st.Filter, "the last good filter"
		}
	}
	return f, good, fmt.Sprintf("filter rejected, %s is in force: %v", in, err)
}

// FilesFunc lists a PR's changed files (forge.GH.PRFiles).
type FilesFunc func(ctx context.Context, repo string, number int) ([]string, error)

// fetchParallel bounds concurrent file listings: the first round of a new filter lists every untitled PR in the window.
const fetchParallel = 4

// relevant splits the window into tracked PRs, deciding each by title, then by its cached path verdict at its head,
// then by its file list (fetched once per head). It updates st.Hit and st.Skip and returns the keys that were in the
// skip list before this round and are tracked now (promoted: news, not a re-entry).
func relevant(ctx context.Context, st *model.AuthorState, f *filter, spec model.AuthorFilter, prs []forge.AuthoredPR, files FilesFunc) ([]forge.AuthoredPR, map[string]bool, error) {
	if f.all() {
		st.FilterSum, st.Hit, st.Skip = "", nil, nil
		return prs, nil, nil
	}
	if sum := digest(model.MustJSON(spec)); sum != st.FilterSum {
		st.FilterSum, st.Hit, st.Skip = sum, nil, nil // verdicts made under another filter are void
	}
	wasSkipped := map[string]bool{}
	for k := range st.Skip {
		wasSkipped[k] = true
	}
	verdict := map[string]bool{}
	var need []forge.AuthoredPR
	for _, pr := range prs {
		k := pr.Key()
		switch {
		case f.titleHit(pr.Title):
			verdict[k] = true
		case len(f.paths) == 0:
			verdict[k] = false
		case st.Hit[k] != "" && st.Hit[k] == pr.Head:
			verdict[k] = true
		case st.Skip[k] != "" && st.Skip[k] == pr.Head:
			verdict[k] = false
		default:
			need = append(need, pr)
		}
	}
	type got struct {
		hit bool
		err error
	}
	res := make([]got, len(need))
	var wg sync.WaitGroup
	sem := make(chan struct{}, fetchParallel)
	for i, pr := range need {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			fs, err := files(ctx, pr.Repo, pr.Number)
			res[i] = got{hit: err == nil && f.pathHit(fs), err: err}
		}()
	}
	wg.Wait()
	for i, pr := range need {
		if res[i].err != nil {
			return nil, nil, fmt.Errorf("files of %s: %w", pr.Key(), res[i].err)
		}
		verdict[pr.Key()] = res[i].hit
	}

	inWindow := map[string]bool{}
	var tracked []forge.AuthoredPR
	promoted := map[string]bool{}
	for _, pr := range prs {
		k := pr.Key()
		inWindow[k] = true
		if st.Hit == nil {
			st.Hit = map[string]string{}
		}
		if st.Skip == nil {
			st.Skip = map[string]string{}
		}
		if !verdict[k] {
			st.Skip[k] = pr.Head
			delete(st.Hit, k)
			continue
		}
		delete(st.Skip, k)
		if !f.titleHit(pr.Title) {
			st.Hit[k] = pr.Head
		}
		tracked = append(tracked, pr)
		if wasSkipped[k] {
			promoted[k] = true
		}
	}
	for k := range st.Hit {
		if !inWindow[k] {
			delete(st.Hit, k)
		}
	}
	for k := range st.Skip {
		if !inWindow[k] {
			delete(st.Skip, k)
		}
	}
	return tracked, promoted, nil
}

// CheckFilter validates a filter before it is stored (ebac add-author).
func CheckFilter(spec model.AuthorFilter) error {
	_, err := compileFilter(spec)
	return err
}
