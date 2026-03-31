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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseActionMetadata(t *testing.T) {
	for _, tc := range []struct {
		name        string
		yaml        string
		wantName    string
		wantUsing   string
		wantSteps   int
		wantErr     bool
		isComposite bool
		isDocker    bool
	}{
		{
			name: "composite action",
			yaml: `
name: 'My Composite Action'
description: 'A test composite action'
runs:
  using: 'composite'
  steps:
    - name: 'Checkout'
      uses: 'actions/checkout@v4'
    - name: 'Run script'
      run: 'echo hello'
`,
			wantName:    "My Composite Action",
			wantUsing:   "composite",
			wantSteps:   2,
			isComposite: true,
		},
		{
			name: "docker action",
			yaml: `
name: 'My Docker Action'
description: 'A test docker action'
runs:
  using: 'docker'
  image: 'docker://alpine:3.18'
`,
			wantName:  "My Docker Action",
			wantUsing: "docker",
			isDocker:  true,
		},
		{
			name: "node action",
			yaml: `
name: 'My Node Action'
description: 'A test node action'
runs:
  using: 'node20'
  main: 'dist/index.js'
`,
			wantName:  "My Node Action",
			wantUsing: "node20",
		},
		{
			name:    "malformed YAML",
			yaml:    `{{{invalid yaml`,
			wantErr: true,
		},
		{
			name: "empty YAML produces zero-value struct",
			yaml: ``,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta, err := ParseActionMetadata([]byte(tc.yaml))
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, meta)
			if tc.wantName != "" {
				require.Equal(t, tc.wantName, meta.Name)
			}
			if tc.wantUsing != "" {
				require.Equal(t, tc.wantUsing, meta.Runs.Using)
			}
			if tc.wantSteps > 0 {
				require.Len(t, meta.Runs.Steps, tc.wantSteps)
			}
			require.Equal(t, tc.isComposite, meta.IsComposite())
			require.Equal(t, tc.isDocker, meta.IsDocker())
		})
	}
}

func TestGitHubContentFetcherFetchFile(t *testing.T) {
	t.Run("success returns file content", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "/myorg/myrepo/v1/action.yml", r.URL.Path)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("name: My Action"))
		}))
		defer server.Close()

		f := NewGitHubContentFetcher(
			WithBaseURL(server.URL),
			WithHTTPClient(server.Client()),
		)
		data, err := f.FetchFile(context.Background(), "myorg", "myrepo", "v1", "action.yml")
		require.NoError(t, err)
		require.Equal(t, []byte("name: My Action"), data)
	})

	t.Run("404 returns FileNotFoundError", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		f := NewGitHubContentFetcher(
			WithBaseURL(server.URL),
			WithHTTPClient(server.Client()),
		)
		_, err := f.FetchFile(context.Background(), "myorg", "myrepo", "v1", "missing.yml")
		require.Error(t, err)
		var notFound *FileNotFoundError
		require.ErrorAs(t, err, &notFound)
	})

	t.Run("rate limit returns RateLimitError after retries", func(t *testing.T) {
		callCount := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			callCount++
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer server.Close()

		f := NewGitHubContentFetcher(
			WithBaseURL(server.URL),
			WithHTTPClient(server.Client()),
		)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		_, err := f.FetchFile(ctx, "myorg", "myrepo", "v1", "action.yml")
		require.Error(t, err)
		require.GreaterOrEqual(t, callCount, 1)
	})

	t.Run("second call returns cached result without hitting server", func(t *testing.T) {
		callCount := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			callCount++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("cached content"))
		}))
		defer server.Close()

		f := NewGitHubContentFetcher(
			WithBaseURL(server.URL),
			WithHTTPClient(server.Client()),
		)

		data1, err := f.FetchFile(context.Background(), "myorg", "myrepo", "v1", "action.yml")
		require.NoError(t, err)

		data2, err := f.FetchFile(context.Background(), "myorg", "myrepo", "v1", "action.yml")
		require.NoError(t, err)

		require.Equal(t, data1, data2)
		require.Equal(t, 1, callCount, "second call should use cache")
	})

	t.Run("auth header set when token provided", func(t *testing.T) {
		var capturedAuth string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("content"))
		}))
		defer server.Close()

		f := NewGitHubContentFetcher(
			WithBaseURL(server.URL),
			WithHTTPClient(server.Client()),
			WithToken("my-secret-token"),
		)
		_, err := f.FetchFile(context.Background(), "myorg", "myrepo", "v1", "action.yml")
		require.NoError(t, err)
		require.Equal(t, "token my-secret-token", capturedAuth)
	})

	t.Run("no auth header when no token", func(t *testing.T) {
		var capturedAuth string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("content"))
		}))
		defer server.Close()

		origToken := os.Getenv("GITHUB_TOKEN")
		os.Unsetenv("GITHUB_TOKEN")
		defer os.Setenv("GITHUB_TOKEN", origToken)

		f := NewGitHubContentFetcher(
			WithBaseURL(server.URL),
			WithHTTPClient(server.Client()),
		)
		_, err := f.FetchFile(context.Background(), "myorg", "myrepo", "v1", "action.yml")
		require.NoError(t, err)
		require.Empty(t, capturedAuth)
	})
}

