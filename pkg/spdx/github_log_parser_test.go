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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func createTestZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, content := range files {
		f, err := w.Create(name)
		require.NoError(t, err)
		_, err = f.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	return buf.Bytes()
}

func TestParseLogForDependencies(t *testing.T) {
	for _, tc := range []struct {
		name        string
		line        string
		wantLen     int
		wantName    string
		wantVersion string
		wantType    string
		wantSource  string
	}{
		{
			name:        "setupGoRe",
			line:        "Successfully set up Go version 1.21.5",
			wantLen:     1,
			wantName:    "go",
			wantVersion: "1.21.5",
			wantType:    "tool",
			wantSource:  "setup-action",
		},
		{
			name:        "setupNodeRe",
			line:        "Successfully set up Node.js version 18.17.0",
			wantLen:     1,
			wantName:    "node",
			wantVersion: "18.17.0",
			wantType:    "tool",
			wantSource:  "setup-action",
		},
		{
			name:        "setupPythonRe",
			line:        "Successfully set up CPython (3.11.4)",
			wantLen:     1,
			wantName:    "python",
			wantVersion: "3.11.4",
			wantType:    "tool",
			wantSource:  "setup-action",
		},
		{
			name:        "setupJavaRe",
			line:        "Successfully set up Temurin 17.0.8",
			wantLen:     1,
			wantName:    "java",
			wantVersion: "17.0.8",
			wantType:    "tool",
			wantSource:  "setup-action",
		},
		{
			name:        "setupRubyRe",
			line:        "ruby 3.2.2p53 ",
			wantLen:     1,
			wantName:    "ruby",
			wantVersion: "3.2.2p53",
			wantType:    "tool",
			wantSource:  "setup-action",
		},
		{
			name:       "aptGetInstallRe",
			line:       "apt-get install -y curl wget",
			wantLen:    1,
			wantName:   "curl wget",
			wantSource: "apt-get",
			wantType:   "package",
		},
		{
			name:        "aptSettingUpRe",
			line:        "Setting up curl (7.88.1-10)",
			wantLen:     1,
			wantName:    "curl",
			wantVersion: "7.88.1-10",
			wantSource:  "apt-get",
			wantType:    "package",
		},
		{
			name:       "pipInstallRe",
			line:       "pip install requests flask",
			wantLen:    1,
			wantName:   "requests flask",
			wantSource: "pip",
			wantType:   "package",
		},
		{
			name:       "npmInstallRe",
			line:       "added 150 packages",
			wantLen:    1,
			wantName:   "npm-packages(150)",
			wantSource: "npm",
			wantType:   "package",
		},
		{
			name:        "goInstallRe",
			line:        "go install golang.org/x/tools/cmd/goimports@v0.14.0",
			wantLen:     1,
			wantName:    "golang.org/x/tools/cmd/goimports",
			wantVersion: "v0.14.0",
			wantSource:  "go-install",
			wantType:    "tool",
		},
		{
			name:        "goDownloadRe",
			line:        "go: downloading golang.org/x/text v0.13.0",
			wantLen:     1,
			wantName:    "golang.org/x/text",
			wantVersion: "0.13.0",
			wantSource:  "go-install",
			wantType:    "package",
		},
		{
			name:       "dockerPullRe",
			line:       "docker pull alpine:3.18",
			wantLen:    1,
			wantName:   "alpine:3.18",
			wantSource: "docker-pull",
			wantType:   "docker-image",
		},
		{
			name:    "no match",
			line:    "This line matches nothing at all",
			wantLen: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := ParseLogForDependencies("job", "step", []byte(tc.line))
			require.Len(t, deps, tc.wantLen)
			if tc.wantLen > 0 {
				d := deps[0]
				if tc.wantName != "" {
					require.Equal(t, tc.wantName, d.Name)
				}
				if tc.wantVersion != "" {
					require.Equal(t, tc.wantVersion, d.Version)
				}
				if tc.wantType != "" {
					require.Equal(t, tc.wantType, d.Type)
				}
				if tc.wantSource != "" {
					require.Equal(t, tc.wantSource, d.Source)
				}
				require.Equal(t, "job", d.JobName)
				require.Equal(t, "step", d.StepName)
			}
		})
	}

	t.Run("pipInstalledRe", func(t *testing.T) {
		line := "Successfully installed requests-2.31.0 flask-3.0.0"
		deps := ParseLogForDependencies("job", "step", []byte(line))
		require.Len(t, deps, 2)

		byName := map[string]ObservedDependency{}
		for _, d := range deps {
			byName[d.Name] = d
		}
		req, ok := byName["requests"]
		require.True(t, ok, "should find requests")
		require.Equal(t, "2.31.0", req.Version)
		require.Equal(t, "pip", req.Source)

		fl, ok := byName["flask"]
		require.True(t, ok, "should find flask")
		require.Equal(t, "3.0.0", fl.Version)
		require.Equal(t, "pip", fl.Source)
	})

	t.Run("dockerDigestRe", func(t *testing.T) {
		line := "Digest: sha256:abcdef0123456789"
		deps := ParseLogForDependencies("job", "step", []byte(line))
		require.Len(t, deps, 1)
		d := deps[0]
		require.Equal(t, "docker-image", d.Name)
		require.Contains(t, d.Version, "sha256:")
		require.Equal(t, "docker-image", d.Type)
		require.Equal(t, "docker-pull", d.Source)
	})
}

