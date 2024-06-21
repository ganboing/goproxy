package goproxy

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
)

func (p *ProxyServer) gitCloneWorkerFunc(modulePath, remote string) {
	if remote == "" {
		loggerGreen.Printf("cacheModGit: Updating %s"+LOG_RST, modulePath)
		ctx, cancel := context.WithTimeout(context.Background(), GitCloneTimeout)
		defer cancel()
		cmd := getGitCmd(ctx, path.Join(modulePath, ".git"), "remote", "update")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Run()
		return
	}
	err := os.MkdirAll(modulePath, 0755)
	if err != nil {
		loggerRed.Printf("cacheModGit: Failed to create module directory: %s"+LOG_RST, err.Error())
		return
	}
	// Start cloning remote
	gitdir := path.Join(modulePath, ".git")
	// Clone to temporary directory and later rename it back to git (atomicity)
	tmpdir, err := os.MkdirTemp(modulePath, ".gittmp")
	if err != nil {
		loggerRed.Printf("cacheModGit: failed to create temp git dir: %s"+LOG_RST, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), GitCloneTimeout)
	defer cancel()
	loggerGreen.Printf("cacheModGit: Git cloning to %s from %s"+LOG_RST, tmpdir, remote)
	// Clone to temp directory first
	err = getGitCmd(ctx, ".", "clone", "--template=.gittemplate", "--quiet", "--mirror", remote, tmpdir).Run()
	if err != nil {
		loggerGreen.Printf("cacheModGit: Failed to git clone from %s"+LOG_RST, remote)
		os.RemoveAll(tmpdir)
		return
	}
	// If rename failed, we are racing with others, abort
	err = os.Rename(tmpdir, gitdir)
	if err != nil {
		loggerYellow.Printf("cacheModGit: gitdir %s already exists, cleaning up"+LOG_RST, gitdir)
		os.RemoveAll(tmpdir)
		return
	}
	// Should be successful
	err = os.Symlink(".git", path.Join(modulePath, ".vcs"))
	if err != nil {
		loggerRed.Printf("cacheModGit: Failed to create .vcs" + LOG_RST)
	} else {
		loggerGreen.Printf("cacheModGit: Done cloning %s"+LOG_RST, remote)
	}
}

func (p *ProxyServer) gitCloneWorker() {
	for {
		modulePath := <-p.gitClones
		v, loaded := p.pendingGit.Load(modulePath)
		if !loaded {
			log.Panicf("pendingGit must have %s", modulePath)
		}
		p.gitCloneWorkerFunc(modulePath, v.(string))
		p.pendingGit.Delete(modulePath)
	}
}

func (p *ProxyServer) cacheModGit(modulePath, subPath, ver, remote string) {
	if remote == "" {
		// The local repo already exists. Check if we have the version locally
		refspec := semver.Canonical(ver)
		pseudoVer := module.IsPseudoVersion(refspec)
		if pseudoVer {
			refspec, _ = module.PseudoVersionRev(refspec)
		} else if subPath != "" {
			refspec = strings.Join([]string{subPath, refspec}, "/")
		}
		gitdir := path.Join(modulePath, ".git")
	retry_refspec:
		cmd := getGitCmd(context.Background(), gitdir, "log", "-1", "--format=%H", refspec)
		err := cmd.Run()
		if err != nil {
			if !pseudoVer && subPath == "" && strings.HasPrefix(refspec, "v") {
				// This is necessary for some weird projects such as golang.zx2c4.com/wireguard
				// It doesn't follow the vX.Y.Z as tag names, rather the tag name is X.Y.Z
				// We need to try again if the vX.Y.Z tag fails
				// Currently let's limit this retrying only when there's no subPath
				refspec, _ = strings.CutPrefix(refspec, "v")
				goto retry_refspec
			}
		}
		if err == nil {
			// The tag/commit exists, just return
			return
		}
	}
	loggerGreen.Printf("cacheModGit: Trying to create/update gitdir for %s, remote=%s, ver=%s"+LOG_RST,
		modulePath, remote, ver)
	_, running := p.pendingGit.LoadOrStore(modulePath, remote)
	if running {
		loggerGreen.Printf("cacheModGit: Git clone/update %s already running"+LOG_RST, remote)
		return
	}
	if p.gitCloneWorkers.Add(-1) < 0 {
		p.gitCloneWorkers.Add(1)
		// gitCloneWorkers is an Int64, Technically it's nearly impossible to underflow
	} else {
		go p.gitCloneWorker()
		loggerGreen.Printf("cacheModGit: Starting git clone worker" + LOG_RST)
	}
	// It's OK if we get blocked here. We should be invoked in a go routine that's separate from the HTTP worker
	p.gitClones <- modulePath
}

