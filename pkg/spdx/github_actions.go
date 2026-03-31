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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	purl "github.com/package-url/packageurl-go"
	"github.com/sirupsen/logrus"
	"sigs.k8s.io/yaml"
)

// Workflow represents a GitHub Actions workflow YAML file.
type Workflow struct {
	Name string                 `json:"name"`
	On   interface{}            `json:"on"` //nolint:revive // "on" matches the GitHub Actions schema
	Jobs map[string]WorkflowJob `json:"jobs"`
}

// WorkflowJob represents a single job in a GitHub Actions workflow.
type WorkflowJob struct {
	Name      string               `json:"name"`
	RunsOn    interface{}          `json:"runs-on"` // string or []string
	Container interface{}          `json:"container"`
	Services  map[string]Service   `json:"services"`
	Steps     []WorkflowStep       `json:"steps"`
	Uses      string               `json:"uses"` // reusable workflow reference
	Needs     interface{}          `json:"needs"`
	Strategy  *WorkflowJobStrategy `json:"strategy"`
}

// WorkflowJobStrategy represents a job's matrix strategy.
type WorkflowJobStrategy struct {
	Matrix interface{} `json:"matrix"`
}

// Service represents a service container in a GitHub Actions job.
type Service struct {
	Image string `json:"image"`
}

// WorkflowStep represents a single step in a GitHub Actions job.
type WorkflowStep struct {
	Name string                 `json:"name"`
	Uses string                 `json:"uses"`
	With map[string]interface{} `json:"with"`
	Run  string                 `json:"run"`
}

// ContainerConfig represents the container field which can be a string or object.
type ContainerConfig struct {
	Image string `json:"image"`
}

// ActionRef represents a parsed GitHub Actions reference.
type ActionRef struct {
	Owner    string // e.g., "actions"
	Repo     string // e.g., "checkout"
	Path     string // e.g., "setup-tejolote" (for owner/repo/subpath@ref)
	Ref      string // e.g., "v4" or a SHA
	IsDocker bool   // true if uses: docker://...
	IsLocal  bool   // true if uses: ./...
	Image    string // Docker image if IsDocker
	Raw      string
}

// ParseActionRef parses a GitHub Actions `uses` string into its components.
func ParseActionRef(uses string) ActionRef {
	ref := ActionRef{Raw: uses}

	// Docker action: docker://image:tag
	if strings.HasPrefix(uses, "docker://") {
		ref.IsDocker = true
		ref.Image = strings.TrimPrefix(uses, "docker://")
		return ref
	}

	// Local action: ./path
	if strings.HasPrefix(uses, "./") || strings.HasPrefix(uses, "../") {
		ref.IsLocal = true
		return ref
	}

	// Standard action or reusable workflow: owner/repo@ref or owner/repo/path@ref
	atIdx := strings.LastIndex(uses, "@")
	if atIdx == -1 {
		// No @ — malformed, treat as raw
		return ref
	}

	ref.Ref = uses[atIdx+1:]
	repoPath := uses[:atIdx]

	parts := strings.SplitN(repoPath, "/", 3)
	if len(parts) >= 2 {
		ref.Owner = parts[0]
		ref.Repo = parts[1]
	}
	if len(parts) == 3 {
		ref.Path = parts[2]
	}

	return ref
}

// IsSHA returns true if the ref looks like a full commit SHA (40 hex chars).
var shaRegexp = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (r ActionRef) IsSHA() bool {
	return shaRegexp.MatchString(r.Ref)
}

// FullName returns the owner/repo or owner/repo/path string.
func (r ActionRef) FullName() string {
	if r.Owner == "" || r.Repo == "" {
		return r.Raw
	}
	if r.Path != "" {
		return fmt.Sprintf("%s/%s/%s", r.Owner, r.Repo, r.Path)
	}
	return fmt.Sprintf("%s/%s", r.Owner, r.Repo)
}

