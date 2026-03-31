/*
Copyright 2025 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package spdx

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/sirupsen/logrus"
)

const (
	githubAPIBaseURL = "https://api.github.com"
	maxLogFileSize   = 100 * 1024 * 1024 // 100MB
)

// ObservedDependency is a tool or package detected from CI log output.
type ObservedDependency struct {
	Name           string // dependency name
	Version        string // detected version string
	Type           string // "tool", "package", "docker-image"
	InstallCommand string // the actual command/line that triggered detection
	Source         string // "setup-action", "apt-get", "pip", "npm", "go-install", "docker-pull"
	StepName       string // which step in the log
	JobName        string // which job in the log
}

// WorkflowRun holds basic metadata for a completed GitHub Actions run.
type WorkflowRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HeadBranch string `json:"head_branch"`
	CreatedAt  string `json:"created_at"`
}

type workflowRunsResponse struct {
	TotalCount   int           `json:"total_count"`
	WorkflowRuns []WorkflowRun `json:"workflow_runs"`
}

// RunLogFetcher downloads workflow run logs from the GitHub Actions API.
type RunLogFetcher struct {
	client  *http.Client
	token   string
	baseURL string // defaults to githubAPIBaseURL
}

// NewRunLogFetcher creates a RunLogFetcher with the given options.
// If no token is provided, GITHUB_TOKEN is used from the environment.
func NewRunLogFetcher(opts ...GitHubContentFetcherOption) *RunLogFetcher {
	f := &RunLogFetcher{
		client:  &http.Client{Timeout: defaultFetchTimeout},
		baseURL: githubAPIBaseURL,
	}
	// Reuse the same option functions by applying them through an adapter.
	adapter := &GitHubContentFetcher{
		client:  f.client,
		baseURL: f.baseURL,
	}
	for _, opt := range opts {
		opt(adapter)
	}
	f.client = adapter.client
	f.token = adapter.token
	f.baseURL = adapter.baseURL
	// Restore default if WithBaseURL was not called (adapter starts with rawGitHubBaseURL).
	if f.baseURL == rawGitHubBaseURL {
		f.baseURL = githubAPIBaseURL
	}
	if f.token == "" {
		f.token = os.Getenv("GITHUB_TOKEN")
	}
	return f
}

// FetchRunLog downloads the zip of logs for a workflow run and returns a map
// of zip entry name → raw log bytes.
func (f *RunLogFetcher) FetchRunLog(ctx context.Context, owner, repo string, runID int64) (map[string][]byte, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d/logs", f.baseURL, owner, repo, runID)
	logrus.Debugf("Fetching run logs from %s", url)

	// Use a client that does NOT auto-follow redirects so we can capture the 302 Location.
	noRedirectClient := &http.Client{
		Timeout: f.client.Timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating log request: %w", err)
	}
	f.setAuthHeader(req)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := noRedirectClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching logs redirect: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, &FileNotFoundError{URL: url}
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden {
		return nil, &RateLimitError{StatusCode: resp.StatusCode}
	}

	location := resp.Header.Get("Location")
	if location == "" {
		return nil, fmt.Errorf("no redirect location for logs URL %s (HTTP %d)", url, resp.StatusCode)
	}

	// Follow the redirect to the actual zip download URL.
	dlReq, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return nil, fmt.Errorf("creating download request: %w", err)
	}
	f.setAuthHeader(dlReq)

	dlResp, err := f.client.Do(dlReq)
	if err != nil {
		return nil, fmt.Errorf("downloading log zip: %w", err)
	}
	defer dlResp.Body.Close()

	if dlResp.StatusCode < 200 || dlResp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d downloading log zip from %s", dlResp.StatusCode, location)
	}

	limited := io.LimitReader(dlResp.Body, maxLogFileSize)
	zipData, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("reading log zip: %w", err)
	}

	return readZipToMap(zipData)
}

// ListRecentRuns returns up to count completed workflow runs for the repository.
func (f *RunLogFetcher) ListRecentRuns(ctx context.Context, owner, repo string, count int) ([]WorkflowRun, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/actions/runs?per_page=%d&status=completed", f.baseURL, owner, repo, count)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating runs request: %w", err)
	}
	f.setAuthHeader(req)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing workflow runs: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, &FileNotFoundError{URL: url}
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden {
		return nil, &RateLimitError{StatusCode: resp.StatusCode}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d listing workflow runs", resp.StatusCode)
	}

	var result workflowRunsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding workflow runs response: %w", err)
	}
	return result.WorkflowRuns, nil
}

func (f *RunLogFetcher) setAuthHeader(req *http.Request) {
	if f.token != "" {
		req.Header.Set("Authorization", "token "+f.token)
	}
}

// readZipToMap reads a zip archive from raw bytes and returns a map of filename → content.
func readZipToMap(data []byte) (map[string][]byte, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("opening log zip: %w", err)
	}

	result := make(map[string][]byte, len(r.File))
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("opening zip entry %s: %w", f.Name, err)
		}
		limited := io.LimitReader(rc, maxLogFileSize)
		content, err := io.ReadAll(limited)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("reading zip entry %s: %w", f.Name, err)
		}
		result[f.Name] = content
	}
	return result, nil
}

// ── Log Parser ────────────────────────────────────────────────────────────────

var (
	// setup-* action success lines
	setupGoRe     = regexp.MustCompile(`Successfully set up Go version\s+(\S+)`)
	setupNodeRe   = regexp.MustCompile(`Successfully set up Node\.js version\s+(\S+)`)
	setupPythonRe = regexp.MustCompile(`Successfully set up (?:CPython|PyPy)\s+\(?([^)]+)\)?`)
	setupJavaRe   = regexp.MustCompile(`Successfully set up (?:OpenJDK|JDK|Temurin)\s+(\S+)`)
	setupRubyRe   = regexp.MustCompile(`ruby\s+(\d+\.\d+\.\d+\S*)\s+`)

	// apt-get install command and dpkg "Setting up" lines
	aptGetInstallRe = regexp.MustCompile(`(?:apt-get|apt)\s+install\s+(?:-\S+\s+)*(.+)`)
	aptSettingUpRe  = regexp.MustCompile(`Setting up\s+(\S+)\s+\(([^)]+)\)`)

	// pip install command and success message
	pipInstallRe   = regexp.MustCompile(`(?:pip|pip3)\s+install\s+(.+)`)
	pipInstalledRe = regexp.MustCompile(`Successfully installed\s+(.+)`)

	// npm package count line
	npmInstallRe = regexp.MustCompile(`added\s+(\d+)\s+packages?`)

	// go install and go module download lines
	goInstallRe  = regexp.MustCompile(`go install\s+(\S+)@(\S+)`)
	goDownloadRe = regexp.MustCompile(`go: downloading\s+(\S+)\s+v?(\S+)`)

	// docker pull command and image digest
	dockerPullRe   = regexp.MustCompile(`docker pull\s+(\S+)`)
	dockerDigestRe = regexp.MustCompile(`Digest:\s+sha256:([a-f0-9]+)`)
)

// ParseLogForDependencies scans a single log file and returns all detected dependencies.
func ParseLogForDependencies(jobName, stepName string, logContent []byte) []ObservedDependency {
	var deps []ObservedDependency

	scanner := bufio.NewScanner(bytes.NewReader(logContent))
	for scanner.Scan() {
		line := scanner.Text()

		// setup-go
		if m := setupGoRe.FindStringSubmatch(line); m != nil {
			deps = append(deps, ObservedDependency{
				Name: "go", Version: m[1], Type: "tool",
				InstallCommand: line, Source: "setup-action",
				StepName: stepName, JobName: jobName,
			})
		}

		// setup-node
		if m := setupNodeRe.FindStringSubmatch(line); m != nil {
			deps = append(deps, ObservedDependency{
				Name: "node", Version: m[1], Type: "tool",
				InstallCommand: line, Source: "setup-action",
				StepName: stepName, JobName: jobName,
			})
		}

		// setup-python / setup-java / setup-ruby (single capture each)
		if m := setupPythonRe.FindStringSubmatch(line); m != nil {
			deps = append(deps, ObservedDependency{
				Name: "python", Version: strings.TrimSpace(m[1]), Type: "tool",
				InstallCommand: line, Source: "setup-action",
				StepName: stepName, JobName: jobName,
			})
		}

		if m := setupJavaRe.FindStringSubmatch(line); m != nil {
			deps = append(deps, ObservedDependency{
				Name: "java", Version: m[1], Type: "tool",
				InstallCommand: line, Source: "setup-action",
				StepName: stepName, JobName: jobName,
			})
		}

		if m := setupRubyRe.FindStringSubmatch(line); m != nil {
			deps = append(deps, ObservedDependency{
				Name: "ruby", Version: m[1], Type: "tool",
				InstallCommand: line, Source: "setup-action",
				StepName: stepName, JobName: jobName,
			})
		}

		// apt-get install (record the raw command line as a single entry)
		if m := aptGetInstallRe.FindStringSubmatch(line); m != nil {
			deps = append(deps, ObservedDependency{
				Name: strings.TrimSpace(m[1]), Version: "", Type: "package",
				InstallCommand: line, Source: "apt-get",
				StepName: stepName, JobName: jobName,
			})
		}

		// dpkg "Setting up pkg (version)" — extract individual name + version
		if m := aptSettingUpRe.FindStringSubmatch(line); m != nil {
			deps = append(deps, ObservedDependency{
				Name: m[1], Version: m[2], Type: "package",
				InstallCommand: line, Source: "apt-get",
				StepName: stepName, JobName: jobName,
			})
		}

		// pip install command
		if m := pipInstallRe.FindStringSubmatch(line); m != nil {
			deps = append(deps, ObservedDependency{
				Name: strings.TrimSpace(m[1]), Version: "", Type: "package",
				InstallCommand: line, Source: "pip",
				StepName: stepName, JobName: jobName,
			})
		}

		// pip "Successfully installed name-ver name-ver ..." — split into individual packages
		if m := pipInstalledRe.FindStringSubmatch(line); m != nil {
			for _, pair := range strings.Fields(m[1]) {
				// pairs are typically name-version; split on the last '-'
				name, version := splitNameVersion(pair)
				deps = append(deps, ObservedDependency{
					Name: name, Version: version, Type: "package",
					InstallCommand: line, Source: "pip",
					StepName: stepName, JobName: jobName,
				})
			}
		}

		// npm "added N packages"
		if m := npmInstallRe.FindStringSubmatch(line); m != nil {
			deps = append(deps, ObservedDependency{
				Name: fmt.Sprintf("npm-packages(%s)", m[1]), Version: "", Type: "package",
				InstallCommand: line, Source: "npm",
				StepName: stepName, JobName: jobName,
			})
		}

		// go install module@version
		if m := goInstallRe.FindStringSubmatch(line); m != nil {
			deps = append(deps, ObservedDependency{
				Name: m[1], Version: m[2], Type: "tool",
				InstallCommand: line, Source: "go-install",
				StepName: stepName, JobName: jobName,
			})
		}

		// go: downloading module version
		if m := goDownloadRe.FindStringSubmatch(line); m != nil {
			deps = append(deps, ObservedDependency{
				Name: m[1], Version: m[2], Type: "package",
				InstallCommand: line, Source: "go-install",
				StepName: stepName, JobName: jobName,
			})
		}

		// docker pull image
		if m := dockerPullRe.FindStringSubmatch(line); m != nil {
			deps = append(deps, ObservedDependency{
				Name: m[1], Version: extractImageTag(m[1]), Type: "docker-image",
				InstallCommand: line, Source: "docker-pull",
				StepName: stepName, JobName: jobName,
			})
		}

		// docker image digest
		if m := dockerDigestRe.FindStringSubmatch(line); m != nil {
			deps = append(deps, ObservedDependency{
				Name: "docker-image", Version: "sha256:" + m[1], Type: "docker-image",
				InstallCommand: line, Source: "docker-pull",
				StepName: stepName, JobName: jobName,
			})
		}
	}

	return deps
}

// ParseRunLogs iterates over all log files, extracts job/step names from the
// file path keys (format: "jobname/N_stepname.txt"), and returns a deduplicated
// list of observed dependencies.
func ParseRunLogs(logFiles map[string][]byte) []ObservedDependency {
	seen := map[string]bool{}
	var all []ObservedDependency

	for filePath, content := range logFiles {
		jobName, stepName := parseLogFilePath(filePath)
		deps := ParseLogForDependencies(jobName, stepName, content)
		for _, d := range deps {
			key := d.Source + ":" + d.Name + "@" + d.Version
			if seen[key] {
				continue
			}
			seen[key] = true
			all = append(all, d)
		}
	}
	return all
}

// parseLogFilePath extracts job and step names from a zip entry path.
// Expected format: "jobname/step_number_step_name.txt"
func parseLogFilePath(filePath string) (jobName, stepName string) {
	// Split on the first slash to separate job dir from step file.
	parts := strings.SplitN(filePath, "/", 2)
	if len(parts) == 0 {
		return filePath, ""
	}
	jobName = parts[0]
	if len(parts) < 2 {
		return jobName, ""
	}

	// Step file name: "3_Set up Go.txt" → strip extension and leading "N_"
	name := strings.TrimSuffix(parts[1], ".txt")
	if idx := strings.Index(name, "_"); idx != -1 {
		name = name[idx+1:]
	}
	stepName = name
	return jobName, stepName
}

// splitNameVersion splits a pip "name-version" token on the last hyphen.
func splitNameVersion(s string) (name, version string) {
	idx := strings.LastIndex(s, "-")
	if idx == -1 {
		return s, ""
	}
	return s[:idx], s[idx+1:]
}

// ── SPDX Integration ─────────────────────────────────────────────────────────

// RunLogsToSPDXPackages downloads run logs, parses observed dependencies, and
// returns an SPDX Package wrapping them all as build dependencies.
func RunLogsToSPDXPackages(ctx context.Context, owner, repo string, runID int64, fetcher *RunLogFetcher) (*Package, error) {
	logFiles, err := fetcher.FetchRunLog(ctx, owner, repo, runID)
	if err != nil {
		return nil, fmt.Errorf("fetching run logs for %s/%s run %d: %w", owner, repo, runID, err)
	}

	deps := ParseRunLogs(logFiles)
	logrus.Infof("Extracted %d observed build dependencies from run logs", len(deps))

	buildPkg := NewPackage()
	buildPkg.Options().Prefix = "build"
	buildPkg.Name = "observed-build-dependencies"
	buildPkg.BuildID("observed-build-dependencies")
	buildPkg.DownloadLocation = "NOASSERTION"
	buildPkg.LicenseConcluded = "NOASSERTION"
	buildPkg.PrimaryPurpose = "OTHER"

	for i := range deps {
		dep := &deps[i]
		pkg := observedDepToSPDXPackage(dep)
		buildPkg.AddRelationship(&Relationship{
			FullRender: true,
			Type:       BUILD_DEPENDENCY_OF,
			Peer:       pkg,
		})
	}

	return buildPkg, nil
}

// observedDepToSPDXPackage converts an ObservedDependency into an SPDX Package.
func observedDepToSPDXPackage(dep *ObservedDependency) *Package {
	pkg := NewPackage()
	pkg.Options().Prefix = "observed-" + dep.Type
	pkg.Name = dep.Name
	pkg.Version = dep.Version
	pkg.DownloadLocation = "NOASSERTION"
	pkg.LicenseConcluded = "NOASSERTION"
	// Note it was observed from run logs rather than declared in workflow YAML.
	pkg.Comment = fmt.Sprintf("Observed from run logs (source: %s, job: %s, step: %s)", dep.Source, dep.JobName, dep.StepName)

	pkg.BuildID(dep.Name, dep.Version)
	return pkg
}
