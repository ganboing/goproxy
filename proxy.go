package goproxy

import (
	"context"
	"net/http"
	"os"
	"path"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const UpstreamProxyScheme = "https"
const UpstreamProxyHost = "proxy.golang.org"
const UpstreamProxy = "https://proxy.golang.org"
const UpstreamProxyTimeout = 10 * time.Second
const DirectConnectTimeout = 10 * time.Second
const GitCloneTimeout = 20 * time.Minute
const GitLocalTimeout = 5 * time.Minute

type ProxyServer struct {
	Prefix          string
	initOnce        sync.Once
	pendingMod      sync.Map
	pendingGit      sync.Map
	gitClones       chan string
	gitCloneWorkers atomic.Int64
	mux             *http.ServeMux
}

func (p *ProxyServer) init() {
	numCpus := runtime.NumCPU()
	p.gitCloneWorkers.Store(int64(numCpus))
	p.gitClones = make(chan string, numCpus)
	p.mux = http.NewServeMux()
	if !strings.HasSuffix(p.Prefix, "/") {
		p.Prefix += "/"
	}
	p.mux.Handle(p.Prefix,
		http.StripPrefix(p.Prefix, http.HandlerFunc(p.monitorModFetch)))
	p.mux.Handle(p.Prefix+"cached-only/",
		http.StripPrefix(p.Prefix+"cached-only/", http.HandlerFunc(p.serveModCached)))
	os.MkdirAll(".gittemplate", 0700)
	os.MkdirAll(".tmp", 0700)
	os.Symlink("/dev/fd/3", ".tmp/zip-fd3.zip")
}

func (p *ProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.initOnce.Do(p.init)
	p.mux.ServeHTTP(w, r)
}

func (p *ProxyServer) tryServeCached(w http.ResponseWriter, modulePath, verSuffix, prop string) bool {
	gitDir := path.Join(modulePath, "git")
	getGitCmd(context.Background(), gitDir, "worktree", "list").Run()
	return false
}

func (p *ProxyServer) checkModVcsLocal(modulePath string) (string, string, string, error) {
	sep := len(modulePath)
	subPath := ""
	// Start with longest path first
	// Reason: golang.zx2c4.com/wireguard and golang.zx2c4.com/wireguard/wgctrl
	// Are all valid projects and backed by different repo
	for {
		parentPath := modulePath[:sep]
		vcsdir := path.Join(parentPath, ".vcs")
		target, err := os.Readlink(vcsdir)
		if err == nil {
			return parentPath, subPath, target, nil
		}
		sep = strings.LastIndexByte(parentPath, '/')
		if sep == -1 {
			return "", "", "", os.ErrNotExist
		}
		subPath = modulePath[sep+1:]
	}
}
