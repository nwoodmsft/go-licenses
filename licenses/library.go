// Copyright 2019 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package licenses

import (
	"context"
	"fmt"
	"go/build"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/golang/glog"
	"golang.org/x/tools/go/packages"
)

type RepoPathFixup struct {
	newHostName string
	prefix      string
}

// Library is a collection of packages covered by the same license file.
type Library struct {
	// LicensePath is the path of the file containing the library's license.
	LicensePath string
	// Packages contains import paths for Go packages in this library.
	// It may not be the complete set of all packages in the library.
	Packages []string
}

// SkippedLibrary represents a library which doesn't have a license file.
type SkippedLibrary struct {
	// Path to the library that was skipped.
	PackagePath string
	// Reason for skipping this library.
	Reason string
}

// PackagesError aggregates all Packages[].Errors into a single error.
type PackagesError struct {
	pkgs []*packages.Package
}

func (e PackagesError) Error() string {
	var str strings.Builder
	str.WriteString(fmt.Sprintf("errors for %q:", e.pkgs))
	packages.Visit(e.pkgs, nil, func(pkg *packages.Package) {
		for _, err := range pkg.Errors {
			str.WriteString(fmt.Sprintf("\n%s: %s", pkg.PkgPath, err))
		}
	})
	return str.String()
}

// Libraries returns the collection of libraries used by this package, directly or transitively.
// A library is a collection of one or more packages covered by the same license file.
// Packages not covered by a license will be returned as individual libraries.
// Standard library packages will be ignored.
func Libraries(ctx context.Context, classifier Classifier, importPaths ...string) ([]*Library, []*SkippedLibrary, error) {
	cfg := &packages.Config{
		Context: ctx,
		Mode:    packages.NeedImports | packages.NeedDeps | packages.NeedFiles | packages.NeedName,
	}

	rootPkgs, err := packages.Load(cfg, importPaths...)
	if err != nil {
		return nil, nil, err
	}

	var skippedLibraries []*SkippedLibrary
	pkgs := map[string]*packages.Package{}
	pkgsByLicense := make(map[string][]*packages.Package)
	errorOccurred := false
	packages.Visit(rootPkgs, func(p *packages.Package) bool {
		if len(p.Errors) > 0 {
			errorOccurred = true
			return false
		}
		if isStdLib(p) {
			// No license requirements for the Go standard library.
			skippedLibraries = append(skippedLibraries, &SkippedLibrary{PackagePath: p.PkgPath, Reason: "Go standard library that doesn't have any license requirement"})
			return false
		}
		if len(p.OtherFiles) > 0 {
			skippedLibraries = append(skippedLibraries, &SkippedLibrary{PackagePath: p.PkgPath, Reason: fmt.Sprintf("Contains non-Go code that can't be inspected for further dependencies: %s", strings.Join(p.OtherFiles, ", "))})
			//glog.Warningf("%q contains non-Go code that can't be inspected for further dependencies:\n%s", p.PkgPath, strings.Join(p.OtherFiles, "\n"))
		}
		var pkgDir string
		switch {
		case len(p.GoFiles) > 0:
			pkgDir = filepath.Dir(p.GoFiles[0])
		case len(p.CompiledGoFiles) > 0:
			pkgDir = filepath.Dir(p.CompiledGoFiles[0])
		case len(p.OtherFiles) > 0:
			pkgDir = filepath.Dir(p.OtherFiles[0])
		default:
			// This package is empty - nothing to do.
			return true
		}
		licensePath, err := Find(pkgDir, classifier)
		if err != nil {
			skippedLibraries = append(skippedLibraries, &SkippedLibrary{PackagePath: p.PkgPath, Reason: fmt.Sprintf("Failed to find license for %s: %v", p.PkgPath, err)})
			glog.Errorf("Failed to find license for %s: %v", p.PkgPath, err)
		}
		pkgs[p.PkgPath] = p
		pkgsByLicense[licensePath] = append(pkgsByLicense[licensePath], p)
		return true
	}, nil)

	if errorOccurred {
		return nil, nil, PackagesError{
			pkgs: rootPkgs,
		}
	}

	var libraries []*Library
	for licensePath, pkgs := range pkgsByLicense {
		if licensePath == "" {
			// No license for these packages - return each one as a separate library.
			for _, p := range pkgs {
				libraries = append(libraries, &Library{
					Packages: []string{p.PkgPath},
				})
			}
			continue
		}
		lib := &Library{
			LicensePath: licensePath,
		}
		for _, pkg := range pkgs {
			lib.Packages = append(lib.Packages, pkg.PkgPath)
		}
		libraries = append(libraries, lib)
	}
	return libraries, skippedLibraries, nil
}

