package goproxy

import "time"

// An Origin describes the provenance of a given repo method result.
// It can be passed to CheckReuse (usually in a different go command invocation)
// to see whether the result remains up-to-date.
type Origin struct {
	VCS    string `json:",omitempty"` // "git" etc
	URL    string `json:",omitempty"` // URL of repository
	Subdir string `json:",omitempty"` // subdirectory in repo

	// If TagSum is non-empty, then the resolution of this module version
	// depends on the set of tags present in the repo, specifically the tags
	// of the form TagPrefix + a valid semver version.
	// If the matching repo tags and their commit hashes still hash to TagSum,
	// the Origin is still valid (at least as far as the tags are concerned).
	// The exact chksumdir is up to the Repo implementation; see (*gitRepo).Tags.
	TagPrefix string `json:",omitempty"`
	TagSum    string `json:",omitempty"`

	// If Ref is non-empty, then the resolution of this module version
	// depends on Ref resolving to the revision identified by Hash.
	// If Ref still resolves to Hash, the Origin is still valid (at least as far as Ref is concerned).
	// For Git, the Ref is a full ref like "refs/heads/main" or "refs/tags/v1.2.3",
	// and the Hash is the Git object hash the ref maps to.
	// Other VCS might choose differently, but the idea is that Ref is the name
	// with a mutable meaning while Hash is a name with an immutable meaning.
	Ref  string `json:",omitempty"`
	Hash string `json:",omitempty"`

	// If RepoSum is non-empty, then the resolution of this module version
	// failed due to the repo being available but the version not being present.
	// This depends on the entire state of the repo, which RepoSum summarizes.
	// For Git, this is a hash of all the refs and their hashes.
	RepoSum string `json:",omitempty"`
}

// A RevInfo describes a single revision in a module repository.
type RevInfo struct {
	Version string    // suggested version string for this revision
	Time    time.Time // commit time

	Origin *Origin `json:",omitempty"` // provenance for reuse
}

// metaImport represents the parsed <meta name="go-import"
// content="prefix vcs reporoot" /> tags from HTML files.
type MetaImport struct {
	Prefix, VCS, RepoRoot string
}
