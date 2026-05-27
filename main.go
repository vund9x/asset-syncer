package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Source describes one updatable item.
//
// Type "file": download a single file from URL -> Dest (relative to root).
// Type "repo": download a GitHub repo zip (Repo + Branch), then extract files
//
//	whose path starts with PathPrefix (inside the repo) into Dest.
//	If Include / Exclude (glob, matched against the path *inside*
//	Dest) are set, they filter what is copied.
type Source struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"` // "file" | "repo" | "release"
	URL        string   `json:"url,omitempty"`
	Repo       string   `json:"repo,omitempty"`   // owner/name
	Branch     string   `json:"branch,omitempty"` // default: master (for "repo")
	PathPrefix string   `json:"path_prefix,omitempty"`
	Dest       string   `json:"dest"` // relative path
	Include    []string `json:"include,omitempty"`
	Exclude    []string `json:"exclude,omitempty"`
	Disabled   bool     `json:"disabled,omitempty"`

	// "release" specific:
	// Tag: "latest" (default) or a specific tag like "1.1".
	// Asset: glob pattern matched against asset name (e.g. "FSRCNNX_x2_16-*.glsl").
	//        If multiple match, all are downloaded into Dest (treated as a directory).
	//        If exactly one match, Dest can be the file path itself.
	Tag   string `json:"tag,omitempty"`
	Asset string `json:"asset,omitempty"`

	// Archive extraction (for "file" and "release" types).
	// Extract: if true, treat the downloaded payload as a zip and extract it
	//          into Dest (which is then treated as a directory).
	// StripComponents: number of leading path components to strip from each
	//          entry in the archive (like tar --strip-components).
	// PathPrefix / Include / Exclude are reused for filtering.
	Extract         bool `json:"extract,omitempty"`
	StripComponents int  `json:"strip_components,omitempty"`
}

type Config struct {
	Root    string   `json:"root"`              // root dir, e.g. "portable_config"
	Include []string `json:"include,omitempty"` // glob paths to additional config files
	Sources []Source `json:"sources,omitempty"`
}

var (
	cfgPath    = flag.String("config", "sources.json", "path to sources.json")
	dryRun     = flag.Bool("dry-run", false, "do not write any files")
	only       = flag.String("only", "", "comma-separated names to update (default: all)")
	jobs       = flag.Int("jobs", 4, "parallel downloads")
	verbose    = flag.Bool("v", false, "verbose")
	httpClient = &http.Client{Timeout: 5 * time.Minute}
)

func main() {
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		die("config: %v", err)
	}

	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		die("root abs: %v", err)
	}

	filter := map[string]bool{}
	if *only != "" {
		for _, n := range strings.Split(*only, ",") {
			filter[strings.TrimSpace(n)] = true
		}
	}

	var (
		wg    sync.WaitGroup
		sem   = make(chan struct{}, *jobs)
		stats = struct {
			sync.Mutex
			updated, unchanged, added, failed int
		}{}
	)

	for _, s := range cfg.Sources {
		s := s
		if s.Disabled {
			continue
		}
		if len(filter) > 0 && !filter[s.Name] {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			res, err := process(root, s)
			stats.Lock()
			defer stats.Unlock()
			if err != nil {
				stats.failed++
				fmt.Printf("[FAIL]   %-25s %v\n", s.Name, err)
				return
			}
			stats.updated += res.changed
			stats.unchanged += res.unchanged
			stats.added += res.added
			tag := "OK"
			if res.changed > 0 || res.added > 0 {
				tag = "UPDATED"
			}
			fmt.Printf("[%-7s] %-25s changed=%d added=%d unchanged=%d\n",
				tag, s.Name, res.changed, res.added, res.unchanged)
		}()
	}
	wg.Wait()

	fmt.Printf("\nSummary: updated=%d added=%d unchanged=%d failed=%d\n",
		stats.updated, stats.added, stats.unchanged, stats.failed)
	if stats.failed > 0 {
		waitExit(1)
	}
	waitExit(0)
}

type result struct {
	changed, added, unchanged int
}

func process(root string, s Source) (result, error) {
	switch s.Type {
	case "file":
		return processFile(root, s)
	case "repo":
		return processRepo(root, s)
	case "release":
		return processRelease(root, s)
	default:
		return result{}, fmt.Errorf("unknown type %q", s.Type)
	}
}

