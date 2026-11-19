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
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Source describes one updatable item.
//
// Type "file": download a single file from URL -> Dest (relative to root).
// Type "repo": download a GitHub repo zip (Repo + Branch), then extract files
//
//	whose path starts with PathPrefix (inside the repo) into Dest.
//	If Include / Exclude (glob, matched against the path *inside*
//	Dest) are set, they filter what is copied.
//
// Type "compose": pull the images of services in a docker-compose.yml
// (docker or podman) and recreate only the containers whose image changed.
type Source struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"` // "file" | "repo" | "release" | "compose"
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

	// "compose" specific:
	// File: path to docker-compose.yml (absolute, or relative to root).
	// Services: optional subset of service names (default: all with "image").
	// Engine: "auto" (default) | "docker" | "podman".
	// ComposeCmd: optional command override, e.g. "podman-compose".
	// Prune: after an image update, run "<engine> image prune -a -f".
	// ForceRecreate: pass --force-recreate to "up -d" (needed by some
	// podman-compose versions that miss digest-only changes).
	File          string   `json:"file,omitempty"`
	Services      []string `json:"services,omitempty"`
	Engine        string   `json:"engine,omitempty"`
	ComposeCmd    string   `json:"compose_cmd,omitempty"`
	Prune         bool     `json:"prune,omitempty"`
	ForceRecreate bool     `json:"force_recreate,omitempty"`
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
	case "compose":
		return processCompose(root, s)
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

// ---- compose: update images of docker/podman compose services ----

type composeService struct {
	name  string
	image string
}

type composeDoc struct {
	Services map[string]struct {
		Image string `yaml:"image"`
	} `yaml:"services"`
}

func parseCompose(data []byte) ([]composeService, error) {
	var doc composeDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse compose: %w", err)
	}
	if len(doc.Services) == 0 {
		return nil, errors.New("compose file has no services")
	}
	names := make([]string, 0, len(doc.Services))
	for n := range doc.Services {
		names = append(names, n)
	}
	sort.Strings(names)

	var out []composeService
	for _, n := range names {
		img := strings.TrimSpace(doc.Services[n].Image)
		if img == "" {
			if *verbose {
				fmt.Printf("  skip service %q: no image\n", n)
			}
			continue
		}
		out = append(out, composeService{name: n, image: img})
	}
	if len(out) == 0 {
		return nil, errors.New("no service with an image in compose file")
	}
	return out, nil
}

func selectComposeServices(all []composeService, want []string) ([]composeService, error) {
	if len(want) == 0 {
		return all, nil
	}
	byName := make(map[string]composeService, len(all))
	available := make([]string, 0, len(all))
	for _, svc := range all {
		byName[svc.name] = svc
		available = append(available, svc.name)
	}
	var out []composeService
	var missing []string
	picked := map[string]bool{}
	for _, n := range want {
		svc, ok := byName[n]
		if !ok {
			missing = append(missing, n)
			continue
		}
		if picked[n] {
			continue
		}
		picked[n] = true
		out = append(out, svc)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("unknown service(s) %s (available: %s)",
			strings.Join(missing, ", "), strings.Join(available, ", "))
	}
	return out, nil
}

// resolveComposeCommand returns the compose CLI argv prefix and the container
// engine binary used for "image inspect".
func resolveComposeCommand(s Source) ([]string, string, error) {
	explicit := strings.ToLower(strings.TrimSpace(s.Engine))
	var candidates [][]string
	switch explicit {
	case "", "auto":
		candidates = [][]string{{"docker", "compose"}, {"podman", "compose"}, {"podman-compose"}}
	case "docker":
		candidates = [][]string{{"docker", "compose"}}
	case "podman":
		candidates = [][]string{{"podman", "compose"}, {"podman-compose"}}
	default:
		return nil, "", fmt.Errorf("unknown engine %q (use auto, docker or podman)", s.Engine)
	}
	if s.ComposeCmd != "" {
		fields := strings.Fields(s.ComposeCmd)
		if len(fields) == 0 {
			return nil, "", errors.New("compose_cmd is empty")
		}
		candidates = [][]string{fields}
	}
	for _, c := range candidates {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		probe := exec.Command(c[0], append(append([]string{}, c[1:]...), "--version")...)
		if probe.Run() != nil {
			continue
		}
		eng := engineFromComposeBinary(c[0])
		if eng == "" {
			if explicit == "docker" || explicit == "podman" {
				eng = explicit
			} else {
				return nil, "", fmt.Errorf("cannot tell docker or podman from %q; set engine explicitly", c[0])
			}
		}
		return c, eng, nil
	}
	if s.ComposeCmd != "" {
		return nil, "", fmt.Errorf("compose command %q not found or not working", s.ComposeCmd)
	}
	return nil, "", errors.New("no working compose command found (install docker compose or podman compose)")
}