func TestGitHubContentFetcherFetchActionMetadata(t *testing.T) {
	compositeYAML := `
name: 'My Composite Action'
description: 'A test composite action'
runs:
  using: 'composite'
  steps:
    - name: 'Checkout'
      uses: 'actions/checkout@v4'
`

	t.Run("composite action.yml found", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/myorg/myaction/v1/action.yml" {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(compositeYAML))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		f := NewGitHubContentFetcher(WithBaseURL(server.URL), WithHTTPClient(server.Client()))
		ref := ParseActionRef("myorg/myaction@v1")
		meta, err := f.FetchActionMetadata(context.Background(), ref)
		require.NoError(t, err)
		require.Equal(t, "My Composite Action", meta.Name)
		require.True(t, meta.IsComposite())
	})

	t.Run("action.yml 404 falls back to requesting action.yaml", func(t *testing.T) {
		requestedPaths := []string{}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestedPaths = append(requestedPaths, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		f := NewGitHubContentFetcher(WithBaseURL(server.URL), WithHTTPClient(server.Client()))
		ref := ParseActionRef("myorg/myaction@v1")
		_, _ = f.FetchActionMetadata(context.Background(), ref)
		require.Contains(t, requestedPaths, "/myorg/myaction/v1/action.yml")
	})

	t.Run("non-GitHub ref returns error", func(t *testing.T) {
		f := NewGitHubContentFetcher()
		ref := ParseActionRef("docker://alpine:3.18")
		_, err := f.FetchActionMetadata(context.Background(), ref)
		require.Error(t, err)
	})

	t.Run("local ref returns error", func(t *testing.T) {
		f := NewGitHubContentFetcher()
		ref := ParseActionRef("./local-action")
		_, err := f.FetchActionMetadata(context.Background(), ref)
		require.Error(t, err)
	})

	t.Run("both action.yml and action.yaml 404 returns error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		f := NewGitHubContentFetcher(WithBaseURL(server.URL), WithHTTPClient(server.Client()))
		ref := ParseActionRef("myorg/myaction@v1")
		_, err := f.FetchActionMetadata(context.Background(), ref)
		require.Error(t, err)
	})

	t.Run("action with subpath uses correct path", func(t *testing.T) {
		var capturedPath string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(compositeYAML))
		}))
		defer server.Close()

		f := NewGitHubContentFetcher(WithBaseURL(server.URL), WithHTTPClient(server.Client()))
		ref := ParseActionRef("myorg/myrepo/subpath@v1")
		_, err := f.FetchActionMetadata(context.Background(), ref)
		require.NoError(t, err)
		require.Equal(t, "/myorg/myrepo/v1/subpath/action.yml", capturedPath)
	})
}