// PackageURL returns a purl string for the action reference.
func (r ActionRef) PackageURL() string {
	if r.IsDocker {
		return dockerImagePURL(r.Image)
	}
	if r.IsLocal || r.Owner == "" {
		return ""
	}

	repoName := r.Repo
	if r.Path != "" {
		repoName = r.Repo + "/" + r.Path
	}

	return purl.NewPackageURL(
		"github", r.Owner, repoName,
		r.Ref, nil, "",
	).ToString()
}

// DownloadLocation returns the download URL for the action.
func (r ActionRef) DownloadLocation() string {
	if r.IsDocker {
		return "https://hub.docker.com/_/" + strings.Split(r.Image, ":")[0]
	}
	if r.IsLocal {
		return "NOASSERTION"
	}
	if r.Owner == "" || r.Repo == "" {
		return "NOASSERTION"
	}
	if r.IsSHA() {
		return fmt.Sprintf("https://github.com/%s/%s/commit/%s", r.Owner, r.Repo, r.Ref)
	}
	return fmt.Sprintf("https://github.com/%s/%s/releases/tag/%s", r.Owner, r.Repo, r.Ref)
}

// dockerImagePURL creates a purl for a Docker image reference.
func dockerImagePURL(image string) string {
	// Split image:tag or image@digest
	var name, version string
	if atIdx := strings.Index(image, "@"); atIdx != -1 {
		name = image[:atIdx]
		version = image[atIdx+1:]
	} else if colonIdx := strings.LastIndex(image, ":"); colonIdx != -1 {
		name = image[:colonIdx]
		version = image[colonIdx+1:]
	} else {
		name = image
		version = "latest"
	}

	// Split namespace and package name
	parts := strings.Split(name, "/")
	var namespace, pkgName string
	switch len(parts) {
	case 1:
		// Official image like "alpine"
		namespace = "library"
		pkgName = parts[0]
	case 2:
		// Could be registry/image or namespace/image
		if strings.Contains(parts[0], ".") {
			// Registry prefix like gcr.io/image — use full name
			namespace = parts[0]
			pkgName = parts[1]
		} else {
			namespace = parts[0]
			pkgName = parts[1]
		}
	default:
		// registry/namespace/image or deeper
		namespace = strings.Join(parts[:len(parts)-1], "/")
		pkgName = parts[len(parts)-1]
	}

	return purl.NewPackageURL(
		"docker", namespace, pkgName,
		version, nil, "",
	).ToString()
}

// ParseWorkflowFile reads and parses a GitHub Actions workflow YAML file.
func ParseWorkflowFile(path string) (*Workflow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading workflow file %s: %w", path, err)
	}
	return ParseWorkflowData(data)
}

// ParseWorkflowData parses workflow YAML bytes into a Workflow struct.
func ParseWorkflowData(data []byte) (*Workflow, error) {
	wf := &Workflow{}
	if err := yaml.Unmarshal(data, wf); err != nil {
		return nil, fmt.Errorf("parsing workflow YAML: %w", err)
	}
	return wf, nil
}

// WorkflowDependency represents a single extracted build dependency.
type WorkflowDependency struct {
	// Name is the human-readable dependency name.
	Name string
	// Version is the version/ref string.
	Version string
	// Type is the kind of dependency: "action", "docker-image", "runner-os",
	// "service-container", "reusable-workflow".
	Type string
	// DownloadLocation is the URL where the dependency can be obtained.
	DownloadLocation string
	// PURL is the Package URL for the dependency.
	PURL string
	// RelationshipType is the SPDX relationship type for this dependency.
	RelationshipType RelationshipType
	// PrimaryPurpose is the SPDX primary purpose of the package.
	PrimaryPurpose string
	// Comment provides additional context.
	Comment string
	// SourceJob is the name/key of the job that declared this dependency.
	SourceJob string
	// SourceStep is the name of the step that declared this dependency.
	SourceStep string
}