func engineFromComposeBinary(bin string) string {
	b := strings.ToLower(filepath.Base(bin))
	switch {
	case strings.Contains(b, "podman"):
		return "podman"
	case strings.Contains(b, "docker"):
		return "docker"
	default:
		return ""
	}
}

func imageID(engine, image string) string {
	out, err := exec.Command(engine, "image", "inspect", "--format", "{{.Id}}", image).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func runCmd(dir string, args []string) (string, error) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		if s != "" {
			return s, fmt.Errorf("%s: %w\n%s", strings.Join(args, " "), err, tail(s, 2000))
		}
		return s, fmt.Errorf("%s: %w", strings.Join(args, " "), err)
	}
	return s, nil
}

func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "..." + string(r[len(r)-n:])
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}

func processCompose(root string, s Source) (result, error) {
	if s.File == "" {
		return result{}, errors.New("compose source needs file")
	}
	file := resolveDest(root, s.File)
	data, err := os.ReadFile(file)
	if err != nil {
		return result{}, fmt.Errorf("read compose: %w", err)
	}
	all, err := parseCompose(data)
	if err != nil {
		return result{}, err
	}
	services, err := selectComposeServices(all, s.Services)
	if err != nil {
		return result{}, err
	}
	dir := filepath.Dir(file)

	if *dryRun {
		for _, svc := range services {
			fmt.Printf("  would pull   %-20s %s\n", svc.name, svc.image)
		}
		fmt.Println("  would pull & recreate changed services via docker/podman compose")
		if s.Prune {
			fmt.Println("  would run image prune -a -f after an update")
		}
		return result{}, nil
	}

	composeArgs, engine, err := resolveComposeCommand(s)
	if err != nil {
		return result{}, err
	}

	before := make(map[string]string, len(services))
	for _, svc := range services {
		before[svc.name] = imageID(engine, svc.image)
	}

	pullArgs := append([]string{}, composeArgs...)
	pullArgs = append(pullArgs, "-f", file, "pull")
	for _, svc := range services {
		pullArgs = append(pullArgs, svc.name)
	}
	if *verbose {
		fmt.Printf("  running %s\n", strings.Join(pullArgs, " "))
	}
	out, err := runCmd(dir, pullArgs)
	if err != nil {
		return result{}, fmt.Errorf("pull: %w", err)
	}
	if *verbose && out != "" {
		fmt.Println(out)
	}

	var res result
	var recreate []string
	for _, svc := range services {
		after := imageID(engine, svc.image)
		switch {
		case before[svc.name] == "" && after != "":
			res.added++
			recreate = append(recreate, svc.name)
			fmt.Printf("  [added]      %-20s %s\n", svc.name, svc.image)
		case before[svc.name] != "" && after != before[svc.name]:
			res.changed++
			recreate = append(recreate, svc.name)
			fmt.Printf("  [updated]    %-20s %s (%s -> %s)\n",
				svc.name, svc.image, shortID(before[svc.name]), shortID(after))
		default:
			res.unchanged++
			if *verbose {
				fmt.Printf("  [current]    %-20s %s\n", svc.name, svc.image)
			}
		}
	}

	if len(recreate) > 0 {
		upArgs := append([]string{}, composeArgs...)
		upArgs = append(upArgs, "-f", file, "up", "-d")
		if s.ForceRecreate {
			upArgs = append(upArgs, "--force-recreate")
		}
		upArgs = append(upArgs, recreate...)
		if *verbose {
			fmt.Printf("  running %s\n", strings.Join(upArgs, " "))
		}
		out, err := runCmd(dir, upArgs)
		if err != nil {
			return res, fmt.Errorf("recreate: %w", err)
		}
		if *verbose && out != "" {
			fmt.Println(out)
		}
	}

	if s.Prune && res.changed+res.added > 0 {
		pruneArgs := []string{engine, "image", "prune", "-a", "-f"}
		if *verbose {
			fmt.Printf("  running %s\n", strings.Join(pruneArgs, " "))
		}
		out, err := runCmd(dir, pruneArgs)
		if err != nil {
			return res, fmt.Errorf("prune: %w", err)
		}
		if *verbose && out != "" {
			fmt.Println(out)
		}
		fmt.Println("  pruned unused images")
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
	// Keep the prompt for interactive runs (e.g. double-click on Windows),
	// but don't block cron/systemd jobs or piped output.
	if isCharDevice(os.Stdin) && isCharDevice(os.Stdout) {
		fmt.Println("\nNhấn Enter để thoát...")
		fmt.Scanln()
	}
	os.Exit(code)
}

func isCharDevice(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
