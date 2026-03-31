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
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"sigs.k8s.io/yaml"
)

const (
	maxResolveDepth      = 10
	rawGitHubBaseURL     = "https://raw.githubusercontent.com"
	defaultFetchTimeout  = 15 * time.Second
	maxConcurrentFetches = 5
	defaultMaxRetries    = 3
	retryBaseDelay       = 500 * time.Millisecond
)

// ActionMetadata represents a parsed action.yml or action.yaml file.
type ActionMetadata struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Runs        ActionRuns             `json:"runs"`
	Inputs      map[string]interface{} `json:"inputs"`
	Outputs     map[string]interface{} `json:"outputs"`
}

// ActionRuns describes how the action executes.
type ActionRuns struct {
	Using string       `json:"using"`
	Steps []ActionStep `json:"steps"`
	Main  string       `json:"main"`
	Pre   string       `json:"pre"`
	Post  string       `json:"post"`
	Image string       `json:"image"`
}

// ActionStep is a single step in a composite action.
type ActionStep struct {
	Name string `json:"name"`
	ID   string `json:"id"`
	Uses string `json:"uses"`
	Run  string `json:"run"`
}

// IsComposite returns true if the action uses composite runs.
func (m *ActionMetadata) IsComposite() bool {
	return m.Runs.Using == "composite"
}

// IsDocker returns true if the action uses a Docker container.
func (m *ActionMetadata) IsDocker() bool {
	return m.Runs.Using == "docker"
}

// GitHubContentFetcher retrieves file contents from GitHub repositories.
type GitHubContentFetcher struct {
	client  *http.Client
	token   string
	cache   map[string][]byte
	cacheMu sync.RWMutex
	baseURL string
}

// GitHubContentFetcherOption configures a GitHubContentFetcher.
type GitHubContentFetcherOption func(*GitHubContentFetcher)

// WithToken sets the GitHub authentication token.
func WithToken(token string) GitHubContentFetcherOption {
	return func(f *GitHubContentFetcher) {
		f.token = token
	}
}

// WithHTTPClient sets a custom HTTP client (primarily for testing).
func WithHTTPClient(client *http.Client) GitHubContentFetcherOption {
	return func(f *GitHubContentFetcher) {
		f.client = client
	}
}

// WithBaseURL overrides the raw GitHub URL (primarily for testing).
func WithBaseURL(url string) GitHubContentFetcherOption {
	return func(f *GitHubContentFetcher) {
		f.baseURL = url
	}
}

// NewGitHubContentFetcher creates a fetcher with the given options.
// If no token is provided, it checks the GITHUB_TOKEN environment variable.
func NewGitHubContentFetcher(opts ...GitHubContentFetcherOption) *GitHubContentFetcher {
	f := &GitHubContentFetcher{
		client:  &http.Client{Timeout: defaultFetchTimeout},
		cache:   make(map[string][]byte),
		baseURL: rawGitHubBaseURL,
	}
	for _, opt := range opts {
		opt(f)
	}
	if f.token == "" {
		f.token = os.Getenv("GITHUB_TOKEN")
	}
	return f
}