// ExtractWorkflowDependencies extracts all build dependencies from a parsed workflow.
func ExtractWorkflowDependencies(wf *Workflow) []WorkflowDependency {
	var deps []WorkflowDependency
	seen := map[string]bool{}

	for jobKey, job := range wf.Jobs {
		// 1. Runner OS
		runners := parseRunsOn(job.RunsOn)
		for _, runner := range runners {
			key := "runner:" + runner
			if !seen[key] {
				seen[key] = true
				deps = append(deps, WorkflowDependency{
					Name:             runner,
					Version:          "",
					Type:             "runner-os",
					DownloadLocation: "NOASSERTION",
					RelationshipType: BUILD_DEPENDENCY_OF,
					PrimaryPurpose:   "OPERATING-SYSTEM",
					Comment:          fmt.Sprintf("GitHub Actions runner for job %q", jobKey),
					SourceJob:        jobKey,
				})
			}
		}

		// 2. Job container
		containerImage := parseContainer(job.Container)
		if containerImage != "" {
			key := "container:" + containerImage
			if !seen[key] {
				seen[key] = true
				deps = append(deps, WorkflowDependency{
					Name:             containerImage,
					Version:          extractImageTag(containerImage),
					Type:             "docker-image",
					DownloadLocation: "https://hub.docker.com/_/" + strings.Split(containerImage, ":")[0],
					PURL:             dockerImagePURL(containerImage),
					RelationshipType: BUILD_DEPENDENCY_OF,
					PrimaryPurpose:   "CONTAINER",
					Comment:          fmt.Sprintf("Job container for job %q", jobKey),
					SourceJob:        jobKey,
				})
			}
		}

		// 3. Service containers
		for svcName, svc := range job.Services {
			if svc.Image != "" {
				key := "service:" + svc.Image
				if !seen[key] {
					seen[key] = true
					deps = append(deps, WorkflowDependency{
						Name:             svc.Image,
						Version:          extractImageTag(svc.Image),
						Type:             "service-container",
						DownloadLocation: "https://hub.docker.com/_/" + strings.Split(svc.Image, ":")[0],
						PURL:             dockerImagePURL(svc.Image),
						RelationshipType: BUILD_DEPENDENCY_OF,
						PrimaryPurpose:   "CONTAINER",
						Comment:          fmt.Sprintf("Service container %q for job %q", svcName, jobKey),
						SourceJob:        jobKey,
					})
				}
			}
		}

		// 4. Reusable workflow (job-level uses)
		if job.Uses != "" {
			ref := ParseActionRef(job.Uses)
			if !ref.IsLocal {
				key := "reusable:" + ref.FullName() + "@" + ref.Ref
				if !seen[key] {
					seen[key] = true
					deps = append(deps, WorkflowDependency{
						Name:             ref.FullName(),
						Version:          ref.Ref,
						Type:             "reusable-workflow",
						DownloadLocation: ref.DownloadLocation(),
						PURL:             ref.PackageURL(),
						RelationshipType: BUILD_TOOL_OF,
						Comment:          fmt.Sprintf("Reusable workflow for job %q", jobKey),
						SourceJob:        jobKey,
					})
				}
			}
		}

		// 5. Step actions
		for _, step := range job.Steps {
			if step.Uses == "" {
				continue
			}
			ref := ParseActionRef(step.Uses)
			if ref.IsLocal {
				continue // Skip local actions for now
			}

			key := "action:" + ref.FullName() + "@" + ref.Ref
			if ref.IsDocker {
				key = "docker-action:" + ref.Image
			}

			if !seen[key] {
				seen[key] = true
				dep := WorkflowDependency{
					Name:             ref.FullName(),
					Version:          ref.Ref,
					Type:             "action",
					DownloadLocation: ref.DownloadLocation(),
					PURL:             ref.PackageURL(),
					RelationshipType: BUILD_TOOL_OF,
					SourceJob:        jobKey,
					SourceStep:       step.Name,
				}
				if ref.IsDocker {
					dep.Name = ref.Image
					dep.Version = extractImageTag(ref.Image)
					dep.Type = "docker-image"
					dep.PrimaryPurpose = "CONTAINER"
					dep.RelationshipType = BUILD_DEPENDENCY_OF
				}
				deps = append(deps, dep)
			}
		}
	}

	return deps
}

