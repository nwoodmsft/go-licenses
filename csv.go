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

package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"strings"

	"github.com/golang/glog"
	"github.com/jocelynberrendonner/go-licenses/licenses"
	"github.com/spf13/cobra"
)

var (
	csvCmd = &cobra.Command{
		Use:   "csv <package>",
		Short: "Prints or saves all licenses that apply to a Go package and its dependencies",
		Args:  cobra.MinimumNArgs(1),
		RunE:  csvMain,
	}

	gitRemotes          []string
	csvFileName         string
	skippedLibsFileName string
)

func init() {
	csvCmd.Flags().StringArrayVar(&gitRemotes, "git_remote", []string{"origin", "upstream"}, "Remote Git repositories to try")
	csvCmd.Flags().StringVar(&csvFileName, "output", "", "Location of a file to save the license information to")
	csvCmd.Flags().StringVar(&skippedLibsFileName, "skipped_libs_path", "", "Location of a file to save the skipped skipped licenses to")

	if err := csvCmd.MarkFlagFilename("output"); err != nil {
		glog.Fatal(err)
	}

	if err := csvCmd.MarkFlagFilename("skipped_libs_path"); err != nil {
		glog.Fatal(err)
	}

	rootCmd.AddCommand(csvCmd)
}

func csvMain(_ *cobra.Command, args []string) error {
	var writer *csv.Writer
	if len(csvFileName) == 0 {
		writer = csv.NewWriter(os.Stdout)
	} else {
		f, err := os.Create(csvFileName)
		if err != nil {
			return err
		}
		writer = csv.NewWriter(f)
		defer f.Close()
	}

	var skippedLibsWriter *csv.Writer
	if len(skippedLibsFileName) == 0 {
		skippedLibsWriter = csv.NewWriter(os.Stdout)
	} else {
		f, err := os.Create(skippedLibsFileName)
		if err != nil {
			return err
		}
		skippedLibsWriter = csv.NewWriter(f)
		defer f.Close()
	}

	fmt.Printf("Generating CSV file for '%v'...\n", args[0])

	classifier, err := licenses.NewClassifier(confidenceThreshold)
	if err != nil {
		return err
	}

	libs, skippedLibs, err := licenses.Libraries(context.Background(), classifier, args...)
	if err != nil {
		return err
	}

	for _, lib := range libs {
		licenseURL := "Unknown"
		licenseName := "Unknown"
		if lib.LicensePath != "" {
			// Find a URL for the license file, based on the URL of a remote for the Git repository.
			var errs []string
			repo, err := licenses.FindGitRepo(lib.LicensePath)
			if err != nil {
				// Can't find Git repo (possibly a Go Module?) - derive URL from lib name instead.
				lURL, err := lib.FileURL(lib.LicensePath)
				if err != nil {
					errs = append(errs, err.Error())
				} else if lURL != nil {
					licenseURL = lURL.String()
				} else {
					licenseURL = "n/a (included in golang)"
					// else this is a file we can ignore
				}
			} else {
				for _, remote := range gitRemotes {
					url, err := repo.FileURL(lib.LicensePath, remote)
					if err != nil {
						errs = append(errs, err.Error())
						continue
					}
					licenseURL = url.String()
					break
				}
			}
			if licenseURL == "Unknown" {
				glog.Errorf("\nError discovering URL for %q:\n- %s\n\n", lib.LicensePath, strings.Join(errs, "\n- "))
			}
			licenseName, _, err = classifier.Identify(lib.LicensePath)
			if err != nil {
				glog.Errorf("Error identifying license in %q: %v", lib.LicensePath, err)
				licenseName = "Unknown"
			}
		}
		// Remove the "*/vendor/" prefix from the library name for conciseness.
		if err := writer.Write([]string{unvendor(lib.Name()), licenseURL, licenseName}); err != nil {
			return err
		}
	}

	for _, lib := range skippedLibs {
		if err := skippedLibsWriter.Write([]string{lib.PackagePath, lib.Reason}); err != nil {
			return err
		}
	}

	fmt.Printf("Processed %d Golang licenses. Skipped %d Golang libraries\n", len(libs), len(skippedLibs))

	skippedLibsWriter.Flush()
	writer.Flush()
	return writer.Error()
}