func processFile(root string, s Source) (result, error) {
	if s.URL == "" || s.Dest == "" {
		return result{}, errors.New("file source needs url and dest")
	}
	data, err := httpGet(s.URL)
	if err != nil {
		return result{}, err
	}
	if s.Extract {
		return extractZipBytes(data, resolveDest(root, s.Dest), s.PathPrefix, s.StripComponents, s.Include, s.Exclude)
	}
	return writeIfChanged(resolveDest(root, s.Dest), data)
}

// resolveDest joins root + dest, but lets dest override root when absolute.
func resolveDest(root, dest string) string {
	if filepath.IsAbs(dest) {
		return filepath.Clean(dest)
	}
	return filepath.Join(root, dest)
}

// extractZipBytes extracts a zip archive in memory into destDir.
//   - pathPrefix: literal prefix inside the archive; entries not under it are skipped.
//     The prefix is then stripped from each entry's path.
//   - stripComponents: number of leading path components to strip *after* pathPrefix.
//   - include / exclude: glob patterns matched against the resulting relative path
//     (and its basename).
func extractZipBytes(data []byte, destDir, pathPrefix string, stripComponents int, include, exclude []string) (result, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return result{}, fmt.Errorf("zip: %w", err)
	}
	prefix := strings.TrimPrefix(pathPrefix, "/")
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	var res result
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if prefix != "" && !strings.HasPrefix(f.Name, prefix) {
			continue
		}
		rel := strings.TrimPrefix(f.Name, prefix)
		if stripComponents > 0 {
			parts := strings.SplitN(rel, "/", stripComponents+1)
			if len(parts) <= stripComponents {
				continue
			}
			rel = parts[stripComponents]
		}
		if rel == "" {
			continue
		}
		if !matchAny(include, rel, true) {
			continue
		}
		if matchAny(exclude, rel, false) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return res, err
		}
		buf, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return res, err
		}
		dst := filepath.Join(destDir, filepath.FromSlash(rel))
		r2, err := writeIfChanged(dst, buf)
		if err != nil {
			return res, err
		}
		res = res.merge(r2)
	}
	return res, nil
}

func processRepo(root string, s Source) (result, error) {
	if s.Repo == "" || s.Dest == "" {
		return result{}, errors.New("repo source needs repo and dest")
	}
	branch := s.Branch
	if branch == "" {
		branch = "master"
	}
	zipURL := fmt.Sprintf("https://codeload.github.com/%s/zip/refs/heads/%s", s.Repo, branch)
	if *verbose {
		fmt.Printf("  fetching %s\n", zipURL)
	}
	data, err := httpGet(zipURL)
	if err != nil {
		// try main
		if branch == "master" {
			alt := fmt.Sprintf("https://codeload.github.com/%s/zip/refs/heads/main", s.Repo)
			data, err = httpGet(alt)
		}
		if err != nil {
			return result{}, err
		}
	}
	// GitHub repo zips wrap everything under "<repo>-<branch>/".
	// Strip that one component, then apply user PathPrefix on top.
	return extractZipBytes(data, resolveDest(root, s.Dest), s.PathPrefix, 1, s.Include, s.Exclude)
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}
type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

func processRelease(root string, s Source) (result, error) {
	if s.Repo == "" || s.Dest == "" || s.Asset == "" {
		return result{}, errors.New("release source needs repo, asset, dest")
	}
	tag := s.Tag
	if tag == "" {
		tag = "latest"
	}
	var apiURL string
	if tag == "latest" {
		apiURL = fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", s.Repo)
	} else {
		apiURL = fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", s.Repo, tag)
	}
	body, err := httpGetWithHeader(apiURL, map[string]string{
		"Accept": "application/vnd.github+json",
	})
	if err != nil {
		return result{}, fmt.Errorf("github api: %w", err)
	}
	var rel ghRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		return result{}, fmt.Errorf("parse release: %w", err)
	}

	var matched []ghAsset
	for _, a := range rel.Assets {
		ok, _ := filepath.Match(s.Asset, a.Name)
		if ok {
			matched = append(matched, a)
		}
	}
	if len(matched) == 0 {
		return result{}, fmt.Errorf("no asset matches %q in %s@%s", s.Asset, s.Repo, rel.TagName)
	}
	if *verbose {
		fmt.Printf("  release %s@%s: %d asset(s) matched\n", s.Repo, rel.TagName, len(matched))
	}

	var res result
	for _, a := range matched {
		data, err := httpGet(a.BrowserDownloadURL)
		if err != nil {
			return res, err
		}
		if s.Extract {
			r2, err := extractZipBytes(data, resolveDest(root, s.Dest), s.PathPrefix, s.StripComponents, s.Include, s.Exclude)
			if err != nil {
				return res, err
			}
			res = res.merge(r2)
			continue
		}
		var dst string
		if len(matched) == 1 && filepath.Ext(s.Dest) != "" {
			dst = resolveDest(root, s.Dest)
		} else {
			dst = filepath.Join(resolveDest(root, s.Dest), a.Name)
		}
		r2, err := writeIfChanged(dst, data)
		if err != nil {
			return res, err
		}
		res = res.merge(r2)
	}
	return res, nil
}

