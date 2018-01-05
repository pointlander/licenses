package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/pmezard/licenses/assets"
)

const VendorPath = string(os.PathSeparator) + "vendor" + string(os.PathSeparator)

type Template struct {
	Title    string
	Nickname string
	Words    map[string]int
}

func parseTemplate(content string) (*Template, error) {
	t := Template{}
	text := []byte{}
	state := 0
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if state == 0 {
			if line == "---" {
				state = 1
			}
		} else if state == 1 {
			if line == "---" {
				state = 2
			} else {
				if strings.HasPrefix(line, "title:") {
					t.Title = strings.TrimSpace(line[len("title:"):])
				} else if strings.HasPrefix(line, "nickname:") {
					t.Nickname = strings.TrimSpace(line[len("nickname:"):])
				}
			}
		} else if state == 2 {
			text = append(text, scanner.Bytes()...)
			text = append(text, []byte("\n")...)
		}
	}
	t.Words = makeWordSet(text)
	return &t, scanner.Err()
}

func loadTemplates() ([]*Template, error) {
	templates := []*Template{}
	for _, a := range assets.Assets {
		templ, err := parseTemplate(a.Content)
		if err != nil {
			return nil, err
		}
		templates = append(templates, templ)
	}
	return templates, nil
}

var (
	reWords     = regexp.MustCompile(`[\w']+`)
	reCopyright = regexp.MustCompile(
		`(?i)\s*Copyright (?:©|\(c\)|\xC2\xA9)?\s*(?:\d{4}|\[year\]).*`)
)

func cleanLicenseData(data []byte) []byte {
	data = bytes.ToLower(data)
	data = reCopyright.ReplaceAll(data, nil)
	return data
}

func makeWordSet(data []byte) map[string]int {
	words := map[string]int{}
	data = cleanLicenseData(data)
	matches := reWords.FindAll(data, -1)
	for i, m := range matches {
		s := string(m)
		if _, ok := words[s]; !ok {
			// Non-matching words are likely in the license header, to mention
			// copyrights and authors. Try to preserve the initial sequences,
			// to display them later.
			words[s] = i
		}
	}
	return words
}

type Word struct {
	Text string
	Pos  int
}

type sortedWords []Word

func (s sortedWords) Len() int {
	return len(s)
}

func (s sortedWords) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s sortedWords) Less(i, j int) bool {
	return s[i].Pos < s[j].Pos
}

type MatchResult struct {
	Template     *Template
	Score        float64
	ExtraWords   []string
	MissingWords []string
}

func sortAndReturnWords(words []Word) []string {
	sort.Sort(sortedWords(words))
	tokens := []string{}
	for _, w := range words {
		tokens = append(tokens, w.Text)
	}
	return tokens
}

// matchTemplates returns the best license template matching supplied data,
// its score between 0 and 1 and the list of words appearing in license but not
// in the matched template.
func matchTemplates(license []byte, templates []*Template) MatchResult {
	bestScore := float64(-1)
	var bestTemplate *Template
	bestExtra := []Word{}
	bestMissing := []Word{}
	words := makeWordSet(license)
	for _, t := range templates {
		extra := []Word{}
		missing := []Word{}
		common := 0
		for w, pos := range words {
			_, ok := t.Words[w]
			if ok {
				common++
			} else {
				extra = append(extra, Word{
					Text: w,
					Pos:  pos,
				})
			}
		}
		for w, pos := range t.Words {
			if _, ok := words[w]; !ok {
				missing = append(missing, Word{
					Text: w,
					Pos:  pos,
				})
			}
		}
		score := 2 * float64(common) / (float64(len(words)) + float64(len(t.Words)))
		if score > bestScore {
			bestScore = score
			bestTemplate = t
			bestMissing = missing
			bestExtra = extra
		}
	}
	return MatchResult{
		Template:     bestTemplate,
		Score:        bestScore,
		ExtraWords:   sortAndReturnWords(bestExtra),
		MissingWords: sortAndReturnWords(bestMissing),
	}
}

