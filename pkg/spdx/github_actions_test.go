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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseActionRef(t *testing.T) {
	for _, tc := range []struct {
		name     string
		uses     string
		expected ActionRef
	}{
		{
			name: "standard action with tag",
			uses: "actions/checkout@v4",
			expected: ActionRef{
				Owner: "actions", Repo: "checkout", Ref: "v4",
				Raw: "actions/checkout@v4",
			},
		},
		{
			name: "SHA-pinned action",
			uses: "actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd",
			expected: ActionRef{
				Owner: "actions", Repo: "checkout",
				Ref: "de0fac2e4500dabe0009e67214ff5f5447ce83dd",
				Raw: "actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd",
			},
		},
		{
			name: "action with subpath",
			uses: "kubernetes-sigs/release-actions/setup-tejolote@v0.4.0",
			expected: ActionRef{
				Owner: "kubernetes-sigs", Repo: "release-actions",
				Path: "setup-tejolote", Ref: "v0.4.0",
				Raw: "kubernetes-sigs/release-actions/setup-tejolote@v0.4.0",
			},
		},
		{
			name: "docker action",
			uses: "docker://alpine:3.18",
			expected: ActionRef{
				IsDocker: true, Image: "alpine:3.18",
				Raw: "docker://alpine:3.18",
			},
		},
		{
			name: "docker action with registry",
			uses: "docker://ghcr.io/owner/image:latest",
			expected: ActionRef{
				IsDocker: true, Image: "ghcr.io/owner/image:latest",
				Raw: "docker://ghcr.io/owner/image:latest",
			},
		},
		{
			name: "local action",
			uses: "./local-action",
			expected: ActionRef{
				IsLocal: true,
				Raw:     "./local-action",
			},
		},
		{
			name: "relative local action",
			uses: "../other-action",
			expected: ActionRef{
				IsLocal: true,
				Raw:     "../other-action",
			},
		},
		{
			name: "malformed no at sign",
			uses: "actions/checkout",
			expected: ActionRef{
				Raw: "actions/checkout",
			},
		},
		{
			name: "reusable workflow",
			uses: "org/repo/.github/workflows/ci.yml@main",
			expected: ActionRef{
				Owner: "org", Repo: "repo",
				Path: ".github/workflows/ci.yml",
				Ref:  "main",
				Raw:  "org/repo/.github/workflows/ci.yml@main",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := ParseActionRef(tc.uses)
			require.Equal(t, tc.expected.Owner, ref.Owner, "Owner")
			require.Equal(t, tc.expected.Repo, ref.Repo, "Repo")
			require.Equal(t, tc.expected.Path, ref.Path, "Path")
			require.Equal(t, tc.expected.Ref, ref.Ref, "Ref")
			require.Equal(t, tc.expected.IsDocker, ref.IsDocker, "IsDocker")
			require.Equal(t, tc.expected.IsLocal, ref.IsLocal, "IsLocal")
			require.Equal(t, tc.expected.Image, ref.Image, "Image")
			require.Equal(t, tc.expected.Raw, ref.Raw, "Raw")
		})
	}
}

func TestActionRefIsSHA(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ref      string
		expected bool
	}{
		{"40 hex chars", "de0fac2e4500dabe0009e67214ff5f5447ce83dd", true},
		{"short tag", "v4", false},
		{"39 chars", "de0fac2e4500dabe0009e67214ff5f5447ce83d", false},
		{"uppercase hex", "DE0FAC2E4500DABE0009E67214FF5F5447CE83DD", false},
		{"empty string", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ar := ActionRef{Ref: tc.ref}
			require.Equal(t, tc.expected, ar.IsSHA())
		})
	}
}

func TestActionRefFullName(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ref      ActionRef
		expected string
	}{
		{
			name:     "owner and repo",
			ref:      ActionRef{Owner: "actions", Repo: "checkout", Raw: "actions/checkout@v4"},
			expected: "actions/checkout",
		},
		{
			name:     "owner repo and path",
			ref:      ActionRef{Owner: "org", Repo: "repo", Path: "setup-foo", Raw: "org/repo/setup-foo@v1"},
			expected: "org/repo/setup-foo",
		},
		{
			name:     "empty owner returns raw",
			ref:      ActionRef{Raw: "something-weird"},
			expected: "something-weird",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, tc.ref.FullName())
		})
	}
}

func TestActionRefPackageURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		ref  ActionRef
		want string
	}{
		{
			name: "standard action",
			ref:  ActionRef{Owner: "actions", Repo: "checkout", Ref: "v4"},
			want: "pkg:github/actions/checkout@v4",
		},
		{
			name: "docker action",
			ref:  ActionRef{IsDocker: true, Image: "alpine:3.18"},
			want: "pkg:docker/library/alpine@3.18",
		},
		{
			name: "local action",
			ref:  ActionRef{IsLocal: true, Raw: "./local"},
			want: "",
		},
		{
			name: "empty owner",
			ref:  ActionRef{Raw: "malformed"},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.ref.PackageURL()
			require.Equal(t, tc.want, got)
		})
	}
}

func TestActionRefDownloadLocation(t *testing.T) {
	for _, tc := range []struct {
		name string
		ref  ActionRef
		want string
	}{
		{
			name: "SHA ref",
			ref:  ActionRef{Owner: "actions", Repo: "checkout", Ref: "de0fac2e4500dabe0009e67214ff5f5447ce83dd"},
			want: "https://github.com/actions/checkout/commit/de0fac2e4500dabe0009e67214ff5f5447ce83dd",
		},
		{
			name: "tag ref",
			ref:  ActionRef{Owner: "actions", Repo: "checkout", Ref: "v4"},
			want: "https://github.com/actions/checkout/releases/tag/v4",
		},
		{
			name: "docker action",
			ref:  ActionRef{IsDocker: true, Image: "alpine:3.18"},
			want: "https://hub.docker.com/_/alpine",
		},
		{
			name: "local action",
			ref:  ActionRef{IsLocal: true, Raw: "./local"},
			want: "NOASSERTION",
		},
		{
			name: "empty owner",
			ref:  ActionRef{Raw: "malformed"},
			want: "NOASSERTION",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.ref.DownloadLocation())
		})
	}
}

func TestDockerImagePURL(t *testing.T) {
	for _, tc := range []struct {
		name  string
		image string
		want  string
	}{
		{"official image", "alpine:3.18", "pkg:docker/library/alpine@3.18"},
		{"namespaced image", "nginx/nginx:latest", "pkg:docker/nginx/nginx@latest"},
		{"no tag defaults to latest", "ubuntu", "pkg:docker/library/ubuntu@latest"},
		{"registry prefix", "gcr.io/project/image:v1", "pkg:docker/gcr.io/project/image@v1"},
		{"digest ref", "alpine@sha256:abc123def456", "pkg:docker/library/alpine@sha256%3Aabc123def456"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := dockerImagePURL(tc.image)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestExtractImageTag(t *testing.T) {
	for _, tc := range []struct {
		image string
		want  string
	}{
		{"alpine:3.18", "3.18"},
		{"alpine@sha256:abc123", "sha256:abc123"},
		{"ubuntu", "latest"},
		{"gcr.io/proj/img:v2", "v2"},
	} {
		t.Run(tc.image, func(t *testing.T) {
			require.Equal(t, tc.want, extractImageTag(tc.image))
		})
	}
}

func TestParseRunsOn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runsOn interface{}
		want   []string
	}{
		{"string", "ubuntu-latest", []string{"ubuntu-latest"}},
		{"slice", []interface{}{"ubuntu-latest", "self-hosted"}, []string{"ubuntu-latest", "self-hosted"}},
		{"map with labels", map[string]interface{}{"labels": []interface{}{"self-hosted"}}, []string{"self-hosted"}},
		{"map with group", map[string]interface{}{"group": "my-group"}, []string{"my-group"}},
		{"nil", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseRunsOn(tc.runsOn)
			if tc.want == nil {
				require.Nil(t, got)
			} else {
				require.Equal(t, tc.want, got)
			}
		})
	}
}

func TestParseContainer(t *testing.T) {
	for _, tc := range []struct {
		name      string
		container interface{}
		want      string
	}{
		{"string", "golang:1.21", "golang:1.21"},
		{"map with image", map[string]interface{}{"image": "golang:1.21"}, "golang:1.21"},
		{"nil", nil, ""},
		{"map without image", map[string]interface{}{"env": "FOO=bar"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, parseContainer(tc.container))
		})
	}
}