func (p *ProxyServer) cacheModPlain(modulePath, subPath, ver string) {

}

func (p *ProxyServer) refreshModPathVer(key, escapedModulePath, modulePath, ver string) {
	defer p.pendingMod.Delete(key)
	modulePath, _, _, ok := checkModulePathVer(modulePath, ver)
	if !ok {
		loggerYellow.Printf("refreshModPathVer: module path '%s' is invalid"+LOG_RST, modulePath)
		return
	}
	parentPath, subPath, vcs, err := p.checkModVcsLocal(modulePath)
	if err == nil {
		// Module already exist locally, try to refresh the cache if version is missing
		modulePath = parentPath
		switch vcs {
		case ".git":
			p.cacheModGit(modulePath, subPath, ver, "")
			return
		case ".mod":
			p.cacheModPlain(modulePath, subPath, ver)
			return
		}
		log.Panicf("Invalid local VCS type %s for module %s, should not happen", vcs, modulePath)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), UpstreamProxyTimeout)
	defer cancel()
	info, err := checkEsModulePathUpstream(ctx, escapedModulePath)
	if err != nil {
		loggerRed.Printf("refreshModPathVer: failed to check module path on upstream: %s"+LOG_RST, err.Error())
		return
	}
	if info.Origin != nil {
		// Upstream proxy provides the repo link, use that
		subPath = info.Origin.Subdir
		modulePath = strings.TrimRight(strings.TrimSuffix(modulePath, subPath), "/")
		if info.Origin.VCS == "git" {
			p.cacheModGit(modulePath, subPath, ver, info.Origin.URL)
		} else {
			p.cacheModPlain(modulePath, subPath, ver)
		}
		return
	}
	// Now we'll have to get the repo link ourselves
	prefix, imports, err := searchModuleVcsDirect(modulePath)
	if err != nil {
		loggerRed.Printf("refreshModPathVer: Cannot find go-import paths for %s: %s"+LOG_RST, modulePath, err.Error())
		return
	}
	subPath = strings.TrimLeft(strings.TrimPrefix(modulePath, prefix), "/")
	modulePath = prefix
	loggerGreen.Printf("refreshModPathVer: go-import found: modulepath=%s, subpath=%s"+LOG_RST, modulePath, subPath)
	for _, im := range imports {
		if im.VCS == "git" {
			p.cacheModGit(modulePath, subPath, ver, im.RepoRoot)
			return
		}
		loggerYellow.Printf("refreshModPathVer: Ignoring go-import: %s %s %s"+LOG_RST, im.Prefix, im.VCS, im.RepoRoot)
	}
	loggerYellow.Printf("refreshModPathVer: %s is not git vcs, will have to fetch files from proxy"+LOG_RST, modulePath)
	p.cacheModPlain(modulePath, subPath, ver)
}

func (p *ProxyServer) processEsModPathVer(key, escapedModulePath, ver string) error {
	// key is the URL without splitting, but with extension removed,
	// such as golang.org/x/tools/gopls@v0.6.4.zip
	// This helps avoid duplicate work
	modulePath, err := module.UnescapePath(escapedModulePath)
	if err != nil {
		return err
	}
	_, existing := p.pendingMod.LoadOrStore(key, struct{}{})
	if existing {
		// Other threads already handling the jobs
		return nil
	}
	go p.refreshModPathVer(key, escapedModulePath, modulePath, ver)
	return nil
}

func (p *ProxyServer) monitorModFetch(w http.ResponseWriter, r *http.Request) {
	escapedModulePath, prop, ok := parseRequest(w, r)
	if !ok {
		return
	}
	ext := path.Ext(prop)
	switch ext {
	case ".info", ".mod", ".zip":
		ver := prop[:len(prop)-len(ext)]
		key := r.URL.Path[:len(r.URL.Path)-len(ext)]
		err := p.processEsModPathVer(key, escapedModulePath, ver)
		if err != nil {
			httpRespString(w, http.StatusInternalServerError, err.Error())
			return
		}
	case "":
		// Just redirect. We are not interested in these
		if prop == "latest" || prop == "list" {
			break
		}
		fallthrough
	default:
		err := errors.New(fmt.Sprintf("Invalid URL path: %s", r.URL.Path))
		httpRespString(w, http.StatusInternalServerError, err.Error())
		return
	}
	redirectToUpstream(w, r)
	return
}
