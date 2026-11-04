package github

import (
	"context"
	"net/http"
)

// A Download is one runner application package in the downloads
// endpoint's answer, as GitHub serves it, for the runner version rules
// to choose from.
type Download struct {
	// OS and Arch are as GitHub names them: "linux", "win" or "osx", and
	// "x64", "arm64" or "arm".
	OS, Arch string
	// URL is the package's download URL, and Filename its file name,
	// which carries the version.
	URL, Filename string
	// SHA256 is the package's SHA-256, in hex.
	SHA256 string
}

// Downloads reads the runner application packages GitHub serves t: its
// downloads endpoint, which picks a version for t during GitHub's
// progressive rollout. The list may be empty.
func (c *Client) Downloads(ctx context.Context, t Target) ([]Download, error) {
	var got []struct {
		OS       string `json:"os"`
		Arch     string `json:"architecture"`
		URL      string `json:"download_url"`
		Filename string `json:"filename"`
		SHA256   string `json:"sha256_checksum"`
	}
	if _, err := c.do(ctx, t.scope(), http.MethodGet, t.runners()+"/downloads", nil, &got); err != nil {
		return nil, err
	}
	out := []Download{}
	for _, d := range got {
		out = append(out, Download{OS: d.OS, Arch: d.Arch, URL: d.URL, Filename: d.Filename, SHA256: d.SHA256})
	}
	return out, nil
}

// A Release is a release of actions/runner.
type Release struct {
	// Tag is its tag, such as "v2.338.2".
	Tag    string
	Assets []Asset
}

// An Asset is one file of a release.
type Asset struct {
	Name, URL string
	// Digest is GitHub's digest of the file, such as "sha256:<hex>", or
	// empty if GitHub has none.
	Digest string
}

// LatestRunnerRelease reads the latest release of actions/runner, for a
// target whose served list is empty and for a refused version (ADR
// 0011).
func (c *Client) LatestRunnerRelease(ctx context.Context) (Release, error) {
	var got struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if _, err := c.do(ctx, metadataOnly, http.MethodGet, "/repos/actions/runner/releases/latest", nil, &got); err != nil {
		return Release{}, err
	}
	r := Release{Tag: got.Tag, Assets: []Asset{}}
	for _, a := range got.Assets {
		r.Assets = append(r.Assets, Asset{Name: a.Name, URL: a.URL, Digest: a.Digest})
	}
	return r, nil
}