func TestGitHubContentFetcherFetchReusableWorkflow(t *testing.T) {
	reusableWFYAML := `
name: Reusable Workflow
on:
  workflow_call:
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
`

	t.Run("valid workflow YAML fetched and parsed", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/myorg/workflows/main/.github/workflows/reusable.yml" {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(reusableWFYAML))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		f := NewGitHubContentFetcher(WithBaseURL(server.URL), WithHTTPClient(server.Client()))
		ref := ParseActionRef("myorg/workflows/.github/workflows/reusable.yml@main")
		wf, err := f.FetchReusableWorkflow(context.Background(), ref)
		require.NoError(t, err)
		require.Equal(t, "Reusable Workflow", wf.Name)
		require.Contains(t, wf.Jobs, "build")
	})

	t.Run("invalid ref missing path returns error", func(t *testing.T) {
		f := NewGitHubContentFetcher()
		ref := ActionRef{Owner: "myorg", Repo: "myrepo", Ref: "main", Raw: "myorg/myrepo@main"}
		_, err := f.FetchReusableWorkflow(context.Background(), ref)
		require.Error(t, err)
	})

	t.Run("invalid ref missing owner returns error", func(t *testing.T) {
		f := NewGitHubContentFetcher()
		ref := ActionRef{Repo: "myrepo", Path: ".github/workflows/ci.yml", Ref: "main"}
		_, err := f.FetchReusableWorkflow(context.Background(), ref)
		require.Error(t, err)
	})

	t.Run("server returns 404 for workflow", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		f := NewGitHubContentFetcher(WithBaseURL(server.URL), WithHTTPClient(server.Client()))
		ref := ParseActionRef("myorg/workflows/.github/workflows/missing.yml@main")
		_, err := f.FetchReusableWorkflow(context.Background(), ref)
		require.Error(t, err)
	})
}