func TestParseWorkflowData(t *testing.T) {
	t.Run("valid minimal workflow", func(t *testing.T) {
		yamlData := []byte(`
name: Test
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Run tests
        run: go test ./...
`)
		wf, err := ParseWorkflowData(yamlData)
		require.NoError(t, err)
		require.Equal(t, "Test", wf.Name)
		require.Len(t, wf.Jobs, 1)
		job := wf.Jobs["build"]
		require.Len(t, job.Steps, 2)
		require.Equal(t, "actions/checkout@v4", job.Steps[0].Uses)
	})

	t.Run("invalid YAML", func(t *testing.T) {
		_, err := ParseWorkflowData([]byte(`{{{invalid`))
		require.Error(t, err)
	})
}

func TestExtractWorkflowDependencies(t *testing.T) {
	t.Run("extracts all dependency types", func(t *testing.T) {
		wf := &Workflow{
			Name: "Test",
			Jobs: map[string]WorkflowJob{
				"build": {
					RunsOn: "ubuntu-latest",
					Services: map[string]Service{
						"postgres": {Image: "postgres:15"},
					},
					Steps: []WorkflowStep{
						{Name: "Checkout", Uses: "actions/checkout@v4"},
						{Name: "Docker step", Uses: "docker://alpine:3.18"},
					},
				},
			},
		}

		deps := ExtractWorkflowDependencies(wf)

		require.Len(t, deps, 4)

		types := map[string]int{}
		for _, d := range deps {
			types[d.Type]++
		}
		require.Equal(t, 1, types["runner-os"])
		require.Equal(t, 1, types["service-container"])
		require.Equal(t, 1, types["action"])
		require.Equal(t, 1, types["docker-image"])
	})

	t.Run("deduplicates same action across steps", func(t *testing.T) {
		wf := &Workflow{
			Jobs: map[string]WorkflowJob{
				"build": {
					RunsOn: "ubuntu-latest",
					Steps: []WorkflowStep{
						{Name: "Checkout 1", Uses: "actions/checkout@v4"},
						{Name: "Checkout 2", Uses: "actions/checkout@v4"},
					},
				},
			},
		}

		deps := ExtractWorkflowDependencies(wf)

		actionCount := 0
		for _, d := range deps {
			if d.Type == "action" {
				actionCount++
			}
		}
		require.Equal(t, 1, actionCount, "duplicate actions should be deduplicated")
	})

	t.Run("reusable workflow at job level", func(t *testing.T) {
		wf := &Workflow{
			Jobs: map[string]WorkflowJob{
				"call": {
					Uses: "org/repo/.github/workflows/ci.yml@main",
				},
			},
		}

		deps := ExtractWorkflowDependencies(wf)

		found := false
		for _, d := range deps {
			if d.Type == "reusable-workflow" {
				found = true
				require.Equal(t, "org/repo/.github/workflows/ci.yml", d.Name)
				require.Equal(t, "main", d.Version)
				require.Equal(t, BUILD_TOOL_OF, d.RelationshipType)
			}
		}
		require.True(t, found, "should find reusable workflow dependency")
	})

	t.Run("skips local actions", func(t *testing.T) {
		wf := &Workflow{
			Jobs: map[string]WorkflowJob{
				"build": {
					RunsOn: "ubuntu-latest",
					Steps: []WorkflowStep{
						{Uses: "./local-action"},
					},
				},
			},
		}

		deps := ExtractWorkflowDependencies(wf)
		for _, d := range deps {
			require.NotEqual(t, "action", d.Type, "local actions should be skipped")
		}
	})

	t.Run("job container extraction", func(t *testing.T) {
		wf := &Workflow{
			Jobs: map[string]WorkflowJob{
				"build": {
					RunsOn:    "ubuntu-latest",
					Container: "golang:1.22",
				},
			},
		}

		deps := ExtractWorkflowDependencies(wf)
		found := false
		for _, d := range deps {
			if d.Type == "docker-image" && d.Name == "golang:1.22" {
				found = true
				require.Equal(t, BUILD_DEPENDENCY_OF, d.RelationshipType)
				require.Equal(t, "CONTAINER", d.PrimaryPurpose)
			}
		}
		require.True(t, found, "should find job container dependency")
	})
}