// fixEnv returns a copy of the process environment where GOPATH is adjusted to
// supplied value. It returns nil if gopath is empty.
func fixEnv(gopath string) []string {
	if gopath == "" {
		return nil
	}
	kept := []string{
		"GOPATH=" + gopath,
	}
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "GOPATH=") {
			kept = append(kept, env)
		}
	}
	return kept
}

type MissingError struct {
	Err string
}

func (err *MissingError) Error() string {
	return err.Err
}

// expandPackages takes a list of package or package expressions and invoke go
// list to expand them to packages. In particular, it handles things like "..."
// and ".".
func expandPackages(gopath string, pkgs []string) ([]string, error) {
	args := []string{"list"}
	args = append(args, pkgs...)
	cmd := exec.Command("go", args...)
	cmd.Env = fixEnv(gopath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		output := string(out)
		if strings.Contains(output, "cannot find package") ||
			strings.Contains(output, "no buildable Go source files") ||
			strings.Contains(output, "can't load package") {
			return nil, &MissingError{Err: output}
		}
		return nil, fmt.Errorf("'go %s' failed with:\n%s",
			strings.Join(args, " "), output)
	}
	names := []string{}
	for _, s := range strings.Split(string(out), "\n") {
		s = strings.TrimSpace(s)
		if s != "" {
			names = append(names, s)
		}
	}
	return names, nil
}

func listPackagesAndDeps(gopath string, pkgs []string) ([]string, error) {
	pkgs, err := expandPackages(gopath, pkgs)
	if err != nil {
		return nil, err
	}
	args := []string{"list", "-f", "{{range .Deps}}{{.}}|{{end}}"}
	args = append(args, pkgs...)
	cmd := exec.Command("go", args...)
	cmd.Env = fixEnv(gopath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		output := string(out)
		if strings.Contains(output, "cannot find package") ||
			strings.Contains(output, "no buildable Go source files") ||
			strings.Contains(output, "can't load package") {
			return nil, &MissingError{Err: output}
		}
		return nil, fmt.Errorf("'go %s' failed with:\n%s",
			strings.Join(args, " "), output)
	}
	deps := []string{}
	seen := map[string]bool{}
	for _, s := range strings.Split(string(out), "|") {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			deps = append(deps, s)
			seen[s] = true
		}
	}
	for _, pkg := range pkgs {
		if !seen[pkg] {
			seen[pkg] = true
			deps = append(deps, pkg)
		}
	}
	sort.Strings(deps)
	return deps, nil
}

func listStandardPackages(gopath string) ([]string, error) {
	return expandPackages(gopath, []string{"std", "cmd"})
}

type PkgError struct {
	Err string
}

type PkgInfo struct {
	Name       string
	Dir        string
	Root       string
	ImportPath string
	Error      *PkgError
}

func getPackagesInfo(gopath string, pkgs []string) ([]*PkgInfo, error) {
	args := []string{"list", "-e", "-json"}
	// TODO: split the list for platforms which do not support massive argument
	// lists.
	args = append(args, pkgs...)
	cmd := exec.Command("go", args...)
	cmd.Env = fixEnv(gopath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("go %s failed with:\n%s",
			strings.Join(args, " "), string(out))
	}
	infos := make([]*PkgInfo, 0, len(pkgs))
	decoder := json.NewDecoder(bytes.NewBuffer(out))
	for _, pkg := range pkgs {
		info := &PkgInfo{}
		err := decoder.Decode(info)
		if err != nil {
			return nil, fmt.Errorf("could not retrieve package information for %s", pkg)
		}
		if pkg != info.ImportPath {
			return nil, fmt.Errorf("package information mismatch: asked for %s, got %s",
				pkg, info.ImportPath)
		}
		if info.Error != nil && info.Name == "" {
			info.Name = info.ImportPath
		}
		infos = append(infos, info)
	}
	return infos, err
}

var (
	reLicense = regexp.MustCompile(`(?i)^(?:` +
		`((?:un)?licen[sc]e)|` +
		`((?:un)?licen[sc]e\.(?:md|markdown|txt))|` +
		`(copy(?:ing|right)(?:\.[^.]+)?)|` +
		`(licen[sc]e\.[^.]+)|` +
		`(.*(?:un)?licen[sc]e.*)` +
		`)$`)
)

