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

// findRepoRoot traverses parent directories to locate the go.mod file,
// returning the repo root.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "could not find repo root")
		dir = parent
	}
}

// TestE2EWorkflowScanSelf exercises WorkflowsToSPDXPackages against the
// bom repo's own workflow files.
func TestE2EWorkflowScanSelf(t *testing.T) {
	repoRoot := findRepoRoot(t)
	workflowDir := filepath.Join(repoRoot, ".github", "workflows")

	// Find all workflow files (*.yml and *.yaml)
	workflowPaths, err := filepath.Glob(filepath.Join(workflowDir, "*.y*ml"))
	require.NoError(t, err)
	require.NotEmpty(t, workflowPaths, "expected at least one workflow file in .github/workflows/")

	pkg, err := WorkflowsToSPDXPackages(workflowPaths)
	require.NoError(t, err)
	require.NotNil(t, pkg)

	relationships := pkg.GetRelationships()
	require.NotNil(t, relationships)
	require.Greater(t, len(*relationships), 0, "expected at least one relationship from workflow scan")

	// Verify that actions/checkout is found — used in all 4 workflows
	foundCheckout := false
	foundSetupGo := false
	for _, rel := range *relationships {
		if rel.Peer == nil {
			continue
		}
		p, ok := rel.Peer.(*Package)
		if !ok {
			continue
		}
		if strings.HasPrefix(p.Name, "actions/checkout") || p.Name == "actions/checkout" {
			foundCheckout = true
			// Verify the relationship type is BUILD_TOOL_OF for action steps
			require.Equal(t, BUILD_TOOL_OF, rel.Type, "actions/checkout should be BUILD_TOOL_OF")
		}
		if strings.HasPrefix(p.Name, "actions/setup-go") || p.Name == "actions/setup-go" {
			foundSetupGo = true
		}
	}

	require.True(t, foundCheckout, "actions/checkout should be found as a dependency across all workflows")
	require.True(t, foundSetupGo, "actions/setup-go should be found as a dependency (release, snapshot, verify-spdx use it)")
}

// TestE2EBuildDocGenerateSelf exercises the full DocBuilder pipeline for
// build-deps generation using the repo's own workflow files.
func TestE2EBuildDocGenerateSelf(t *testing.T) {
	repoRoot := findRepoRoot(t)
	workflowDir := filepath.Join(repoRoot, ".github", "workflows")
	workflowPaths, err := filepath.Glob(filepath.Join(workflowDir, "*.y*ml"))
	require.NoError(t, err)
	require.NotEmpty(t, workflowPaths)

	tmpDir := t.TempDir()
	buildOutputFile := filepath.Join(tmpDir, "build-deps.spdx.json")

	builder := NewDocBuilder()
	opts := &DocGenerateOptions{
		Workflows:       workflowPaths,
		BuildOutputFile: buildOutputFile,
		Name:            "bom-self-test",
	}

	mainDoc, err := builder.Generate(opts)
	require.NoError(t, err)
	require.NotNil(t, mainDoc)
	require.Empty(t, mainDoc.Packages, "main doc should have no packages when BuildOutputFile is set and only Workflows are specified")

	buildDoc, err := builder.GenerateBuildDoc(opts)
	require.NoError(t, err)
	require.NotNil(t, buildDoc)

	require.Equal(t, "bom-self-test-build-deps", buildDoc.Name, "build doc name should have -build-deps suffix")
	require.NotEmpty(t, buildDoc.Packages, "build doc should contain packages from workflow scan")

	tvOutput, err := buildDoc.Render()
	require.NoError(t, err)
	require.NotEmpty(t, tvOutput)

	err = os.WriteFile(buildOutputFile, []byte(tvOutput), 0o644)
	require.NoError(t, err)
	info, err := os.Stat(buildOutputFile)
	require.NoError(t, err)
	require.Greater(t, info.Size(), int64(0), "build output file should be non-empty")
}