func TestParseLogFilePath(t *testing.T) {
	for _, tc := range []struct {
		name         string
		filePath     string
		wantJobName  string
		wantStepName string
	}{
		{
			name:         "standard with number prefix",
			filePath:     "build/3_Set up Go.txt",
			wantJobName:  "build",
			wantStepName: "Set up Go",
		},
		{
			name:         "another standard path",
			filePath:     "test-unit/1_Checkout.txt",
			wantJobName:  "test-unit",
			wantStepName: "Checkout",
		},
		{
			name:         "job only no slash",
			filePath:     "jobonly",
			wantJobName:  "jobonly",
			wantStepName: "",
		},
		{
			name:         "empty string",
			filePath:     "",
			wantJobName:  "",
			wantStepName: "",
		},
		{
			name:         "job and step without underscore prefix",
			filePath:     "job/nostep",
			wantJobName:  "job",
			wantStepName: "nostep",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jobName, stepName := parseLogFilePath(tc.filePath)
			require.Equal(t, tc.wantJobName, jobName)
			require.Equal(t, tc.wantStepName, stepName)
		})
	}
}

func TestSplitNameVersion(t *testing.T) {
	for _, tc := range []struct {
		name        string
		input       string
		wantName    string
		wantVersion string
	}{
		{
			name:        "simple name-version",
			input:       "requests-2.31.0",
			wantName:    "requests",
			wantVersion: "2.31.0",
		},
		{
			name:        "flask version",
			input:       "flask-3.0.0",
			wantName:    "flask",
			wantVersion: "3.0.0",
		},
		{
			name:        "splits on last hyphen",
			input:       "no-version-here-1.0",
			wantName:    "no-version-here",
			wantVersion: "1.0",
		},
		{
			name:        "single word no hyphen",
			input:       "singleword",
			wantName:    "singleword",
			wantVersion: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotName, gotVersion := splitNameVersion(tc.input)
			require.Equal(t, tc.wantName, gotName)
			require.Equal(t, tc.wantVersion, gotVersion)
		})
	}
}

func TestReadZipToMap(t *testing.T) {
	t.Run("valid zip with two entries", func(t *testing.T) {
		zipData := createTestZip(t, map[string]string{
			"file1.txt":     "hello world",
			"dir/file2.txt": "foo bar",
		})

		result, err := readZipToMap(zipData)
		require.NoError(t, err)
		require.Len(t, result, 2)
		require.Equal(t, []byte("hello world"), result["file1.txt"])
		require.Equal(t, []byte("foo bar"), result["dir/file2.txt"])
	})

	t.Run("invalid zip data returns error", func(t *testing.T) {
		_, err := readZipToMap([]byte("this is not a zip file"))
		require.Error(t, err)
	})

	t.Run("empty zip returns empty map", func(t *testing.T) {
		zipData := createTestZip(t, map[string]string{})
		result, err := readZipToMap(zipData)
		require.NoError(t, err)
		require.Empty(t, result)
	})
}

