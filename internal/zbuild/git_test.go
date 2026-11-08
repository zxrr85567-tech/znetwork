package zbuild

import "os/exec"

// gitCmd builds a git command rooted at dir with a deterministic environment.
func gitCmd(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(),
		"GIT_AUTHOR_DATE=2020-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2020-01-01T00:00:00Z",
		"LC_ALL=C",
	)
	return cmd
}