// TestE2EBuildDocTagValue exercises build doc generation in tag-value format.
func TestE2EBuildDocTagValue(t *testing.T) {
	repoRoot := findRepoRoot(t)
	workflowDir := filepath.Join(repoRoot, ".github", "workflows")
	workflowPaths, err := filepath.Glob(filepath.Join(workflowDir, "*.y*ml"))
	require.NoError(t, err)
	require.NotEmpty(t, workflowPaths)

	tmpDir := t.TempDir()
	buildOutputFile := filepath.Join(tmpDir, "build-deps.spdx")

	// Default format is tag-value
	builder := NewDocBuilder()
	opts := &DocGenerateOptions{
		Workflows:       workflowPaths,
		BuildOutputFile: buildOutputFile,
		Name:            "bom-self-test",
	}

	buildDoc, err := builder.GenerateBuildDoc(opts)
	require.NoError(t, err)
	require.NotNil(t, buildDoc)

	tvOutput, err := buildDoc.Render()
	require.NoError(t, err)
	require.NotEmpty(t, tvOutput)

	require.Contains(t, tvOutput, "SPDXVersion:", "tag-value output should contain SPDXVersion:")
	require.Contains(t, tvOutput, "SPDX-2.", "tag-value output should reference an SPDX-2.x version")

	require.True(t,
		strings.Contains(tvOutput, "build-deps") || strings.Contains(tvOutput, "build-dependencies"),
		"tag-value output should contain build-deps or build-dependencies in document name",
	)
}

// TestE2EParseRunLogsWithRealPatterns tests the log parser with realistic
// GitHub Actions log content to verify parsing and deduplication logic.
func TestE2EParseRunLogsWithRealPatterns(t *testing.T) {
	logFiles := map[string][]byte{
		"build/1_Set up job.txt": []byte(""),
		"build/2_Checkout.txt":   []byte(""),
		"build/3_Set up Go.txt":  []byte("Successfully set up Go version 1.22.0\nAdding to PATH\n"),
		"build/4_Build.txt": []byte(
			"go: downloading golang.org/x/tools v0.16.0\n" +
				"go: downloading golang.org/x/text v0.14.0\n",
		),
		"test/1_Set up job.txt": []byte(""),
		"test/2_Setup Go.txt":   []byte("Successfully set up Go version 1.22.0\n"),
		"test/3_Run tests.txt":  []byte("go install golang.org/x/tools/cmd/goimports@v0.16.0\n"),
	}

	deps := ParseRunLogs(logFiles)
	require.NotNil(t, deps)

	// Group deps by name
	byName := map[string][]ObservedDependency{}
	for _, d := range deps {
		byName[d.Name] = append(byName[d.Name], d)
	}

	// Go setup should appear exactly once (deduplication across build and test jobs)
	goDeps := byName["go"]
	require.Len(t, goDeps, 1, "Go setup should be deduplicated — appeared in build and test jobs")
	require.Equal(t, "1.22.0", goDeps[0].Version)
	require.Equal(t, "setup-action", goDeps[0].Source)

	// go module downloads should be present
	toolsDeps := byName["golang.org/x/tools"]
	require.NotEmpty(t, toolsDeps, "golang.org/x/tools should be found from go: downloading line")
	require.Equal(t, "0.16.0", toolsDeps[0].Version)

	textDeps := byName["golang.org/x/text"]
	require.NotEmpty(t, textDeps, "golang.org/x/text should be found from go: downloading line")
	require.Equal(t, "0.14.0", textDeps[0].Version)

	// go install should also be captured
	goImportsDeps := byName["golang.org/x/tools/cmd/goimports"]
	require.NotEmpty(t, goImportsDeps, "golang.org/x/tools/cmd/goimports should be found from go install line")
	require.Equal(t, "v0.16.0", goImportsDeps[0].Version)
	require.Equal(t, "go-install", goImportsDeps[0].Source)
}