// scoreLicenseName returns a factor between 0 and 1 weighting how likely
// supplied filename is a license file.
func scoreLicenseName(name string) float64 {
	m := reLicense.FindStringSubmatch(name)
	switch {
	case m == nil:
		break
	case m[1] != "":
		return 1.0
	case m[2] != "":
		return 0.9
	case m[3] != "":
		return 0.8
	case m[4] != "":
		return 0.7
	case m[5] != "":
		return 0.6
	}
	return 0.
}

// findLicense looks for license files in package import path, and down to
// parent directories until a file is found or $GOPATH/src is reached. It
// returns the path and score of the best entry, an empty string if none was
// found.
func findLicense(info *PkgInfo) (string, error) {
	path := info.ImportPath
	for ; path != "."; path = filepath.Dir(path) {
		fis, err := ioutil.ReadDir(filepath.Join(info.Root, "src", path))
		if err != nil {
			return "", err
		}
		bestScore := float64(0)
		bestName := ""
		for _, fi := range fis {
			if !fi.Mode().IsRegular() {
				continue
			}
			score := scoreLicenseName(fi.Name())
			if score > bestScore {
				bestScore = score
				bestName = fi.Name()
			}
		}
		if bestName != "" {
			return filepath.Join(path, bestName), nil
		}
	}
	return "", nil
}

type License struct {
	Package      string
	Version      string
	Score        float64
	Template     *Template
	Path         string
	Err          string
	ExtraWords   []string
	MissingWords []string
}

func listLicenses(gopath string, pkgs []string) ([]License, error) {
	templates, err := loadTemplates()
	if err != nil {
		return nil, err
	}
	deps, err := listPackagesAndDeps(gopath, pkgs)
	if err != nil {
		if _, ok := err.(*MissingError); ok {
			return nil, err
		}
		return nil, fmt.Errorf("could not list %s dependencies: %s",
			strings.Join(pkgs, " "), err)
	}
	std, err := listStandardPackages(gopath)
	if err != nil {
		return nil, fmt.Errorf("could not list standard packages: %s", err)
	}
	stdSet := map[string]bool{}
	for _, n := range std {
		stdSet[n] = true
	}
	infos, err := getPackagesInfo(gopath, deps)
	if err != nil {
		return nil, err
	}

	// Cache matched licenses by path. Useful for package with a lot of
	// subpackages like bleve.
	matched := map[string]MatchResult{}

	licenses := []License{}
	for _, info := range infos {
		if info.Error != nil {
			licenses = append(licenses, License{
				Package: info.Name,
				Err:     info.Error.Err,
			})
			continue
		}
		if stdSet[info.ImportPath] {
			continue
		}
		path, err := findLicense(info)
		if err != nil {
			return nil, err
		}
		license := License{
			Package: info.ImportPath,
			Path:    path,
		}
		if path != "" {
			fpath := filepath.Join(info.Root, "src", path)
			m, ok := matched[fpath]
			if !ok {
				data, err := ioutil.ReadFile(fpath)
				if err != nil {
					return nil, err
				}
				m = matchTemplates(data, templates)
				matched[fpath] = m
			}
			license.Score = m.Score
			license.Template = m.Template
			license.ExtraWords = m.ExtraWords
			license.MissingWords = m.MissingWords
		}
		if strings.HasPrefix(info.Dir, gopath) || !strings.Contains(info.Dir, VendorPath) {
			current, err := os.Getwd()
			if err != nil {
				return nil, err
			}
			err = os.Chdir(info.Dir)
			if err != nil {
				return nil, err
			}
			cmd := exec.Command("git", "rev-parse", "HEAD")
			out, err := cmd.CombinedOutput()
			if err != nil {
				license.Version = "?"
			} else {
				license.Version = strings.TrimSpace(string(out))
			}
			err = os.Chdir(current)
			if err != nil {
				return nil, err
			}
		}
		licenses = append(licenses, license)
	}
	return licenses, nil
}