// Name is the common prefix of the import paths for all of the packages in this library.
func (l *Library) Name() string {
	return commonAncestor(l.Packages)
}

func commonAncestor(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	if len(paths) == 1 {
		return paths[0]
	}
	sort.Strings(paths)
	min, max := paths[0], paths[len(paths)-1]
	lastSlashIndex := 0
	for i := 0; i < len(min) && i < len(max); i++ {
		if min[i] != max[i] {
			return min[:lastSlashIndex]
		}
		if min[i] == '/' {
			lastSlashIndex = i
		}
	}
	return min
}

func (l *Library) String() string {
	return l.Name()
}

// Golang project may end with a versioned path name (typically "/v2", "/v3", ...)
// The path to the license doesn't bear this versioned part, so it must be removed.
func (l *Library) tryRemoveVersionedName(input string) string {
	re := regexp.MustCompile(`/v\d+$`)
	input = strings.TrimSuffix(input, string(re.Find([]byte(input))))

	re = regexp.MustCompile(`^v\d+$`)
	return strings.TrimSuffix(input, string(re.Find([]byte(input))))
}

// The original file path may not exactly represent the actual URL to the LICENSE
// There is also a wide variety of fixups possible (each with slight differences)
// Paths must be therefore fixed up accordingly
func (l *Library) fixupFilePath(filePath string) (string, string, error) {
	relFilePath, err := filepath.Rel(filepath.Dir(l.LicensePath), filePath)
	if err != nil {
		return "", "", err
	}

	hostName := ""
	nameParts := strings.SplitN(l.Name(), "/", 2)
	if len(nameParts) > 0 {
		hostName = nameParts[0]
	}

	// TODO(RJPercival): Support replacing "master" with Go Module version
	switch hostName {
	case "github.com":
		nameParts = strings.SplitN(nameParts[1], "/", 3)
		if len(nameParts) < 2 {
			return "", "", fmt.Errorf("cannot determine URL for %q package", l.Name())
		}
		user, project := nameParts[0], nameParts[1]
		prefix := "blob/master/"
		if len(nameParts) == 3 {
			prefix = l.tryRemoveVersionedName(path.Join(prefix, nameParts[2]))
		}

		return "github.com", path.Join(user, project, prefix, relFilePath), nil
	case "gitlab.com":
		nameParts = strings.SplitN(nameParts[1], "/", 3)
		if len(nameParts) < 2 {
			return "", "", fmt.Errorf("cannot determine URL for %q package", l.Name())
		}
		user, project := nameParts[0], nameParts[1]
		suffix := "-/raw/master/"

		return "gitlab.com", path.Join(user, project, suffix, relFilePath), nil
	case "bitbucket.org":
		nameParts = strings.SplitN(nameParts[1], "/", 3)
		if len(nameParts) < 2 {
			return "", "", fmt.Errorf("cannot determine URL for %q package", l.Name())
		}
		user, project := nameParts[0], nameParts[1]
		prefix := "src/master/"
		if len(nameParts) == 3 {
			prefix = l.tryRemoveVersionedName(path.Join(prefix, nameParts[2]))
		}

		return "bitbucket.org", path.Join(user, project, prefix, relFilePath), nil
	case "k8s.io":
		nameParts = strings.SplitN(nameParts[1], "/", 2)
		if len(nameParts) < 1 {
			return "", "", fmt.Errorf("cannot determine URL for %q package", l.Name())
		}
		project := nameParts[0]
		prefix := "blob/master/"
		suffix := ""
		if len(nameParts) == 2 {
			suffix = l.tryRemoveVersionedName(nameParts[1])
		}

		return "github.com", path.Join("kubernetes", project, prefix, suffix, relFilePath), nil
	case "sigs.k8s.io":
		nameParts = strings.SplitN(nameParts[1], "/", 2)
		if len(nameParts) < 1 {
			return "", "", fmt.Errorf("cannot determine URL for %q package", l.Name())
		}
		project := nameParts[0]
		prefix := "blob/master/"
		suffix := ""
		if len(nameParts) == 2 {
			suffix = l.tryRemoveVersionedName(nameParts[1])
		}

		return "github.com", path.Join("kubernetes-sigs", project, prefix, suffix, relFilePath), nil
	case "gomodules.xyz":
		nameParts = strings.SplitN(nameParts[1], "/", 2)
		if len(nameParts) < 1 {
			return "", "", fmt.Errorf("cannot determine URL for %q package", l.Name())
		}
		project := nameParts[0]
		prefix := "blob/master/"
		suffix := ""
		if len(nameParts) == 2 {
			suffix = l.tryRemoveVersionedName(nameParts[1])
		}

		return "github.com", path.Join("gomodules", project, prefix, suffix, relFilePath), nil
	case "go.uber.org":
		nameParts = strings.SplitN(nameParts[1], "/", 2)
		if len(nameParts) < 1 {
			return "", "", fmt.Errorf("cannot determine URL for %q package", l.Name())
		}
		project := nameParts[0]
		prefix := "blob/master/"
		suffix := ""
		if len(nameParts) == 2 {
			suffix = l.tryRemoveVersionedName(nameParts[1])
		}

		return "github.com", path.Join("uber-go", project, prefix, suffix, relFilePath), nil
	case "go.etcd.io":
		nameParts = strings.SplitN(nameParts[1], "/", 2)
		if len(nameParts) < 1 {
			return "", "", fmt.Errorf("cannot determine URL for %q package", l.Name())
		}
		project := nameParts[0]
		prefix := "blob/master/"
		suffix := ""
		if len(nameParts) == 2 {
			suffix = l.tryRemoveVersionedName(nameParts[1])
		}

		return "github.com", path.Join("etcd-io", project, prefix, suffix, relFilePath), nil
	case "msazure.visualstudio.com":
		nameParts = strings.SplitN(nameParts[1], "/", 3)
		if len(nameParts) < 2 {
			return "", "", fmt.Errorf("cannot determine URL for %q package", l.Name())
		}
		project := nameParts[1]
		prefix := "_git/"
		suffix := ""
		if len(nameParts) == 3 {
			suffix = l.tryRemoveVersionedName(nameParts[2])
			suffix = strings.TrimSuffix(suffix, ".git")
		}
		suffix = strings.Join([]string{suffix, "?path=", relFilePath}, "")

		// "https://msazure.visualstudio.com/msk8s/_git/cloud-operator?path=LICENSE
		return "msazure.visualstudio.com", path.Join(project, prefix, suffix), nil
	case "dev.azure.com":
		nameParts = strings.SplitN(nameParts[1], "/", 3)
		if len(nameParts) < 2 {
			return "", "", fmt.Errorf("cannot determine URL for %q package", l.Name())
		}
		project := nameParts[1]
		prefix := "_git/"
		suffix := ""
		if len(nameParts) == 3 {
			suffix = l.tryRemoveVersionedName(nameParts[2])
			suffix = strings.TrimSuffix(suffix, ".git")
		}
		suffix = strings.Join([]string{suffix, "?path=", relFilePath}, "")

		return "dev.azure.com", path.Join(project, prefix, suffix), nil
	case "kubevirt.io":
		nameParts = strings.SplitN(nameParts[1], "/", 2)
		if len(nameParts) < 1 {
			return "", "", fmt.Errorf("cannot determine URL for %q package", l.Name())
		}
		project := nameParts[0]
		prefix := "blob/master/"

		return "github.com", path.Join("kubevirt", project, prefix, relFilePath), nil
	case "code.cloudfoundry.org":
		nameParts = strings.SplitN(nameParts[1], "/", 2)
		if len(nameParts) < 1 {
			return "", "", fmt.Errorf("cannot determine URL for %q package", l.Name())
		}
		project := nameParts[0]
		prefix := "blob/master/"

		return "github.com", path.Join("cloudfoundry", project, prefix, relFilePath), nil
	case "go.starlark.net":
		return "github.com", "github.com/google/starlark-go/LICENSE", nil
	case "cloud.google.com":
		// Main site for cloud.google.com: https://pkg.go.dev/cloud.google.com/go/compute/metadata
		return "github.com", "googleapis/google-cloud-go/LICENSE", nil
	case "helm.sh":
		nameParts = strings.SplitN(nameParts[1], "/", 2)
		if len(nameParts) < 1 {
			return "", "", fmt.Errorf("cannot determine URL for %q package", l.Name())
		}
		project := nameParts[0]
		prefix := "blob/master/"

		return "github.com", path.Join("helm", project, prefix, relFilePath), nil
	case "software.sslmate.com":
		nameParts = strings.SplitN(nameParts[1], "/", 3)
		if len(nameParts) < 1 {
			return "", "", fmt.Errorf("cannot determine URL for %q package", l.Name())
		}
		project := nameParts[1]
		prefix := "blob/master/"
		suffix := ""
		if len(nameParts) == 3 {
			suffix = l.tryRemoveVersionedName(nameParts[2])
		}
		return "github.com", path.Join("SSLMate", project, prefix, suffix, relFilePath), nil
	case "gopkg.in":
		// Main site for gopkg.in is: https://labix.org/gopkg.in, the license points to https://github.com/niemeyer/gopkg/blob/master/LICENSE
		return "github.com", "niemeyer/gopkg/blob/master/LICENSE", nil
	case "go.opencensus.io":
		licensePath := strings.Join([]string{l.Name(), "?tab=licenses"}, "")
		return "pkg.go.dev", licensePath, nil
	case "contrib.go.opencensus.io":
		licensePath := strings.Join([]string{l.Name(), "?tab=licenses"}, "")
		return "pkg.go.dev", licensePath, nil
	case "golang.zx2c4.com":
		licensePath := strings.Join([]string{l.Name(), "?tab=licenses"}, "")
		return "pkg.go.dev", licensePath, nil
	case "google.golang.org":
		fallthrough
	case "golang.org":
		return "", "", nil // Ignore golang packages
	}

	return "", "", fmt.Errorf("unsupported package host %q for %q. FilePath: '%v'", hostName, l.Name(), relFilePath)
}

// FileURL attempts to determine the URL for a file in this library.
// This only works for certain supported package prefixes, such as github.com,
// bitbucket.org and googlesource.com. Prefer GitRepo.FileURL() if possible.
func (l *Library) FileURL(filePath string) (*url.URL, error) {

	hostname, path, err := l.fixupFilePath(filePath)
	if err != nil {
		glog.Errorf("package host error [%v] for %v", err, l.Name())
		return nil, err
	}

	if len(hostname) == 0 { // This happens for golang packages. These packages come without a separate license
		return nil, nil
	}

	return &url.URL{
		Scheme: "https",
		Host:   hostname,
		Path:   path,
	}, nil
}

// isStdLib returns true if this package is part of the Go standard library.
func isStdLib(pkg *packages.Package) bool {
	if len(pkg.GoFiles) == 0 {
		return false
	}
	goroot := build.Default.GOROOT
	if !strings.HasSuffix(goroot, "/") {
		goroot += "/"
	}
	return strings.HasPrefix(pkg.GoFiles[0], goroot)
}