// TestE2EBuildOutputFileSeparation verifies that:
//   - Generate() with BuildOutputFile set excludes workflows (produces only dir pkg)
//   - GenerateBuildDoc() produces build doc with workflow deps but not directory
func TestE2EBuildOutputFileSeparation(t *testing.T) {
	repoRoot := findRepoRoot(t)
	workflowDir := filepath.Join(repoRoot, ".github", "workflows")
	workflowPaths, err := filepath.Glob(filepath.Join(workflowDir, "*.y*ml"))
	require.NoError(t, err)
	require.NotEmpty(t, workflowPaths)

	// Use a real directory that exists (the workflows directory itself)
	tmpDir := t.TempDir()

	// Write a small directory with a placeholder file so PackageFromDirectory works
	srcDir := filepath.Join(tmpDir, "src")
	require.NoError(t, os.MkdirAll(srcDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "hello.txt"), []byte("hello"), 0o644))

	buildOutputFile := filepath.Join(tmpDir, "build-deps.spdx")

	builder := NewDocBuilder()
	opts := &DocGenerateOptions{
		Directories:     []string{srcDir},
		Workflows:       workflowPaths,
		BuildOutputFile: buildOutputFile,
		Name:            "separation-test",
	}

	// Generate main doc — should have the directory package but NOT workflow deps
	mainDoc, err := builder.Generate(opts)
	require.NoError(t, err)
	require.NotNil(t, mainDoc)

	// Main doc should have at least one package (the directory)
	require.NotEmpty(t, mainDoc.Packages, "main doc should contain the directory package")

	// Ensure no "build-dependencies" package in the main doc
	for _, pkg := range mainDoc.Packages {
		require.NotEqual(t, "build-dependencies", pkg.Name,
			"main doc should NOT contain build-dependencies when BuildOutputFile is set")
	}

	// Generate build doc — should have workflow deps
	buildDoc, err := builder.GenerateBuildDoc(opts)
	require.NoError(t, err)
	require.NotNil(t, buildDoc)
	require.Equal(t, "separation-test-build-deps", buildDoc.Name)
	require.NotEmpty(t, buildDoc.Packages, "build doc should contain packages from workflow scan")

	out, err := buildDoc.Render()
	require.NoError(t, err)
	require.NotEmpty(t, out)
}

// TestE2EWorkflowDependencyDetails verifies that specific expected actions are
// found when scanning the release.yml workflow.
func TestE2EWorkflowDependencyDetails(t *testing.T) {
	repoRoot := findRepoRoot(t)
	releasePath := filepath.Join(repoRoot, ".github", "workflows", "release.yml")

	_, err := os.Stat(releasePath)
	require.NoError(t, err, "release.yml should exist")

	pkg, err := WorkflowsToSPDXPackages([]string{releasePath})
	require.NoError(t, err)
	require.NotNil(t, pkg)

	relationships := pkg.GetRelationships()
	require.NotNil(t, relationships)

	// Build a map of package name → package for easy lookup
	depsByName := map[string]*Package{}
	for _, rel := range *relationships {
		if rel.Peer == nil {
			continue
		}
		p, ok := rel.Peer.(*Package)
		if !ok {
			continue
		}
		depsByName[p.Name] = p
	}

	// Define expected dependencies from release.yml
	type expectedDep struct {
		name        string
		versionHint string // partial match against version
	}
	expectedDeps := []expectedDep{
		{name: "actions/checkout", versionHint: "de0fac2e4500dabe0009e67214ff5f5447ce83dd"},
		{name: "actions/setup-go", versionHint: "7a3fe6cf4cb3a834922a1244abfce67bcef6a0c5"},
		{name: "sigstore/cosign-installer", versionHint: ""},
		{name: "goreleaser/goreleaser-action", versionHint: ""},
		{name: "magefile/mage-action", versionHint: ""},
		{name: "kubernetes-sigs/release-actions/setup-tejolote", versionHint: ""},
		{name: "softprops/action-gh-release", versionHint: ""},
	}

	for _, ed := range expectedDeps {
		t.Run(ed.name, func(t *testing.T) {
			p, found := depsByName[ed.name]
			require.True(t, found, "expected dependency %q not found in release.yml scan", ed.name)
			require.NotEmpty(t, p.Version, "version/ref for %q should not be empty", ed.name)
			if ed.versionHint != "" {
				require.Contains(t, p.Version, ed.versionHint,
					"version for %q should contain the expected SHA/ref", ed.name)
			}
		})
	}
}