// longestCommonPrefix returns the longest common prefix over import path
// components of supplied licenses.
func longestCommonPrefix(licenses []License) string {
	type Node struct {
		Name     string
		Children map[string]*Node
	}
	// Build a prefix tree. Not super efficient, but easy to do.
	root := &Node{
		Children: map[string]*Node{},
	}
	for _, l := range licenses {
		n := root
		for _, part := range strings.Split(l.Package, "/") {
			c := n.Children[part]
			if c == nil {
				c = &Node{
					Name:     part,
					Children: map[string]*Node{},
				}
				n.Children[part] = c
			}
			n = c
		}
	}
	n := root
	prefix := []string{}
	for {
		if len(n.Children) != 1 {
			break
		}
		for _, c := range n.Children {
			prefix = append(prefix, c.Name)
			n = c
			break
		}
	}
	return strings.Join(prefix, "/")
}

// groupLicenses returns the input licenses after grouping them by license path
// and find their longest import path common prefix. Entries with empty paths
// are left unchanged.
func groupLicenses(licenses []License) ([]License, error) {
	paths := map[string][]License{}
	for _, l := range licenses {
		if l.Path == "" {
			continue
		}
		paths[l.Path] = append(paths[l.Path], l)
	}
	for k, v := range paths {
		if len(v) <= 1 {
			continue
		}
		prefix := longestCommonPrefix(v)
		if prefix == "" {
			return nil, fmt.Errorf(
				"packages share the same license but not common prefix: %v", v)
		}
		l := v[0]
		l.Package = prefix
		paths[k] = []License{l}
	}
	kept := []License{}
	for _, l := range licenses {
		if l.Path == "" {
			kept = append(kept, l)
			continue
		}
		if v, ok := paths[l.Path]; ok {
			kept = append(kept, v[0])
			delete(paths, l.Path)
		}
	}
	return kept, nil
}

type Row struct {
	Package, Version, License, Match, Words string
	Score                                   float64
}

type Rows []Row

func (r Rows) Len() int {
	return len(r)
}

func (r Rows) Less(i, j int) bool {
	ii, jj := r[i], r[j]
	if license := strings.Compare(ii.License, jj.License); license < 0 {
		return true
	} else if license == 0 {
		if ii.Score < jj.Score {
			return true
		} else if ii.Score == jj.Score {
			if pack := strings.Compare(ii.Package, jj.Package); pack < 0 {
				return true
			}
		}
	}
	return false
}

func (r Rows) Swap(i, j int) {
	r[i], r[j] = r[j], r[i]
}

func generateReport(report string, licenses []License, confidence float64, words bool) error {
	table := make(Rows, len(licenses))
	for i, l := range licenses {
		license, diff := "?", ""
		if l.Template != nil {
			if l.Score > .99 {
				license = fmt.Sprintf("%s", l.Template.Title)
			} else if l.Score >= confidence {
				license = fmt.Sprintf("%s", l.Template.Title)
				for _, word := range l.ExtraWords {
					diff += " +" + word
				}
				for _, word := range l.MissingWords {
					diff += " -" + word
				}
			} else {
				license = fmt.Sprintf("? (%s)", l.Template.Title)
			}
		} else if l.Err != "" {
			license = strings.Replace(l.Err, "\n", " ", -1)
		}
		table[i].Package = l.Package
		table[i].Version = l.Version
		table[i].License = license
		table[i].Match = fmt.Sprintf("%2d%%", int(100*l.Score+.5))
		table[i].Words = diff
		table[i].Score = l.Score
	}
	sort.Sort(table)

	maxPackage, maxVersion, maxLicense, maxMatch, maxWords := 0, 0, 0, 0, 0
	for _, row := range table {
		if width := len(row.Package); width > maxPackage {
			maxPackage = width
		}
		if width := len(row.Version); width > maxVersion {
			maxVersion = width
		}
		if width := len(row.License); width > maxLicense {
			maxLicense = width
		}
		if width := len(row.Match); width > maxMatch {
			maxMatch = width
		}
		if width := len(row.Words); width > maxWords {
			maxWords = width
		}
	}

	out, err := os.Create(report)
	if err != nil {
		return err
	}
	defer out.Close()

	writeHeading := func(name string, width int) int {
		out.WriteString(" ")
		out.WriteString(name)
		padding, rowWidth := width-len(name), width
		if padding < 0 {
			padding, rowWidth = 0, len(name)
		}
		for i := 0; i < padding; i++ {
			out.WriteString(" ")
		}
		out.WriteString(" |")

		return rowWidth
	}
	out.WriteString("|")
	rowWidthPackage := writeHeading("Package", maxPackage)
	rowWidthVersion := writeHeading("Version", maxVersion)
	rowWidthLicense := writeHeading("License", maxLicense)
	rowWidthMatch := writeHeading("Match", maxMatch)
	var rowWidthWords int
	if words {
		rowWidthWords = writeHeading("Words", maxWords)
	}
	out.WriteString("\n")

	writeSep := func(width int) {
		out.WriteString(" ")
		for i := 0; i < width; i++ {
			out.WriteString("-")
		}
		out.WriteString(" |")
	}
	out.WriteString("|")
	writeSep(rowWidthPackage)
	writeSep(rowWidthVersion)
	writeSep(rowWidthLicense)
	writeSep(rowWidthMatch)
	if words {
		writeSep(rowWidthWords)
	}
	out.WriteString("\n")

	writeRow := func(data string, width int) {
		out.WriteString(" ")
		out.WriteString(data)
		padding := width - len(data)
		for i := 0; i < padding; i++ {
			out.WriteString(" ")
		}
		out.WriteString(" |")
	}
	for _, row := range table {
		out.WriteString("|")
		writeRow(row.Package, rowWidthPackage)
		writeRow(row.Version, rowWidthVersion)
		writeRow(row.License, rowWidthLicense)
		writeRow(row.Match, rowWidthMatch)
		if words {
			writeRow(row.Words, rowWidthWords)
		}
		out.WriteString("\n")
	}

	return nil
}