func matchAny(patterns []string, name string, defWhenEmpty bool) bool {
	if len(patterns) == 0 {
		return defWhenEmpty
	}
	for _, p := range patterns {
		ok, _ := filepath.Match(p, name)
		if ok {
			return true
		}
		// also try matching just the base
		ok, _ = filepath.Match(p, filepath.Base(name))
		if ok {
			return true
		}
	}
	return false
}

func writeIfChanged(dst string, data []byte) (result, error) {
	cur, err := os.ReadFile(dst)
	if err == nil {
		if sha(cur) == sha(data) {
			return result{unchanged: 1}, nil
		}
		if *dryRun {
			fmt.Printf("  would update %s\n", dst)
			return result{changed: 1}, nil
		}
		if err := os.WriteFile(dst, data, 0644); err != nil {
			return result{}, err
		}
		return result{changed: 1}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return result{}, err
	}
	if *dryRun {
		fmt.Printf("  would add    %s\n", dst)
		return result{added: 1}, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return result{}, err
	}
	if err := os.WriteFile(dst, data, 0644); err != nil {
		return result{}, err
	}
	return result{added: 1}, nil
}

func (a result) merge(b result) result {
	a.added += b.added
	a.changed += b.changed
	a.unchanged += b.unchanged
	return a
}

func sha(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func httpGet(url string) ([]byte, error) {
	return httpGetWithHeader(url, nil)
}

func httpGetWithHeader(url string, headers map[string]string) ([]byte, error) {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "mpv-updater/1.0")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" && strings.HasPrefix(url, "https://api.github.com/") {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func loadConfig(path string) (*Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	root := &Config{}
	seen := map[string]bool{}
	names := map[string]string{} // source name -> file it came from (for dup detection)
	if err := loadConfigFile(abs, root, seen, names, true); err != nil {
		return nil, err
	}
	if root.Root == "" {
		root.Root = "portable_config"
	}
	return root, nil
}

func loadConfigFile(abs string, out *Config, seen map[string]bool, names map[string]string, isRoot bool) error {
	if seen[abs] {
		return nil // cycle / already loaded
	}
	seen[abs] = true

	b, err := os.ReadFile(abs)
	if err != nil {
		return fmt.Errorf("read %s: %w", abs, err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return fmt.Errorf("parse %s: %w", abs, err)
	}

	if isRoot {
		out.Root = c.Root
	} else if c.Root != "" && *verbose {
		fmt.Printf("note: ignoring \"root\" in included file %s\n", abs)
	}

	for _, s := range c.Sources {
		if s.Name == "" {
			return fmt.Errorf("%s: source missing \"name\"", abs)
		}
		if prev, dup := names[s.Name]; dup {
			return fmt.Errorf("duplicate source %q in %s (already defined in %s)", s.Name, abs, prev)
		}
		names[s.Name] = abs
		out.Sources = append(out.Sources, s)
	}

	// Resolve includes relative to the directory of *this* config file.
	dir := filepath.Dir(abs)
	for _, pat := range c.Include {
		if !filepath.IsAbs(pat) {
			pat = filepath.Join(dir, pat)
		}
		matches, err := filepath.Glob(pat)
		if err != nil {
			return fmt.Errorf("bad include glob %q: %w", pat, err)
		}
		if len(matches) == 0 && *verbose {
			fmt.Printf("note: include %q matched no files\n", pat)
		}
		for _, m := range matches {
			mAbs, err := filepath.Abs(m)
			if err != nil {
				return err
			}
			if err := loadConfigFile(mAbs, out, seen, names, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func die(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "error: "+f+"\n", a...)
	waitExit(1)
}

func waitExit(code int) {
	fmt.Println("\nNhấn Enter để thoát...")
	fmt.Scanln()
	os.Exit(code)
}