func TestDependencyResolverResolveWorkflowDependencies(t *testing.T) {
	t.Run("direct deps only when action has no action.yml", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		f := NewGitHubContentFetcher(WithBaseURL(server.URL), WithHTTPClient(server.Client()))
		resolver := NewDependencyResolver(f)

		directDeps := []WorkflowDependency{
			{
				Name:    "actions/checkout",
				Version: "v4",
				Type:    "action",
			},
		}

		resolved, err := resolver.ResolveWorkflowDependencies(context.Background(), directDeps)
		require.NoError(t, err)
		require.Len(t, resolved, 1)
		require.Equal(t, "actions/checkout", resolved[0].Name)
		require.False(t, resolved[0].Transitive)
	})

	t.Run("composite action with transitive deps", func(t *testing.T) {
		compositeYAML := `
name: 'Composite Action'
runs:
  using: 'composite'
  steps:
    - name: 'Checkout'
      uses: 'actions/checkout@v4'
    - name: 'Run script'
      run: 'echo hello'
`
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/myorg/composite/v1/action.yml" {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(compositeYAML))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		f := NewGitHubContentFetcher(WithBaseURL(server.URL), WithHTTPClient(server.Client()))
		resolver := NewDependencyResolver(f)

		directDeps := []WorkflowDependency{
			{
				Name:    "myorg/composite",
				Version: "v1",
				Type:    "action",
			},
		}

		resolved, err := resolver.ResolveWorkflowDependencies(context.Background(), directDeps)
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(resolved), 2)

		names := make(map[string]bool)
		for _, r := range resolved {
			names[r.Name] = true
		}
		require.True(t, names["myorg/composite"])
		require.True(t, names["actions/checkout"])

		for _, r := range resolved {
			if r.Name == "actions/checkout" {
				require.True(t, r.Transitive)
				require.Equal(t, "myorg/composite", r.ResolvedVia)
			}
		}
	})

	t.Run("reusable workflow with nested deps", func(t *testing.T) {
		reusableWFYAML := `
name: Reusable Workflow
on:
  workflow_call:
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
`
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/myorg/workflows/main/.github/workflows/ci.yml" {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(reusableWFYAML))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		f := NewGitHubContentFetcher(WithBaseURL(server.URL), WithHTTPClient(server.Client()))
		resolver := NewDependencyResolver(f)

		directDeps := []WorkflowDependency{
			{
				Name:    "myorg/workflows/.github/workflows/ci.yml",
				Version: "main",
				Type:    "reusable-workflow",
			},
		}

		resolved, err := resolver.ResolveWorkflowDependencies(context.Background(), directDeps)
		require.NoError(t, err)

		names := make(map[string]bool)
		for _, r := range resolved {
			names[r.Name] = true
		}
		require.True(t, names["myorg/workflows/.github/workflows/ci.yml"])
		require.True(t, names["actions/checkout"])
		require.True(t, names["actions/setup-go"])
	})

	t.Run("cycle detection prevents infinite loop", func(t *testing.T) {
		selfRefYAML := `
name: 'Self Referencing'
runs:
  using: 'composite'
  steps:
    - uses: 'myorg/selfref@v1'
`
		callCount := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			callCount++
			if r.URL.Path == "/myorg/selfref/v1/action.yml" {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(selfRefYAML))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		f := NewGitHubContentFetcher(WithBaseURL(server.URL), WithHTTPClient(server.Client()))
		resolver := NewDependencyResolver(f)

		directDeps := []WorkflowDependency{
			{
				Name:    "myorg/selfref",
				Version: "v1",
				Type:    "action",
			},
		}

		resolved, err := resolver.ResolveWorkflowDependencies(context.Background(), directDeps)
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(resolved), 1)
	})

	t.Run("max depth reached stops recursion", func(t *testing.T) {
		var buildYAML = func(nextAction string) string {
			if nextAction == "" {
				return `
name: 'Leaf'
runs:
  using: 'node20'
  main: 'index.js'
`
			}
			return `
name: 'Composite'
runs:
  using: 'composite'
  steps:
    - uses: '` + nextAction + `'
`
		}

		depth := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			depth++
			if depth <= maxResolveDepth {
				nextAction := "myorg/level" + string(rune('0'+depth)) + "@v1"
				if depth > 9 {
					nextAction = ""
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(buildYAML(nextAction)))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		f := NewGitHubContentFetcher(WithBaseURL(server.URL), WithHTTPClient(server.Client()))
		resolver := NewDependencyResolver(f)
		resolver.maxDepth = 2

		directDeps := []WorkflowDependency{
			{
				Name:    "myorg/level0",
				Version: "v1",
				Type:    "action",
			},
		}

		resolved, err := resolver.ResolveWorkflowDependencies(context.Background(), directDeps)
		require.NoError(t, err)
		require.NotNil(t, resolved)
	})

	t.Run("docker action image extracted as transitive dep", func(t *testing.T) {
		dockerActionYAML := `
name: 'Docker Action'
runs:
  using: 'docker'
  image: 'docker://alpine:3.18'
`
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/myorg/docker-action/v1/action.yml" {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(dockerActionYAML))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		f := NewGitHubContentFetcher(WithBaseURL(server.URL), WithHTTPClient(server.Client()))
		resolver := NewDependencyResolver(f)

		directDeps := []WorkflowDependency{
			{
				Name:    "myorg/docker-action",
				Version: "v1",
				Type:    "action",
			},
		}

		resolved, err := resolver.ResolveWorkflowDependencies(context.Background(), directDeps)
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(resolved), 2)

		var foundDocker bool
		for _, r := range resolved {
			if r.Type == "docker-image" && r.Name == "alpine:3.18" {
				foundDocker = true
				require.True(t, r.Transitive)
			}
		}
		require.True(t, foundDocker, "should find docker image transitive dep")
	})

	t.Run("non-action type deps are not resolved transitively", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("should not fetch anything for runner-os type")
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		f := NewGitHubContentFetcher(WithBaseURL(server.URL), WithHTTPClient(server.Client()))
		resolver := NewDependencyResolver(f)

		directDeps := []WorkflowDependency{
			{
				Name:    "ubuntu-latest",
				Version: "",
				Type:    "runner-os",
			},
		}

		resolved, err := resolver.ResolveWorkflowDependencies(context.Background(), directDeps)
		require.NoError(t, err)
		require.Len(t, resolved, 1)
	})
}

