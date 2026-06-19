// mesh-firmware-packager
//
// A Go program to download the latest *stable* firmware releases from the
// Meshtastic and MeshCore (meshcore.io) GitHub repositories, filter to
// firmware binaries only, and package them into convenient per-project zip
// archives containing a JSON manifest.
//
// It deliberately skips pre-releases / alphas (common for Meshtastic) by
// using GitHub's prerelease flag and grouping logic for coordinated releases
// like MeshCore's companion/repeater/room-server.
//
// Usage:
//   export GITHUB_TOKEN=ghp_xxx          # highly recommended (5000 req/h vs 60)
//   go run main.go                       # outputs meshtastic-firmware-*.zip and meshcore-firmware-*.zip in current dir
//   go run main.go -out /tmp/firmwares
//
// Build a binary:
//   go build -o mesh-fw-packager .
//
// The manifest.json inside each zip lists every firmware file with SHA256,
// original download URL, size, etc. for verification / auditing.
//
// Only depends on Go standard library. Tested with Go 1.21+.

package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// GitHubRelease mirrors the minimal fields we need from the GitHub Releases API.
type GitHubRelease struct {
	ID          int64     `json:"id"`
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Prerelease  bool      `json:"prerelease"`
	Draft       bool      `json:"draft"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Assets      []Asset   `json:"assets"`
}

// Asset describes a release asset (firmware binary, etc.).
type Asset struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
	ContentType        string `json:"content_type"`
}

// Manifest is written as manifest.json inside each generated zip.
type Manifest struct {
	Project       string          `json:"project"`
	Version       string          `json:"version"`
	Tags          []string        `json:"tags"`
	ReleaseURL    string          `json:"release_url"`
	PublishedAt   time.Time       `json:"published_at"`
	GeneratedAt   time.Time       `json:"generated_at"`
	TotalFiles    int             `json:"total_files"`
	TotalSize     int64           `json:"total_size_bytes"`
	FirmwareFiles []FirmwareFile  `json:"firmware_files"`
}

// FirmwareFile entry inside the manifest.
type FirmwareFile struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	DownloadURL string `json:"download_url"`
	ContentType string `json:"content_type"`
}

// downloadedFile holds a successfully downloaded asset + its local path and hash.
type downloadedFile struct {
	Asset     Asset
	LocalPath string
	SHA256    string
}

// Project defines one firmware source we care about.
type Project struct {
	Owner string
	Repo  string
	Slug  string // used in output filename, e.g. "meshtastic"
	Name  string // human name, e.g. "Meshtastic"
}

var projects = []Project{
	{Owner: "meshtastic", Repo: "firmware", Slug: "meshtastic", Name: "Meshtastic"},
	{Owner: "meshcore-dev", Repo: "MeshCore", Slug: "meshcore", Name: "MeshCore"},
}

func main() {
	outDir := flag.String("out", ".", "Directory to write the generated firmware zips")
	token := flag.String("token", "", "GitHub token (or set GITHUB_TOKEN / GH_TOKEN env var)")
	help := flag.Bool("help", false, "Show usage")
	flag.Parse()

	if *help {
		flag.Usage()
		fmt.Println("\nExample:\n  GITHUB_TOKEN=ghp_... go run main.go -out ./firmwares")
		return
	}

	if *token == "" {
		*token = os.Getenv("GITHUB_TOKEN")
		if *token == "" {
			*token = os.Getenv("GH_TOKEN")
		}
	}

	if *token == "" {
		fmt.Println("WARNING: No GitHub token provided. You may hit rate limits (60 req/h).")
		fmt.Println("         Set GITHUB_TOKEN env var or use -token for best results.")
		fmt.Println()
	}

	if err := os.MkdirAll(*outDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create output dir: %v\n", err)
		os.Exit(1)
	}

	for _, p := range projects {
		if err := processProject(p, *token, *outDir); err != nil {
			fmt.Fprintf(os.Stderr, "Error processing %s: %v\n", p.Name, err)
			// continue with other projects
		}
	}

	fmt.Println("\nDone. Zips contain firmware binaries + manifest.json")
}

// processProject fetches latest stable release(s), downloads firmware assets,
// and builds a single zip + manifest for the project.
func processProject(p Project, token, outDir string) error {
	fmt.Printf("\n=== Processing %s (%s/%s) ===\n", p.Name, p.Owner, p.Repo)

	releases, err := fetchReleases(p.Owner, p.Repo, token)
	if err != nil {
		return fmt.Errorf("fetch releases: %w", err)
	}

	// Filter to non-draft, non-prerelease (stable)
	var stable []GitHubRelease
	for _, r := range releases {
		if !r.Draft && !r.Prerelease {
			stable = append(stable, r)
		}
	}
	if len(stable) == 0 {
		return fmt.Errorf("no stable (non-prerelease, non-draft) releases found")
	}

	// Group by version key so MeshCore's companion/repeater/room-server vX.Y.Z
	// releases (published minutes apart) are collected together.
	groups := groupReleasesByVersion(stable)
	if len(groups) == 0 {
		return fmt.Errorf("no version groups found")
	}

	// Pick the group with the most recent publish time (latest stable)
	bestKey, bestGroup := pickLatestGroup(groups)
	fmt.Printf("Latest stable version group: %s (includes %d release(s))\n", bestKey, len(bestGroup.releases))

	// Collect firmware-looking assets across the releases in the group (dedup by name)
	assets := collectFirmwareAssets(bestGroup.releases, p.Slug)
	if len(assets) == 0 {
		return fmt.Errorf("no firmware assets found in latest stable release(s)")
	}
	fmt.Printf("Found %d firmware assets (total ~%.1f MB)\n", len(assets), float64(sumAssetSizes(assets))/1024/1024)

	// Prepare temp dir for downloads + manifest
	tempDir, err := os.MkdirTemp("", "firmware-pkg-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	// Download assets (with SHA256)
	var downloaded []downloadedFile
	var totalSize int64

	for i, a := range assets {
		fmt.Printf("  [%3d/%d] %s (%.1f MB)\n", i+1, len(assets), a.Name, float64(a.Size)/1024/1024)
		local := filepath.Join(tempDir, sanitizeFileName(a.Name))
		sha, err := downloadAsset(a.BrowserDownloadURL, local, token)
		if err != nil {
			fmt.Printf("    WARNING: failed to download %s: %v (skipping)\n", a.Name, err)
			continue
		}
		downloaded = append(downloaded, downloadedFile{Asset: a, LocalPath: local, SHA256: sha})
		totalSize += a.Size
	}

	if len(downloaded) == 0 {
		return fmt.Errorf("all downloads failed")
	}

	// Build manifest
	tags := make([]string, 0, len(bestGroup.releases))
	var primaryURL string
	var primaryPub time.Time
	for _, r := range bestGroup.releases {
		tags = append(tags, r.TagName)
		if r.PublishedAt.After(primaryPub) {
			primaryPub = r.PublishedAt
			primaryURL = r.HTMLURL
		}
	}
	sort.Strings(tags)

	manifest := Manifest{
		Project:     p.Name,
		Version:     bestKey,
		Tags:        tags,
		ReleaseURL:  primaryURL,
		PublishedAt: primaryPub,
		GeneratedAt: time.Now().UTC(),
		TotalFiles:  len(downloaded),
		TotalSize:   totalSize,
		FirmwareFiles: make([]FirmwareFile, len(downloaded)),
	}
	for i, d := range downloaded {
		manifest.FirmwareFiles[i] = FirmwareFile{
			Name:        d.Asset.Name,
			Size:        d.Asset.Size,
			SHA256:      d.SHA256,
			DownloadURL: d.Asset.BrowserDownloadURL,
			ContentType: d.Asset.ContentType,
		}
	}

	manifestPath := filepath.Join(tempDir, "manifest.json")
	mb, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(manifestPath, mb, 0644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

	// Create the final zip
	versionForFile := strings.ReplaceAll(bestKey, ".", "-")
	zipName := fmt.Sprintf("%s-firmware-%s.zip", p.Slug, versionForFile)
	zipPath := filepath.Join(outDir, zipName)

	if err := createZip(zipPath, manifestPath, downloaded); err != nil {
		return fmt.Errorf("create zip: %w", err)
	}

	fmt.Printf("✓ Created %s (%d files, %.1f MB)\n", zipPath, len(downloaded), float64(totalSize)/1024/1024)
	return nil
}

// fetchReleases calls the GitHub Releases API (newest first).
func fetchReleases(owner, repo, token string) ([]GitHubRelease, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases?per_page=30", owner, repo)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "mesh-firmware-packager/1.0")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 403 || resp.StatusCode == 429 {
		body, _ := io.ReadAll(resp.Body)
		if strings.Contains(string(body), "rate limit") {
			return nil, fmt.Errorf("GitHub rate limit exceeded. Provide a GITHUB_TOKEN for 5000 req/h")
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %s", resp.Status)
	}

	var releases []GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("decode releases JSON: %w", err)
	}
	return releases, nil
}

// groupReleasesByVersion groups stable releases so that MeshCore's three
// simultaneous releases (companion/repeater/room-server vX.Y.Z) end up together.
func groupReleasesByVersion(releases []GitHubRelease) map[string]*releaseGroup {
	re := regexp.MustCompile(`(?:companion-|repeater-|room-server-)?v?(\d+\.\d+(?:\.\d+)?(?:[-.][0-9a-fA-F]+)?)`)
	groups := make(map[string]*releaseGroup)

	for _, r := range releases {
		m := re.FindStringSubmatch(r.TagName)
		if len(m) < 2 {
			continue
		}
		key := m[1]
		g := groups[key]
		if g == nil {
			g = &releaseGroup{}
			groups[key] = g
		}
		g.releases = append(g.releases, r)
		if r.PublishedAt.After(g.maxPub) {
			g.maxPub = r.PublishedAt
		}
	}
	return groups
}

type releaseGroup struct {
	releases []GitHubRelease
	maxPub   time.Time
}

func pickLatestGroup(groups map[string]*releaseGroup) (string, *releaseGroup) {
	var bestKey string
	var best *releaseGroup
	for k, g := range groups {
		if best == nil || g.maxPub.After(best.maxPub) {
			best = g
			bestKey = k
		}
	}
	return bestKey, best
}

func collectFirmwareAssets(releases []GitHubRelease, projectSlug string) []Asset {
	seen := make(map[string]bool)
	var out []Asset
	for _, r := range releases {
		for _, a := range r.Assets {
			if isFirmwareAsset(a.Name, projectSlug) && !seen[a.Name] {
				seen[a.Name] = true
				out = append(out, a)
			}
		}
	}
	// Sort for deterministic output
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func isFirmwareAsset(name string, projectSlug string) bool {
	lower := strings.ToLower(name)
	// Exclude obvious non-firmware / metadata
	if strings.Contains(lower, "source") ||
		strings.Contains(lower, "checksum") ||
		strings.Contains(lower, "debug") ||
		strings.HasSuffix(lower, ".txt") ||
		strings.HasSuffix(lower, ".md") ||
		strings.HasSuffix(lower, ".asc") ||
		strings.HasSuffix(lower, ".sig") ||
		strings.HasSuffix(lower, ".json") {
		return false
	}

	// For Meshtastic: user wants only the ~8 flushable firmware zip packages
	if projectSlug == "meshtastic" {
		return strings.HasSuffix(lower, ".zip") && strings.Contains(lower, "firmware")
	}

	// For MeshCore (and others): keep binaries + any relevant zips
	for _, ext := range []string{".bin", ".uf2", ".elf", ".hex", ".dfu", ".zip"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

func sumAssetSizes(assets []Asset) int64 {
	var s int64
	for _, a := range assets {
		s += a.Size
	}
	return s
}

// downloadAsset streams the asset to disk while computing SHA256.
func downloadAsset(url, dest, token string) (string, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "mesh-firmware-packager/1.0")
	// Token is optional for public release assets but harmless
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{
		Timeout: 120 * time.Second, // allow for slower connections on large files
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %s", resp.Status)
	}

	out, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	defer out.Close()

	hasher := sha256.New()
	mw := io.MultiWriter(out, hasher)

	if _, err := io.Copy(mw, resp.Body); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func sanitizeFileName(name string) string {
	// Very conservative: replace anything that could be problematic in a filename
	repl := strings.NewReplacer("/", "-", "\\", "-", ":", "-", "*", "-", "?", "-", "\"", "-", "<", "-", ">", "-", "|", "-")
	return repl.Replace(name)
}

// createZip builds the final archive containing manifest.json + all firmware files (flat).
func createZip(zipPath, manifestPath string, downloaded []downloadedFile) error {
	f, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	defer zw.Close()

	// Add manifest
	if err := addToZip(zw, manifestPath, "manifest.json"); err != nil {
		return err
	}

	// Add firmware files (flat at root of zip)
	for _, d := range downloaded {
		if err := addToZip(zw, d.LocalPath, d.Asset.Name); err != nil {
			return fmt.Errorf("add %s to zip: %w", d.Asset.Name, err)
		}
	}
	return nil
}

func addToZip(zw *zip.Writer, localPath, entryName string) error {
	info, err := os.Stat(localPath)
	if err != nil {
		return err
	}
	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	header.Name = entryName
	header.Method = zip.Deflate

	w, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}
	src, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer src.Close()

	_, err = io.Copy(w, src)
	return err
}