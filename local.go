package goproxy

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"golang.org/x/sys/unix"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"
)

func (p *ProxyServer) serveModGit(modulePath, verMajorTag, subPath, verCanonical, ext string, incompat bool) (io.ReadCloser, error) {
	timestamp := time.Time{}
	refspec := verCanonical
	pseudoVer := module.IsPseudoVersion(verCanonical)
	if pseudoVer {
		timestamp, _ = module.PseudoVersionTime(verCanonical)
		timestamp = timestamp.In(time.UTC)
		refspec, _ = module.PseudoVersionRev(verCanonical)
	} else if subPath != "" {
		refspec = strings.Join([]string{subPath, refspec}, "/")
	}
	gitdir := path.Join(modulePath, ".git")
	var tm int64
retry_refspec:
	// Use git log to get commit timestamp, instead of git show.
	// Git show will spit out annotations for annotated tag
	unixTime, err := runGitOutputShort(context.Background(), gitdir,
		"log", "-1", "--format=%ct", refspec)
	if err == nil {
		tm, err = strconv.ParseInt(strings.TrimSpace(unixTime), 10, 64)
	}
	if err != nil {
		if !pseudoVer && subPath == "" && strings.HasPrefix(refspec, "v") {
			// This is necessary for some weird projects such as golang.zx2c4.com/wireguard
			// It doesn't follow the vX.Y.Z as tag names, rather the tag name is X.Y.Z
			// We need to try again if the vX.Y.Z tag fails
			// Currently let's limit this retrying only when there's no subPath
			refspec, _ = strings.CutPrefix(refspec, "v")
			goto retry_refspec
		}
		return nil, errors.New(
			fmt.Sprintf("failed to get commit date: %s", err.Error()))
	}
	timestampLocal := time.Unix(tm, 0).In(time.UTC)
	if !timestamp.IsZero() {
		// Check timestamp. Don't forget to enforce UTC timezone.
		if timestampLocal != timestamp {
			return nil, errors.New(fmt.Sprintf("timestamp mismatch: %s vs %s",
				timestamp.String(), timestampLocal.String()))
		}
	}
	ver := verCanonical
	if incompat {
		ver += "+incompatible"
	}
	modFull := modulePath
	if subPath != "" {
		modFull = strings.Join([]string{modFull, subPath}, "/")
	}
	if verMajorTag != "" {
		modFull = strings.Join([]string{modFull, verMajorTag}, "/")
	}
	if ext == ".info" {
		info := RevInfo{Time: timestampLocal.In(time.UTC), Version: ver}
		data, err := json.Marshal(info)
		if err != nil {
			return nil, errors.New(fmt.Sprintf("Failed to encode to json: %s", err.Error()))
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	} else if ext == ".mod" {
		// Try go.mod first
		treeish := refspec + "^{tree}:"
		if subPath != "" {
			treeish += subPath + "/"
		}
		cmdArgs := []string{
			"archive", "--format=tar", treeish, "go.mod",
		}
		if verMajorTag != "" {
			// Try vN/go.mod
			cmdArgs[2] += verMajorTag
		}
	retry_mod:
		cmd, out, err := getGitOutputCmd(
			context.Background(), gitdir, cmdArgs...)
		if err != nil {
			return nil, errors.New(
				fmt.Sprintf("Failed to run git archive (%s) %s: %s", cmdArgs[3], refspec, err.Error()))
		}
		defer out.Close()
		data, err := getSingleFileFromTar(out, "go.mod", tar.TypeReg)
		err2 := cmd.Wait()
		if err2 == nil && err == nil {
			return io.NopCloser(bytes.NewReader(data)), nil
		}
		if cmdArgs[2] != treeish {
			cmdArgs[2] = treeish
			goto retry_mod
		}
		loggerYellow.Printf("serveModGit: Using synthesized go.mod for %s"+LOG_RST, modulePath)
		// If reached here, it means the project doesn't provide go.mod, synthesize one
		mod := fmt.Sprintf("module %s\n", modFull)
		return io.NopCloser(bytes.NewReader([]byte(mod))), nil
	} else if ext == ".zip" {
		prefix := strings.Join([]string{modFull, ver}, "@") + "/"
		// First pass: Collect files with only vendor directory excluded
		// This will help determine if more files needs to be excluded, and
		// check if module is in the versioned (v1/v2...) directory
		cmdArgs, hasLicense, err := collectGitArchiveOpts(gitdir, prefix, refspec+"^{tree}:"+subPath, verMajorTag)
		if err != nil {
			return nil, err
		}
		// Second pass: actual archiving
		archiveTmp, err := createUnnamedTmpFile(".tmp", 0600)
		if err != nil {
			return nil, errors.New(fmt.Sprintf("failed to create temp file (archive): %s", err.Error()))
		}
		// After this, archiveTmp should be closed if error to prevent fd leak
		// loggerGreen.Printf("serveModGit: Archiving: %v"+LOG_RST, cmdArgs)
		cmd := getGitCmd(context.Background(), gitdir, cmdArgs...)
		cmd.Stderr = os.Stderr
		cmd.Stdout = archiveTmp
		err = cmd.Run()
		archiveTmp.Seek(0, io.SeekStart)
		if err != nil {
			archiveTmp.Close()
			return nil, errors.New(fmt.Sprintf("failed to run git archive (second pass): %s", err.Error()))
		}
		// Third pass: Remove directory entries
		// Zip is really annoying in that the zip file name has to end with .zip suffix.
		// Thus, we can't use /dev/fd/3. .tmp/zip-fd3.zip is essentially a symlink to /dev/fd/3
		// Removing directory entries is necessary otherwise the module zip checksum will mismatch against sumdb
		cmd = exec.Command("zip", "-d", ".tmp/zip-fd3.zip", "*/")
		cmd.Stderr = os.Stderr
		cmd.ExtraFiles = append(cmd.ExtraFiles, archiveTmp)
		err = cmd.Run()
		archiveTmp.Seek(0, io.SeekStart)
		exitErr, ok := err.(*exec.ExitError)
		if err != nil && (!ok || exitErr.ExitCode() != 12) {
			// Exit code 12 is "nothing to do" for zip
			archiveTmp.Close()
			return nil, errors.New(fmt.Sprintf("failed to trim zip file (third pass): %s", err.Error()))
		}
		if hasLicense || (subPath == "" && verMajorTag == "") {
			// If there's no license in submod/LICENSE, v4/LICENSE, submod/v4/LICENSE
			// We need to do Fourth pass, else return
			return archiveTmp, nil
		}
		// Fourth pass (optional): try to add LICENSE file from parent repo if missing
		cmd, out, err := getGitOutputCmd(
			context.Background(), gitdir, "archive", "--format=tar", refspec+"^{tree}", "LICENSE")
		if err != nil {
			return nil, errors.New(fmt.Sprintf("failed to run git archive (LICENSE) %s: %s", refspec, err.Error()))
		}
		defer out.Close()
		licDir := path.Join(".tmp/licenses", prefix)
		os.MkdirAll(licDir, 0700)
		licPath := path.Join(licDir, "LICENSE")
		err = unix.Access(licPath, unix.O_RDONLY)
		if err != nil {
			licenseTmp, err := createUnnamedTmpFile(licDir, 0600)
			if err != nil {
				archiveTmp.Close()
				return nil, errors.New(fmt.Sprintf("failed to create temp file (LICENSE): %s", err.Error()))
			}
			defer licenseTmp.Close()
			err = copySingleFileFromTar(out, licenseTmp, "LICENSE", tar.TypeReg)
			if err != nil {
				loggerYellow.Printf("serveModGit: LICENSE file not found for %s (ignored)"+LOG_RST, modulePath)
				return archiveTmp, nil
			}
			// This allows atomic creation of LICENSE, otherwise if we create the file first and write to it,
			// Other threads could observe partial file
			unix.Linkat(unix.AT_FDCWD, fmt.Sprintf("/dev/fd/%d", licenseTmp.Fd()), unix.AT_FDCWD, licPath, unix.AT_SYMLINK_FOLLOW)
			// error is ignored here. If there's one, it's usually EEXIST
		}
		cmd = exec.Command("zip", "-g", "../zip-fd3.zip", path.Join(prefix, "LICENSE"))
		cmd.Stderr = os.Stderr
		cmd.Stdout = os.Stdout
		cmd.Dir = ".tmp/licenses"
		cmd.ExtraFiles = append(cmd.ExtraFiles, archiveTmp)
		err = cmd.Run()
		if err != nil {
			archiveTmp.Close()
			return nil, errors.New(fmt.Sprintf("failed to append LICENSE to zip: %s", err.Error()))
		}
		archiveTmp.Seek(0, io.SeekStart)
		// error is ignored here.
		return archiveTmp, nil
	}
	return nil, nil
}

func (p *ProxyServer) serveModPlain(modulePath, verMajorTag, subPath, verCanonical, ext string, incompat bool) (io.ReadSeekCloser, error) {
	return nil, errors.New("NOT IMPLEMENTED")
}

func (p *ProxyServer) serveModLocal(modulePath, verMajorTag, verCanonical, ext string, incompat bool) (io.ReadCloser, error) {
	parentPath, subPath, vcs, err := p.checkModVcsLocal(modulePath)
	if err != nil {
		return nil, errors.New(
			fmt.Sprintf("cached module %s not found: %s", modulePath, err.Error()))
	}
	modulePath = parentPath
	switch vcs {
	case ".git":
		return p.serveModGit(modulePath, verMajorTag, subPath, verCanonical, ext, incompat)
	case ".mod":
		return p.serveModPlain(modulePath, verMajorTag, subPath, verCanonical, ext, incompat)
	}
	log.Panicf("Invalid local VCS type %s for module %s, should not happen", vcs, modulePath)
	return nil, nil
}

func (p *ProxyServer) serveModCached(w http.ResponseWriter, r *http.Request) {
	escapedModulePath, prop, ok := parseRequest(w, r)
	if !ok {
		return
	}
	ext := path.Ext(prop)
	var contentTy string
	switch ext {
	case ".info":
		contentTy = "application/json"
	case ".mod":
		contentTy = "text/plain; charset=UTF-8"
	case ".zip":
		contentTy = "application/zip"
	default:
		// For cached only mode, we do not provide @latest or @v/list
		// The project must request explicit version of its dependencies
		err := errors.New(fmt.Sprintf("Invalid URL path: %s", r.URL.Path))
		httpRespString(w, http.StatusInternalServerError, err.Error())
		return
	}
	ver := prop[:len(prop)-len(ext)]
	modulePath, err := module.UnescapePath(escapedModulePath)
	if err != nil {
		httpRespString(w, http.StatusInternalServerError, err.Error())
		return
	}
	modulePathTrim, verMajorTag, incompat, ok := checkModulePathVer(modulePath, ver)
	if !ok {
		httpRespString(w, http.StatusInternalServerError,
			fmt.Sprintf("module path/ver %s[%s] is invalid or not supported", modulePath, ver))
		return
	}
	modulePath = modulePathTrim
	ver = semver.Canonical(ver)
	reader, err := p.serveModLocal(modulePath, verMajorTag, ver, ext, incompat)
	if err != nil {
		httpRespString(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer reader.Close()
	// Set Content-Length if the reader is seekable
	seeker, seekable := reader.(io.Seeker)
	if seekable {
		off, err := seeker.Seek(0, io.SeekEnd)
		if err == nil {
			_, err = seeker.Seek(0, io.SeekStart)
		}
		if err != nil {
			httpRespString(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Length", strconv.FormatInt(off, 10))
	}
	w.Header().Set("Content-Type", contentTy)
	w.WriteHeader(http.StatusOK)
	io.Copy(w, reader)
}