func TestWorkflowsToSPDXPackages(t *testing.T) {
	t.Run("parses workflow file from disk", func(t *testing.T) {
		dir := t.TempDir()
		wfContent := `
name: CI
on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
`
		wfPath := filepath.Join(dir, "ci.yml")
		require.NoError(t, os.WriteFile(wfPath, []byte(wfContent), 0o644))

		pkg, err := WorkflowsToSPDXPackages([]string{wfPath})
		require.NoError(t, err)
		require.Equal(t, "build-dependencies", pkg.Name)
		require.Equal(t, "OTHER", pkg.PrimaryPurpose)

		require.GreaterOrEqual(t, len(*pkg.GetRelationships()), 3)
	})

	t.Run("glob pattern works", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range []string{"a.yml", "b.yml"} {
			content := `
name: ` + name + `
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
		}

		pkg, err := WorkflowsToSPDXPackages([]string{filepath.Join(dir, "*.yml")})
		require.NoError(t, err)
		require.NotNil(t, pkg)
		require.GreaterOrEqual(t, len(*pkg.GetRelationships()), 1)
	})

	t.Run("empty workflows returns package with no relationships", func(t *testing.T) {
		dir := t.TempDir()
		wfContent := `
name: Empty
on: push
jobs: {}
`
		wfPath := filepath.Join(dir, "empty.yml")
		require.NoError(t, os.WriteFile(wfPath, []byte(wfContent), 0o644))

		pkg, err := WorkflowsToSPDXPackages([]string{wfPath})
		require.NoError(t, err)
		require.Equal(t, "build-dependencies", pkg.Name)
		require.Empty(t, *pkg.GetRelationships())
	})

	t.Run("nonexistent path logs warning and continues", func(t *testing.T) {
		pkg, err := WorkflowsToSPDXPackages([]string{"/nonexistent/path/*.yml"})
		require.NoError(t, err)
		require.Equal(t, "build-dependencies", pkg.Name)
		require.Empty(t, *pkg.GetRelationships())
	})
}

func TestDepToSPDXPackage(t *testing.T) {
	dep := &WorkflowDependency{
		Name:             "actions/checkout",
		Version:          "v4",
		Type:             "action",
		DownloadLocation: "https://github.com/actions/checkout/releases/tag/v4",
		PURL:             "pkg:github/actions/checkout@v4",
		PrimaryPurpose:   "",
	}

	pkg := depToSPDXPackage(dep)
	require.Equal(t, "actions/checkout", pkg.Name)
	require.Equal(t, "v4", pkg.Version)
	require.Equal(t, "https://github.com/actions/checkout/releases/tag/v4", pkg.DownloadLocation)
	require.Equal(t, "NOASSERTION", pkg.LicenseConcluded)
	require.Len(t, pkg.ExternalRefs, 1)
	require.Equal(t, "purl", pkg.ExternalRefs[0].Type)
	require.Equal(t, "pkg:github/actions/checkout@v4", pkg.ExternalRefs[0].Locator)
	require.True(t, strings.HasPrefix(pkg.Options().Prefix, "build-"))

	t.Run("no purl means no external refs", func(t *testing.T) {
		dep2 := &WorkflowDependency{
			Name:             "ubuntu-latest",
			Type:             "runner-os",
			DownloadLocation: "NOASSERTION",
			PrimaryPurpose:   "OPERATING-SYSTEM",
		}
		pkg2 := depToSPDXPackage(dep2)
		require.Empty(t, pkg2.ExternalRefs)
		require.Equal(t, "OPERATING-SYSTEM", pkg2.PrimaryPurpose)
	})
}

func TestParseWorkflowFile(t *testing.T) {
	t.Run("reads and parses file", func(t *testing.T) {
		dir := t.TempDir()
		content := `
name: Test
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
`
		path := filepath.Join(dir, "test.yml")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

		wf, err := ParseWorkflowFile(path)
		require.NoError(t, err)
		require.Equal(t, "Test", wf.Name)
	})

	t.Run("returns error for nonexistent file", func(t *testing.T) {
		_, err := ParseWorkflowFile("/does/not/exist.yml")
		require.Error(t, err)
	})
}