func TestWorkflowsToSPDXPackagesResolved(t *testing.T) {
	compositeActionYAML := `
name: 'My Composite Action'
description: 'A test composite action'
runs:
  using: 'composite'
  steps:
    - name: 'Checkout'
      uses: 'actions/checkout@v4'
    - name: 'Run script'
      run: 'echo hello'
`

	t.Run("integration with mock server for composite action", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/myorg/myaction/v1/action.yml" {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(compositeActionYAML))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		dir := t.TempDir()
		wfContent := `
name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: myorg/myaction@v1
`
		wfPath := filepath.Join(dir, "ci.yml")
		require.NoError(t, os.WriteFile(wfPath, []byte(wfContent), 0o644))

		origResolveGlob := resolveGlob
		resolveGlob = func(pattern string) ([]string, error) {
			return []string{wfPath}, nil
		}
		t.Cleanup(func() { resolveGlob = origResolveGlob })

		fetcher := NewGitHubContentFetcher(WithBaseURL(server.URL), WithHTTPClient(server.Client()))
		pkg, err := WorkflowsToSPDXPackagesResolved(context.Background(), []string{wfPath}, fetcher)
		require.NoError(t, err)
		require.Equal(t, "build-dependencies", pkg.Name)

		relationships := pkg.GetRelationships()
		require.NotNil(t, relationships)

		pkgNames := make(map[string]bool)
		for _, rel := range *relationships {
			if rel.Peer != nil {
				if p, ok := rel.Peer.(*Package); ok {
					pkgNames[p.Name] = true
				}
			}
		}
		require.True(t, pkgNames["myorg/myaction"], "direct dep should be present")
		require.True(t, pkgNames["actions/checkout"], "transitive dep from composite action should be present")
	})

	t.Run("empty workflow paths returns empty build package", func(t *testing.T) {
		origResolveGlob := resolveGlob
		resolveGlob = func(pattern string) ([]string, error) {
			return []string{}, nil
		}
		t.Cleanup(func() { resolveGlob = origResolveGlob })

		fetcher := NewGitHubContentFetcher()
		pkg, err := WorkflowsToSPDXPackagesResolved(context.Background(), []string{"*.yml"}, fetcher)
		require.NoError(t, err)
		require.Equal(t, "build-dependencies", pkg.Name)
		require.Empty(t, *pkg.GetRelationships())
	})

	t.Run("deduplicates same dep appearing in multiple workflows", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		dir := t.TempDir()
		wfContent1 := `
name: CI1
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`
		wfContent2 := `
name: CI2
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`
		wfPath1 := filepath.Join(dir, "ci1.yml")
		wfPath2 := filepath.Join(dir, "ci2.yml")
		require.NoError(t, os.WriteFile(wfPath1, []byte(wfContent1), 0o644))
		require.NoError(t, os.WriteFile(wfPath2, []byte(wfContent2), 0o644))

		origResolveGlob := resolveGlob
		resolveGlob = func(pattern string) ([]string, error) {
			return []string{wfPath1, wfPath2}, nil
		}
		t.Cleanup(func() { resolveGlob = origResolveGlob })

		fetcher := NewGitHubContentFetcher(WithBaseURL(server.URL), WithHTTPClient(server.Client()))
		pkg, err := WorkflowsToSPDXPackagesResolved(context.Background(), []string{"*.yml"}, fetcher)
		require.NoError(t, err)

		checkoutCount := 0
		for _, rel := range *pkg.GetRelationships() {
			if rel.Peer != nil {
				if p, ok := rel.Peer.(*Package); ok && p.Name == "actions/checkout" {
					checkoutCount++
				}
			}
		}
		require.Equal(t, 1, checkoutCount, "duplicate actions/checkout should be deduplicated")
	})

	t.Run("invalid workflow file is skipped with warning", func(t *testing.T) {
		dir := t.TempDir()
		invalidContent := `{{{not valid yaml`
		wfPath := filepath.Join(dir, "bad.yml")
		require.NoError(t, os.WriteFile(wfPath, []byte(invalidContent), 0o644))

		origResolveGlob := resolveGlob
		resolveGlob = func(pattern string) ([]string, error) {
			return []string{wfPath}, nil
		}
		t.Cleanup(func() { resolveGlob = origResolveGlob })

		fetcher := NewGitHubContentFetcher()
		pkg, err := WorkflowsToSPDXPackagesResolved(context.Background(), []string{wfPath}, fetcher)
		require.NoError(t, err)
		require.Equal(t, "build-dependencies", pkg.Name)
	})
}