// parseRunsOn extracts runner labels from the runs-on field.
func parseRunsOn(runsOn interface{}) []string {
	if runsOn == nil {
		return nil
	}
	switch v := runsOn.(type) {
	case string:
		return []string{v}
	case []interface{}:
		var runners []string
		for _, item := range v {
			if s, ok := item.(string); ok {
				runners = append(runners, s)
			}
		}
		return runners
	case map[string]interface{}:
		// Handle object form: runs-on: { group: ..., labels: ... }
		if labels, ok := v["labels"]; ok {
			return parseRunsOn(labels)
		}
		if group, ok := v["group"]; ok {
			if s, ok := group.(string); ok {
				return []string{s}
			}
		}
	}
	return nil
}

// parseContainer extracts the container image from the container field.
func parseContainer(container interface{}) string {
	if container == nil {
		return ""
	}
	switch v := container.(type) {
	case string:
		return v
	case map[string]interface{}:
		if image, ok := v["image"]; ok {
			if s, ok := image.(string); ok {
				return s
			}
		}
	}
	return ""
}

// extractImageTag extracts the tag/version from a Docker image reference.
func extractImageTag(image string) string {
	if atIdx := strings.Index(image, "@"); atIdx != -1 {
		return image[atIdx+1:]
	}
	if colonIdx := strings.LastIndex(image, ":"); colonIdx != -1 {
		return image[colonIdx+1:]
	}
	return "latest"
}

// WorkflowsToSPDXPackages converts workflow dependencies into SPDX packages
// and returns a top-level "build-deps" package containing them all.
func WorkflowsToSPDXPackages(workflowPaths []string) (*Package, error) {
	buildPkg := NewPackage()
	buildPkg.Options().Prefix = "build"
	buildPkg.Name = "build-dependencies"
	buildPkg.BuildID("build-dependencies")
	buildPkg.DownloadLocation = "NOASSERTION"
	buildPkg.LicenseConcluded = "NOASSERTION"
	buildPkg.PrimaryPurpose = "OTHER"

	allDeps := []WorkflowDependency{}

	for _, wfPath := range workflowPaths {
		matches, err := filepath.Glob(wfPath)
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

	seen := map[string]bool{}
	for i := range allDeps {
		dep := &allDeps[i]
		dedupeKey := dep.Type + ":" + dep.Name + "@" + dep.Version
		if seen[dedupeKey] {
			continue
		}
		seen[dedupeKey] = true

		pkg := depToSPDXPackage(dep)
		buildPkg.AddRelationship(&Relationship{
			FullRender: true,
			Type:       dep.RelationshipType,
			Peer:       pkg,
			Comment:    dep.Comment,
		})
	}

	logrus.Infof("Extracted %d unique build dependencies from workflows", len(seen))
	return buildPkg, nil
}

// depToSPDXPackage converts a WorkflowDependency into an SPDX Package.
func depToSPDXPackage(dep *WorkflowDependency) *Package {
	pkg := NewPackage()
	pkg.Options().Prefix = "build-" + dep.Type
	pkg.Name = dep.Name
	pkg.Version = dep.Version
	pkg.DownloadLocation = dep.DownloadLocation
	pkg.LicenseConcluded = "NOASSERTION"

	if dep.PrimaryPurpose != "" {
		pkg.PrimaryPurpose = dep.PrimaryPurpose
	}

	pkg.BuildID(dep.Name, dep.Version)

	if dep.PURL != "" {
		pkg.ExternalRefs = append(pkg.ExternalRefs, ExternalRef{
			Category: CatPackageManager,
			Type:     "purl",
			Locator:  dep.PURL,
		})
	}

	return pkg
}
