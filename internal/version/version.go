// Package version provides version information for kubetnl.
package version

import "strings"

var (
	// Release version of kubetnl.
	version = "0.2.0"

	// NOTE: The $Format strings are replaced during 'git archive' thanks
	// to the companion .gitattributes file containing 'export-subst' in
	// this same directory.  See also
	// https://git-scm.com/docs/gitattributes
	gitCommit = "$Format:%H$" // sha1 from git, output of $(git rev-parse HEAD)
)

// Info holds version information for kubetnl.
type Info struct {
	// Version is the release version of kubetnl. Format follows the rules
	// of semantic versioning.
	Version string

	// GitCommit is the git commit SHA the kubetnl binary was build against.
	GitCommit string
}

// Get returns the current version information.
func Get() Info {
	comm := gitCommit
	// HACK: Check if the build happens neither via a go build
	// github.com/fischor/kubetnl or go install github.com/fischor/kubentl
	// nor with provided ldflags.
	chars := strings.Split(gitCommit, "")
	if len(chars) == 11 && chars[0] == "$" && chars[10] == "$" {
		comm = "unknown commit"
	}
	return Info{
		Version:   version,
		GitCommit: comm,
	}
}

// String returns i.Version.
func (i Info) String() string {
	return i.Version
}