func printLicenses() error {
	flag.Usage = func() {
		fmt.Println(`Usage: licenses IMPORTPATH...

licenses lists all dependencies of specified packages or commands, excluding
standard library packages, and prints their licenses. Licenses are detected by
looking for files named like LICENSE, COPYING, COPYRIGHT and other variants in
the package directory, and its parent directories until one is found. Files
content is matched against a set of well-known licenses and the best match is
displayed along with its score.

With -a, all individual packages are displayed instead of grouping them by
license files.
With -w, words in package license file not found in the template license are
displayed. It helps assessing the changes importance.
With -r, a report is generated and saved in the specified file.`)
		os.Exit(1)
	}
	all := flag.Bool("a", false, "display all individual packages")
	words := flag.Bool("w", false, "display words not matching license template")
	report := flag.String("r", "", "generate a report file")
	flag.Parse()
	if flag.NArg() < 1 {
		return fmt.Errorf("expect at least one package argument")
	}
	pkgs := flag.Args()

	confidence := 0.9
	licenses, err := listLicenses("", pkgs)
	if err != nil {
		return err
	}
	if !*all {
		licenses, err = groupLicenses(licenses)
		if err != nil {
			return err
		}
	}

	if *report != "" {
		return generateReport(*report, licenses, confidence, *words)
	}

	w := tabwriter.NewWriter(os.Stdout, 1, 4, 2, ' ', 0)
	for _, l := range licenses {
		license := "?"
		if l.Template != nil {
			if l.Score > .99 {
				license = fmt.Sprintf("%s", l.Template.Title)
			} else if l.Score >= confidence {
				license = fmt.Sprintf("%s (%2d%%)", l.Template.Title, int(100*l.Score))
				if *words && len(l.ExtraWords) > 0 {
					license += "\n\t+words: " + strings.Join(l.ExtraWords, ", ")
				}
				if *words && len(l.MissingWords) > 0 {
					license += "\n\t-words: " + strings.Join(l.MissingWords, ", ")
				}
			} else {
				license = fmt.Sprintf("? (%s, %2d%%)", l.Template.Title, int(100*l.Score))
			}
		} else if l.Err != "" {
			license = strings.Replace(l.Err, "\n", " ", -1)
		}
		_, err = w.Write([]byte(l.Package + "\t" + license + "\n"))
		if err != nil {
			return err
		}
	}
	return w.Flush()
}

func main() {
	err := printLicenses()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err)
		os.Exit(1)
	}
}