func TestRunLogFetcherFetchRunLog(t *testing.T) {
	t.Run("success follows redirect and returns zip contents", func(t *testing.T) {
		zipData := createTestZip(t, map[string]string{
			"build/1_Setup Go.txt":  "Successfully set up Go version 1.21.5\n",
			"build/2_Run tests.txt": "go: downloading golang.org/x/text v0.13.0\n",
		})

		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/repos/owner/repo/actions/runs/123/logs" {
				w.Header().Set("Location", "http://"+r.Host+"/download")
				w.WriteHeader(http.StatusFound)
				return
			}
			if r.URL.Path == "/download" {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(zipData)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer ts.Close()

		fetcher := NewRunLogFetcher(WithBaseURL(ts.URL), WithToken("test-token"))
		result, err := fetcher.FetchRunLog(context.Background(), "owner", "repo", 123)
		require.NoError(t, err)
		require.Contains(t, result, "build/1_Setup Go.txt")
		require.Contains(t, result, "build/2_Run tests.txt")
	})

	t.Run("404 response returns FileNotFoundError", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer ts.Close()

		fetcher := NewRunLogFetcher(WithBaseURL(ts.URL), WithToken("test-token"))
		_, err := fetcher.FetchRunLog(context.Background(), "owner", "repo", 999)
		require.Error(t, err)
		var notFound *FileNotFoundError
		require.ErrorAs(t, err, &notFound)
	})

	t.Run("no redirect location returns error", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer ts.Close()

		fetcher := NewRunLogFetcher(WithBaseURL(ts.URL), WithToken("test-token"))
		_, err := fetcher.FetchRunLog(context.Background(), "owner", "repo", 123)
		require.Error(t, err)
	})
}

func TestRunLogFetcherListRecentRuns(t *testing.T) {
	t.Run("returns list of workflow runs", func(t *testing.T) {
		response := workflowRunsResponse{
			TotalCount: 2,
			WorkflowRuns: []WorkflowRun{
				{ID: 100, Name: "CI", Status: "completed", Conclusion: "success"},
				{ID: 99, Name: "Release", Status: "completed", Conclusion: "failure"},
			},
		}
		jsonBytes, err := json.Marshal(response)
		require.NoError(t, err)

		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(jsonBytes)
		}))
		defer ts.Close()

		fetcher := NewRunLogFetcher(WithBaseURL(ts.URL), WithToken("test-token"))
		runs, err := fetcher.ListRecentRuns(context.Background(), "owner", "repo", 10)
		require.NoError(t, err)
		require.Len(t, runs, 2)
		require.Equal(t, int64(100), runs[0].ID)
		require.Equal(t, "CI", runs[0].Name)
		require.Equal(t, int64(99), runs[1].ID)
		require.Equal(t, "Release", runs[1].Name)
	})

	t.Run("404 returns FileNotFoundError", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer ts.Close()

		fetcher := NewRunLogFetcher(WithBaseURL(ts.URL), WithToken("test-token"))
		_, err := fetcher.ListRecentRuns(context.Background(), "owner", "repo", 10)
		require.Error(t, err)
		var notFound *FileNotFoundError
		require.ErrorAs(t, err, &notFound)
	})
}

func TestParseRunLogs(t *testing.T) {
	t.Run("parses and deduplicates across files", func(t *testing.T) {
		logFiles := map[string][]byte{
			"build/1_Setup Go.txt":  []byte("Successfully set up Go version 1.21.5\n"),
			"build/2_Run tests.txt": []byte("go: downloading golang.org/x/text v0.13.0\n"),
			"test/1_Setup Go.txt":   []byte("Successfully set up Go version 1.21.5\n"),
		}

		deps := ParseRunLogs(logFiles)

		goSetupCount := 0
		goDownloadCount := 0
		for _, d := range deps {
			if d.Name == "go" && d.Version == "1.21.5" {
				goSetupCount++
			}
			if d.Name == "golang.org/x/text" && d.Version == "0.13.0" {
				goDownloadCount++
			}
		}
		require.Equal(t, 1, goSetupCount, "go setup should be deduplicated to 1")
		require.Equal(t, 1, goDownloadCount, "go download should appear once")
	})

	t.Run("job and step names are set from file path", func(t *testing.T) {
		logFiles := map[string][]byte{
			"mybuild/3_Set up Go.txt": []byte("Successfully set up Go version 1.21.5\n"),
		}

		deps := ParseRunLogs(logFiles)
		require.Len(t, deps, 1)
		require.Equal(t, "mybuild", deps[0].JobName)
		require.Equal(t, "Set up Go", deps[0].StepName)
	})

	t.Run("empty log files returns empty slice", func(t *testing.T) {
		deps := ParseRunLogs(map[string][]byte{})
		require.Empty(t, deps)
	})
}

