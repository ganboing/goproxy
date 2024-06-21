package goproxy

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"
)

const GitCommand = "git"

func getGitCmd(ctx context.Context, wkdir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, GitCommand, args...)
	cmd.Dir = wkdir
	return cmd
}

func getGitOutputCmd(ctx context.Context, wkdir string, args ...string) (*exec.Cmd, io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, GitCommand, args...)
	cmd.Dir = wkdir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	err = cmd.Start()
	if err != nil {
		defer stdout.Close()
		return nil, nil, err
	}
	return cmd, stdout, nil
}

func runGitOutputShort(ctx context.Context, wkdir string, args ...string) (string, error) {
	cmd, stdout, err := getGitOutputCmd(ctx, wkdir, args...)
	if err != nil {
		return "", err
	}
	defer stdout.Close()
	sb := strings.Builder{}
	io.Copy(&sb, stdout)
	err = cmd.Wait()
	if err != nil {
		return "", err
	}
	return sb.String(), nil
}

func createUnnamedTmpFile(dir string, perm uint32) (*os.File, error) {
	fd, err := unix.Open(dir, unix.O_RDWR|unix.O_TMPFILE|unix.O_CLOEXEC, perm)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), ""), nil
}

func collectGitArchiveOpts(gitdir, prefix, treeish, vertag string) ([]string, bool, error) {
	vendorExcludes := []string{
		// Upstream proxy doesn't fully respect https://go.dev/ref/mod#zip-path-size-constraints
		// It'll serve sigs.k8s.io/kubernetes@1.26.8.zip/vendor/modules.txt|OWNERS
		// Thus, we are only ignoring directories and non-go files in top-level vendor/
		":(exclude)vendor/*.go",
		":(exclude)vendor/*/**",
		":(exclude,top)**/vendor/*",
	}
	cmd, out, err := getGitOutputCmd(context.Background(), gitdir,
		append([]string{"archive", "--format=tar", treeish}, vendorExcludes...)...)
	if err != nil {
		return nil, false, errors.New(fmt.Sprintf("failed to start git archive (first pass): %s", err.Error()))
	}
	defer out.Close()
	tarReader := tar.NewReader(out)
	hasLicense := false
	hasVerLicense := false
	useVersionedDir := false
	var filteredPaths []string
	for {
		hdr, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false, errors.New(fmt.Sprintf("failed to parse git archive (first pass): %s", err.Error()))
		}
		io.Copy(io.Discard, tarReader)
		verLicense := vertag + "/LICENSE"
		switch hdr.Typeflag {
		case tar.TypeReg:
			break
		case tar.TypeDir:
			continue
		default:
			loggerYellow.Printf("collectGitArchiveOpts: ignoring %s for %s"+LOG_RST, hdr.Name, prefix)
			filteredPaths = append(filteredPaths, hdr.Name)
			//cmdArgs = append(cmdArgs,
			//	fmt.Sprintf(":(exclude,top)%s", hdr.Name))
			continue
		}
		if hdr.Name == "LICENSE" {
			hasLicense = true
		} else if hdr.Name == verLicense {
			hasVerLicense = true
		}
		if strings.HasSuffix(hdr.Name, "/go.mod") {
			if strings.TrimSuffix(hdr.Name, "/go.mod") == vertag {
				useVersionedDir = true
				continue
			}
			filteredPaths = append(filteredPaths, strings.TrimSuffix(hdr.Name, "go.mod"))
			//cmdArgs = append(cmdArgs,
			//	fmt.Sprintf(":(exclude,top)%s", strings.TrimSuffix(hdr.Name, "go.mod")))
		}
	}
	err = cmd.Wait()
	if err != nil {
		return nil, false, errors.New(fmt.Sprintf("git archive (first pass) returned error: %s", err.Error()))
	}
	if useVersionedDir {
		hasLicense = hasVerLicense
		// Git archive can take v1.2.3^{tree}:v4, but not v1.2.3^{tree}:/v4
		if !strings.HasSuffix(treeish, ":") {
			treeish += "/"
		}
		treeish += vertag
	}
	cmdArgs := []string{
		"archive", "--prefix", prefix, "--format=zip", "-0", treeish,
	}
	cmdArgs = append(cmdArgs, vendorExcludes...)
	for _, path := range filteredPaths {
		if !useVersionedDir {
			cmdArgs = append(cmdArgs, ":(exclude,top)"+path)
			continue
		}
		subPath, contain := strings.CutPrefix(path, vertag+"/")
		if !contain {
			continue
		}
		cmdArgs = append(cmdArgs, ":(exclude,top)"+subPath)
	}
	//log.Printf("git archive cmd args: %v", cmdArgs)
	return cmdArgs, hasLicense, nil
}