// FetchFile retrieves a file from a GitHub repository at a specific ref.
// URL format: https://raw.githubusercontent.com/{owner}/{repo}/{ref}/{path}
func (f *GitHubContentFetcher) FetchFile(ctx context.Context, owner, repo, ref, path string) ([]byte, error) {
	cacheKey := fmt.Sprintf("%s/%s/%s/%s", owner, repo, ref, path)

	f.cacheMu.RLock()
	if data, ok := f.cache[cacheKey]; ok {
		f.cacheMu.RUnlock()
		return data, nil
	}
	f.cacheMu.RUnlock()

	url := fmt.Sprintf("%s/%s/%s/%s/%s", f.baseURL, owner, repo, ref, path)
	logrus.Debugf("Fetching %s", url)

	var lastErr error
	for attempt := range defaultMaxRetries {
		data, err := f.doFetch(ctx, url)
		if err == nil {
			f.cacheMu.Lock()
			f.cache[cacheKey] = data
			f.cacheMu.Unlock()
			return data, nil
		}
		lastErr = err
		if !isRetryable(err) {
			return nil, fmt.Errorf("fetching %s: %w", cacheKey, err)
		}
		delay := retryBaseDelay * time.Duration(1<<uint(attempt))
		logrus.Debugf("Retry %d/%d for %s after %v: %v", attempt+1, defaultMaxRetries, cacheKey, delay, err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, fmt.Errorf("fetching %s after %d retries: %w", cacheKey, defaultMaxRetries, lastErr)
}

func (f *GitHubContentFetcher) doFetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	if f.token != "" {
		req.Header.Set("Authorization", "token "+f.token)
	}
	req.Header.Set("Accept", "application/vnd.github.v3.raw")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, &FileNotFoundError{URL: url}
	}

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden {
		return nil, &RateLimitError{StatusCode: resp.StatusCode}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	return data, nil
}

// FileNotFoundError indicates the requested file was not found (404).
type FileNotFoundError struct {
	URL string
}

func (e *FileNotFoundError) Error() string {
	return fmt.Sprintf("file not found: %s", e.URL)
}

// RateLimitError indicates a rate limit was hit.
type RateLimitError struct {
	StatusCode int
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limited (HTTP %d)", e.StatusCode)
}

func isRetryable(err error) bool {
	if _, ok := err.(*RateLimitError); ok {
		return true
	}
	return false
}

// FetchActionMetadata fetches and parses an action.yml or action.yaml from a GitHub repo.
func (f *GitHubContentFetcher) FetchActionMetadata(ctx context.Context, ref ActionRef) (*ActionMetadata, error) {
	if ref.IsDocker || ref.IsLocal || ref.Owner == "" {
		return nil, fmt.Errorf("cannot fetch metadata for non-GitHub action: %s", ref.Raw)
	}

	path := "action.yml"
	if ref.Path != "" {
		path = ref.Path + "/action.yml"
	}

	data, err := f.FetchFile(ctx, ref.Owner, ref.Repo, ref.Ref, path)
	if err != nil {
		if _, ok := err.(*FileNotFoundError); ok {
			yamlPath := strings.Replace(path, "action.yml", "action.yaml", 1)
			data, err = f.FetchFile(ctx, ref.Owner, ref.Repo, ref.Ref, yamlPath)
			if err != nil {
				return nil, fmt.Errorf("fetching action metadata for %s: %w", ref.FullName(), err)
			}
		} else {
			return nil, fmt.Errorf("fetching action metadata for %s: %w", ref.FullName(), err)
		}
	}

	return ParseActionMetadata(data)
}

// ParseActionMetadata parses action.yml bytes into ActionMetadata.
func ParseActionMetadata(data []byte) (*ActionMetadata, error) {
	meta := &ActionMetadata{}
	if err := yaml.Unmarshal(data, meta); err != nil {
		return nil, fmt.Errorf("parsing action metadata YAML: %w", err)
	}
	return meta, nil
}

// FetchReusableWorkflow fetches and parses a reusable workflow from a GitHub repo.
func (f *GitHubContentFetcher) FetchReusableWorkflow(ctx context.Context, ref ActionRef) (*Workflow, error) {
	if ref.Owner == "" || ref.Repo == "" || ref.Path == "" {
		return nil, fmt.Errorf("invalid reusable workflow reference: %s", ref.Raw)
	}

	data, err := f.FetchFile(ctx, ref.Owner, ref.Repo, ref.Ref, ref.Path)
	if err != nil {
		return nil, fmt.Errorf("fetching reusable workflow %s: %w", ref.FullName(), err)
	}

	return ParseWorkflowData(data)
}

// DependencyResolver walks the dependency tree of GitHub Actions workflows,
// resolving composite action and reusable workflow dependencies transitively.
type DependencyResolver struct {
	fetcher  *GitHubContentFetcher
	maxDepth int
	seen     map[string]bool
	mu       sync.Mutex
}

// NewDependencyResolver creates a resolver with the given fetcher.
func NewDependencyResolver(fetcher *GitHubContentFetcher) *DependencyResolver {
	return &DependencyResolver{
		fetcher:  fetcher,
		maxDepth: maxResolveDepth,
		seen:     make(map[string]bool),
	}
}

// ResolvedDependency extends WorkflowDependency with resolution metadata.
type ResolvedDependency struct {
	WorkflowDependency
	Depth       int
	ResolvedVia string
	Transitive  bool
}

// ResolveWorkflowDependencies takes the direct dependencies extracted from local
// workflow files and resolves their transitive dependencies by fetching action.yml
// files and reusable workflow definitions from GitHub.
func (r *DependencyResolver) ResolveWorkflowDependencies(
	ctx context.Context, directDeps []WorkflowDependency,
) ([]ResolvedDependency, error) {
	var allResolved []ResolvedDependency

	for i := range directDeps {
		dep := &directDeps[i]
		resolved := ResolvedDependency{
			WorkflowDependency: *dep,
			Depth:              0,
			Transitive:         false,
		}
		allResolved = append(allResolved, resolved)

		transitive, err := r.resolveTransitive(ctx, dep, 1)
		if err != nil {
			logrus.Warnf("Could not resolve transitive deps for %s: %v", dep.Name, err)
			continue
		}
		allResolved = append(allResolved, transitive...)
	}

	return allResolved, nil
}

func (r *DependencyResolver) resolveTransitive(
	ctx context.Context, dep *WorkflowDependency, depth int,
) ([]ResolvedDependency, error) {
	if depth > r.maxDepth {
		logrus.Warnf("Max resolve depth (%d) reached for %s", r.maxDepth, dep.Name)
		return nil, nil
	}

	seenKey := dep.Type + ":" + dep.Name + "@" + dep.Version
	r.mu.Lock()
	if r.seen[seenKey] {
		r.mu.Unlock()
		return nil, nil
	}
	r.seen[seenKey] = true
	r.mu.Unlock()

	switch dep.Type {
	case "action":
		return r.resolveAction(ctx, dep, depth)
	case "reusable-workflow":
		return r.resolveReusableWorkflow(ctx, dep, depth)
	default:
		return nil, nil
	}
}

func (r *DependencyResolver) resolveAction(
	ctx context.Context, dep *WorkflowDependency, depth int,
) ([]ResolvedDependency, error) {
	ref := ParseActionRef(dep.Name + "@" + dep.Version)
	if ref.Owner == "" {
		return nil, nil
	}

	meta, err := r.fetcher.FetchActionMetadata(ctx, ref)
	if err != nil {
		if _, ok := err.(*FileNotFoundError); ok {
			logrus.Debugf("No action.yml found for %s (likely JavaScript/Docker action)", dep.Name)
			return nil, nil
		}
		return nil, fmt.Errorf("fetching action metadata: %w", err)
	}

	var resolved []ResolvedDependency

	if meta.IsComposite() {
		for _, step := range meta.Runs.Steps {
			if step.Uses == "" {
				continue
			}
			stepRef := ParseActionRef(step.Uses)
			if stepRef.IsLocal {
				continue
			}

			childDep := WorkflowDependency{
				Name:             stepRef.FullName(),
				Version:          stepRef.Ref,
				DownloadLocation: stepRef.DownloadLocation(),
				PURL:             stepRef.PackageURL(),
				SourceJob:        dep.SourceJob,
				SourceStep:       step.Name,
			}

			if stepRef.IsDocker {
				childDep.Name = stepRef.Image
				childDep.Version = extractImageTag(stepRef.Image)
				childDep.Type = "docker-image"
				childDep.PrimaryPurpose = "CONTAINER"
				childDep.RelationshipType = BUILD_DEPENDENCY_OF
			} else {
				childDep.Type = "action"
				childDep.RelationshipType = BUILD_TOOL_OF
			}

			childDep.Comment = fmt.Sprintf("Transitive dependency via composite action %s", dep.Name)

			resolved = append(resolved, ResolvedDependency{
				WorkflowDependency: childDep,
				Depth:              depth,
				ResolvedVia:        dep.Name,
				Transitive:         true,
			})

			grandchildren, err := r.resolveTransitive(ctx, &childDep, depth+1)
			if err != nil {
				logrus.Warnf("Could not resolve transitive deps for %s: %v", childDep.Name, err)
				continue
			}
			resolved = append(resolved, grandchildren...)
		}
	}

	if meta.IsDocker() && meta.Runs.Image != "" && !strings.HasPrefix(meta.Runs.Image, "Dockerfile") {
		image := meta.Runs.Image
		if strings.HasPrefix(image, "docker://") {
			image = strings.TrimPrefix(image, "docker://")
		}
		resolved = append(resolved, ResolvedDependency{
			WorkflowDependency: WorkflowDependency{
				Name:             image,
				Version:          extractImageTag(image),
				Type:             "docker-image",
				DownloadLocation: "https://hub.docker.com/_/" + strings.Split(image, ":")[0],
				PURL:             dockerImagePURL(image),
				RelationshipType: BUILD_DEPENDENCY_OF,
				PrimaryPurpose:   "CONTAINER",
				Comment:          fmt.Sprintf("Docker image used by action %s", dep.Name),
			},
			Depth:       depth,
			ResolvedVia: dep.Name,
			Transitive:  true,
		})
	}

	return resolved, nil
}

func (r *DependencyResolver) resolveReusableWorkflow(
	ctx context.Context, dep *WorkflowDependency, depth int,
) ([]ResolvedDependency, error) {
	ref := ParseActionRef(dep.Name + "@" + dep.Version)
	if ref.Owner == "" || ref.Path == "" {
		return nil, nil
	}

	wf, err := r.fetcher.FetchReusableWorkflow(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("fetching reusable workflow: %w", err)
	}

	nestedDeps := ExtractWorkflowDependencies(wf)

	var resolved []ResolvedDependency
	for i := range nestedDeps {
		nd := &nestedDeps[i]
		nd.Comment = fmt.Sprintf("Transitive dependency via reusable workflow %s", dep.Name)

		resolved = append(resolved, ResolvedDependency{
			WorkflowDependency: *nd,
			Depth:              depth,
			ResolvedVia:        dep.Name,
			Transitive:         true,
		})

		grandchildren, err := r.resolveTransitive(ctx, nd, depth+1)
		if err != nil {
			logrus.Warnf("Could not resolve transitive deps for %s: %v", nd.Name, err)
			continue
		}
		resolved = append(resolved, grandchildren...)
	}

	return resolved, nil
}

// WorkflowsToSPDXPackagesResolved is like WorkflowsToSPDXPackages but also
// resolves transitive dependencies via the GitHub API.
func WorkflowsToSPDXPackagesResolved(ctx context.Context, workflowPaths []string, fetcher *GitHubContentFetcher) (*Package, error) {
	buildPkg := NewPackage()
	buildPkg.Options().Prefix = "build"
	buildPkg.Name = "build-dependencies"
	buildPkg.BuildID("build-dependencies")
	buildPkg.DownloadLocation = "NOASSERTION"
	buildPkg.LicenseConcluded = "NOASSERTION"
	buildPkg.PrimaryPurpose = "OTHER"

	var allDeps []WorkflowDependency

	for _, wfPath := range workflowPaths {
		matches, err := resolveGlob(wfPath)
		if err != nil {
			return nil, fmt.Errorf("globbing workflow pattern %s: %w", wfPath, err)
		}

		for _, path := range matches {
			logrus.Infof("Parsing workflow file: %s", path)
			wf, err := ParseWorkflowFile(path)
			if err != nil {
				logrus.Warnf("Skipping workflow %s: %v", path, err)
				continue
			}
			deps := ExtractWorkflowDependencies(wf)
			allDeps = append(allDeps, deps...)
		}
	}

	if len(allDeps) == 0 {
		logrus.Warn("No build dependencies found in workflow files")
		return buildPkg, nil
	}

	resolver := NewDependencyResolver(fetcher)
	resolvedDeps, err := resolver.ResolveWorkflowDependencies(ctx, allDeps)
	if err != nil {
		return nil, fmt.Errorf("resolving transitive dependencies: %w", err)
	}

	seen := map[string]bool{}
	directCount := 0
	transitiveCount := 0
	for i := range resolvedDeps {
		rd := &resolvedDeps[i]
		dedupeKey := rd.Type + ":" + rd.Name + "@" + rd.Version
		if seen[dedupeKey] {
			continue
		}
		seen[dedupeKey] = true

		if rd.Transitive {
			transitiveCount++
		} else {
			directCount++
		}

		pkg := depToSPDXPackage(&rd.WorkflowDependency)
		buildPkg.AddRelationship(&Relationship{
			FullRender: true,
			Type:       rd.RelationshipType,
			Peer:       pkg,
			Comment:    rd.Comment,
		})
	}

	logrus.Infof("Extracted %d unique build dependencies (%d direct, %d transitive) from workflows",
		len(seen), directCount, transitiveCount)

	return buildPkg, nil
}

var resolveGlob = func(pattern string) ([]string, error) {
	return filepath.Glob(pattern)
}