func TestRunLogsToSPDXPackages(t *testing.T) {
	t.Run("returns package with build dependency relationships", func(t *testing.T) {
		zipData := createTestZip(t, map[string]string{
			"build/1_Setup Go.txt":  "Successfully set up Go version 1.21.5\n",
			"build/2_Run tests.txt": "go: downloading golang.org/x/text v0.13.0\n",
		})

		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/repos/owner/repo/actions/runs/42/logs" {
				w.Header().Set("Location", "http://"+r.Host+"/download")
				w.WriteHeader(http.StatusFound)
				return
			}
			if r.URL.Path == "/download" {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(zipData)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer ts.Close()

		fetcher := NewRunLogFetcher(WithBaseURL(ts.URL), WithToken("test-token"))
		pkg, err := RunLogsToSPDXPackages(context.Background(), "owner", "repo", 42, fetcher)
		require.NoError(t, err)
		require.NotNil(t, pkg)
		require.Equal(t, "observed-build-dependencies", pkg.Name)

		rels := pkg.GetRelationships()
		require.NotNil(t, rels)
		require.NotEmpty(t, *rels)

		for _, rel := range *rels {
			require.Equal(t, BUILD_DEPENDENCY_OF, rel.Type)
			require.NotNil(t, rel.Peer)
		}

		peerNames := map[string]bool{}
		for _, rel := range *rels {
			if p, ok := rel.Peer.(*Package); ok {
				peerNames[p.Name] = true
			}
		}
		require.True(t, peerNames["go"], "should have go as a peer package")
		require.True(t, peerNames["golang.org/x/text"], "should have golang.org/x/text as a peer package")
	})

	t.Run("fetch error propagates", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer ts.Close()

		fetcher := NewRunLogFetcher(WithBaseURL(ts.URL), WithToken("test-token"))
		_, err := RunLogsToSPDXPackages(context.Background(), "owner", "repo", 999, fetcher)
		require.Error(t, err)
	})
}

func TestObservedDepToSPDXPackage(t *testing.T) {
	t.Run("converts dependency to SPDX package", func(t *testing.T) {
		dep := &ObservedDependency{
			Name:     "go",
			Version:  "1.21.5",
			Type:     "tool",
			Source:   "setup-action",
			JobName:  "build",
			StepName: "setup",
		}

		pkg := observedDepToSPDXPackage(dep)
		require.Equal(t, "go", pkg.Name)
		require.Equal(t, "1.21.5", pkg.Version)
		require.Contains(t, pkg.Comment, "setup-action")
		require.Contains(t, pkg.Comment, "build")
		require.Contains(t, pkg.Comment, "setup")
		require.Equal(t, "NOASSERTION", pkg.DownloadLocation)
		require.Equal(t, "NOASSERTION", pkg.LicenseConcluded)
	})

	t.Run("docker image type", func(t *testing.T) {
		dep := &ObservedDependency{
			Name:     "alpine:3.18",
			Version:  "3.18",
			Type:     "docker-image",
			Source:   "docker-pull",
			JobName:  "ci",
			StepName: "pull-image",
		}

		pkg := observedDepToSPDXPackage(dep)
		require.Equal(t, "alpine:3.18", pkg.Name)
		require.Equal(t, "3.18", pkg.Version)
		require.Contains(t, pkg.Comment, "docker-pull")
	})

	t.Run("package prefix includes type", func(t *testing.T) {
		dep := &ObservedDependency{
			Name:     "requests",
			Version:  "2.31.0",
			Type:     "package",
			Source:   "pip",
			JobName:  "test",
			StepName: "install",
		}

		pkg := observedDepToSPDXPackage(dep)
		require.Contains(t, pkg.Options().Prefix, "package")
	})
}
