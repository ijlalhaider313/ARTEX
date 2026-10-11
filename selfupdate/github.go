package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Repo is the release source. Hardcoded rather than configurable: a configurable
// update source would give anyone who can edit config a remote code execution
// channel — for a penetration testing platform, that's an opening that must not exist.
const Repo = "Hinln/ARTEX"

// GitHubTokenEnv is only read by the backend process; the token is never passed
// through the frontend or config API.
const GitHubTokenEnv = "ARTEX_UPDATE_GITHUB_TOKEN"

// latestURL is GitHub's "latest stable release" endpoint. It automatically skips
// prereleases and drafts.
const latestURL = "https://api.github.com/repos/" + Repo + "/releases/latest"

// allowedHosts restricts the domains the upgrade chain can access. Combined
// with checkRedirect below, any redirect hop to a host outside the allowlist
// fails immediately — this is the first gate against DNS poisoning / MITM
// swapping the binary; the second is SHA256SUMS verification.
var allowedHosts = map[string]bool{
	"api.github.com":                       true,
	"github.com":                           true,
	"objects.githubusercontent.com":        true, // object storage where release assets actually land
	"release-assets.githubusercontent.com": true,
	"raw.githubusercontent.com":            true,
}

// Release holds the GitHub Release fields we care about.
type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Assets      []Asset   `json:"assets"`
}

// Asset is a file attached to a Release.
type Asset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	APIURL string `json:"url"`
	Size   int64  `json:"size"`
}

// Private repo assets require authenticated download via the Release Asset API.
func (a Asset) downloadURL() string {
	if a.APIURL != "" {
		return a.APIURL
	}
	return a.URL
}

// setUpdateAuth only sends the token to the fixed release repo's HTTPS API.
// When assets redirect to GitHub object storage they use signed URLs; the
// Authorization header must not be forwarded along.
func setUpdateAuth(req *http.Request) {
	req.Header.Del("Authorization")
	u := req.URL
	if u.Scheme != "https" || !strings.EqualFold(u.Hostname(), "api.github.com") ||
		(u.Port() != "" && u.Port() != "443") || u.User != nil ||
		!strings.HasPrefix(strings.ToLower(u.EscapedPath()), strings.ToLower("/repos/"+Repo+"/releases/")) {
		return
	}
	if token := strings.TrimSpace(os.Getenv(GitHubTokenEnv)); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// NewClient builds an HTTP client that only trusts GitHub domains. If proxy is
// empty it connects directly.
//
// The default Transport is deliberately not reused: the upgrade chain must
// enforce TLS with certificate validation, and must not be affected by an
// InsecureSkipVerify set elsewhere.
func NewClient(proxy string) *http.Client {
	tr := &http.Transport{
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	if p := strings.TrimSpace(proxy); p != "" {
		if pu, err := url.Parse(p); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	return &http.Client{
		Transport: tr,
		Timeout:   30 * time.Minute, // downloading the full package, can't time out at request level
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			if err := checkURL(req.URL); err != nil {
				return err
			}
			setUpdateAuth(req)
			return nil
		},
	}
}

// checkURL enforces https + domain allowlist.
func checkURL(u *url.URL) error {
	if u.Scheme != "https" {
		return fmt.Errorf("non-HTTPS address rejected: %s", u.Scheme+"://"+u.Host)
	}
	if !allowedHosts[strings.ToLower(u.Hostname())] {
		return fmt.Errorf("non-GitHub domain rejected: %s", u.Hostname())
	}
	return nil
}

// FetchLatest queries the latest stable release.
func FetchLatest(ctx context.Context, c *http.Client) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "artex-selfupdate")
	setUpdateAuth(req)

	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to reach GitHub (configure a global proxy in system settings): %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, fmt.Errorf("GitHub authentication failed, please check backend environment variable %s", GitHubTokenEnv)
	case resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("GitHub denied access: check the token's repo Contents read permission or API quota")
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("GitHub API rate limited, please try again later")
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("repo %s is inaccessible or has no stable release yet; for private repos set %s in the backend (requires Contents read permission)", Repo, GitHubTokenEnv)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub returned %d", resp.StatusCode)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("failed to parse Release: %w", err)
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return nil, fmt.Errorf("Release missing tag")
	}
	return &rel, nil
}

// AssetName returns the release package name for the current platform, matching
// build.sh's package_binary:
// artex-<version>-<os>-<arch>.zip (version without the v prefix).
func AssetName(tag, goos, goarch string) string {
	return fmt.Sprintf("artex-%s-%s-%s.zip", strings.TrimPrefix(tag, "v"), goos, goarch)
}

// FindAsset finds an asset by name in the Release.
func (r *Release) FindAsset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if strings.EqualFold(a.Name, name) {
			return a, true
		}
	}
	return Asset{}, false
}
