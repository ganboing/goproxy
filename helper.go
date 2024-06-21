package goproxy

import (
	"archive/tar"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"golang.org/x/mod/semver"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
)

const LOG_RED = "\033[0;31m"
const LOG_GRN = "\033[0;32m"
const LOG_YEL = "\033[0;33m"
const LOG_RST = "\033[0m"

var loggerRed = log.New(os.Stderr, LOG_RED, log.LstdFlags)
var loggerGreen = log.New(os.Stderr, LOG_GRN, log.LstdFlags)
var loggerYellow = log.New(os.Stderr, LOG_YEL, log.LstdFlags)

func forwardHttpResp(w http.ResponseWriter, resp *http.Response) {
	hdrContentType := resp.Header.Get("Content-Type")
	hdrContentLength := resp.Header.Get("Content-Length")
	if hdrContentType == "" || hdrContentLength == "" {
		io.Copy(io.Discard, resp.Body)
		httpRespString(w, http.StatusInternalServerError,
			"Upstream server failed to provide Content-Type/Length")
		return
	}
	w.Header().Set("Content-Type", hdrContentType)
	w.Header().Set("Content-Length", hdrContentLength)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func httpRespString(w http.ResponseWriter, code int, resp string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	w.Write([]byte(resp))
}

func parseRequest(w http.ResponseWriter, r *http.Request) (escapedModulePath string, prop string, ok bool) {
	if strings.HasPrefix(r.URL.Path, "sumdb/") {
		httpRespString(w, http.StatusNotFound, "not found")
		return "", "", false
	}
	escapedModulePath, prop, ok = strings.Cut(r.URL.Path, "/@v/")
	if !ok {
		prop = "latest"
		escapedModulePath, ok = strings.CutSuffix(r.URL.Path, "/@latest")
	}
	if !ok {
		err := errors.New(fmt.Sprintf("Unsupported URL path: %s", r.URL.Path))
		httpRespString(w, http.StatusInternalServerError, err.Error())
		return "", "", false
	}
	return
}

func redirectToUpstream(w http.ResponseWriter, r *http.Request) {
	url := *r.URL
	url.Scheme = UpstreamProxyScheme
	url.Host = UpstreamProxyHost
	http.Redirect(w, r, url.String(), http.StatusMovedPermanently)
}

// Does not handle gopkg.in/
func splitModuleMajorVer(modulePath string) (string, string, bool) {
	components := strings.Split(modulePath, "/")
	for _, comp := range components {
		// cleanPath in mux will sanitize all . .. and multi /
		// But we still need to check if any component begins with . just to be safe
		if len(comp) == 0 || comp[0] == '.' {
			return "", "", false
		}
	}
	if len(components) < 2 {
		return modulePath, "", true
	}
	last := components[len(components)-1]
	if len(last) == 0 {
		return "", "", false
	}
	if last[0] != 'v' {
		return modulePath, "", true
	}
	_, err := strconv.Atoi(last[1:])
	if err != nil {
		return modulePath, "", true
	}
	return strings.Join(components[:len(components)-1], "/"), last, true
}

func checkModulePathVer(modulePath, ver string) (path string, major string, incompat bool, ok bool) {
	incompat = semver.Build(ver) == "+incompatible"
	if strings.HasPrefix(modulePath, "gopkg.in/") {
		if incompat {
			return
		}
		// gopkg.in modules must end with .vN, such as gopkg.in/yaml.v2
		idx := strings.LastIndexByte(modulePath, '.')
		if idx != -1 && strings.HasPrefix(ver, modulePath[idx+1:]) {
			return modulePath, "", false, true
		}
		return
	}
	path, major, ok = splitModuleMajorVer(modulePath)
	if !ok {
		return
	}
	if major == "" && !strings.HasPrefix(ver, "v0.") && !strings.HasPrefix(ver, "v1.") && !incompat {
		return
	}
	return path, major, incompat, true
}

func checkEsModulePathUpstream(ctx context.Context, escapedModulePath string) (RevInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/%s/@latest", UpstreamProxy, escapedModulePath), nil)
	if err != nil {
		return RevInfo{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return RevInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		if err == nil {
			err = errors.New(string(body))
		}
		return RevInfo{}, err
	}
	var info RevInfo
	err = json.NewDecoder(resp.Body).Decode(&info)
	if err != nil {
		return RevInfo{}, err
	}
	return info, nil
}

func attrValue(attrs []xml.Attr, name string) string {
	for _, a := range attrs {
		if strings.EqualFold(a.Name.Local, name) {
			return a.Value
		}
	}
	return ""
}

func checkModuleVcsDirect(modulePath string) ([]MetaImport, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DirectConnectTimeout)
	defer cancel()
	link := fmt.Sprintf("https://%s?go-get=1", modulePath)
	loggerGreen.Printf("VcsDirect: Trying %s"+LOG_RST, modulePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(fmt.Sprintf("HTTP error %d", resp.StatusCode))
	}
	decoder := xml.NewDecoder(resp.Body)
	decoder.Strict = false
	var imports []MetaImport
	for {
		t, err := decoder.RawToken()
		if err != nil {
			if err != io.EOF {
				return nil, err
			}
			break
		}
		if e, ok := t.(xml.StartElement); ok && strings.EqualFold(e.Name.Local, "body") {
			break
		}
		if e, ok := t.(xml.EndElement); ok && strings.EqualFold(e.Name.Local, "head") {
			break
		}
		e, ok := t.(xml.StartElement)
		if !ok || !strings.EqualFold(e.Name.Local, "meta") {
			continue
		}
		if attrValue(e.Attr, "name") != "go-import" {
			continue
		}
		if f := strings.Fields(attrValue(e.Attr, "content")); len(f) == 3 {
			imports = append(imports, MetaImport{
				Prefix:   f[0],
				VCS:      f[1],
				RepoRoot: f[2],
			})
		}
	}
	return imports, nil
}

func searchModuleVcsDirect(modulePath string) (string, []MetaImport, error) {
	for {
		imports, err := checkModuleVcsDirect(modulePath)
		if err == nil {
			return modulePath, imports, nil
		}
		loggerYellow.Printf("VcsDirect: Failed to get %s: %s, continue trying"+LOG_RST, modulePath, err.Error())
		idx := strings.LastIndexByte(modulePath, '/')
		if idx == -1 {
			return "", nil, errors.New("not found")
		}
		modulePath = modulePath[:idx]
	}
}

func getSingleFileFromTar(reader io.ReadCloser, name string, ty byte) ([]byte, error) {
	tr := tar.NewReader(reader)
	hdr, err := tr.Next()
	var data []byte
	if err == nil && hdr.Name == name && hdr.Typeflag == ty {
		data, err = io.ReadAll(tr)
	} else {
		err = errors.New(fmt.Sprintf("file %s not found in tar", name))
	}
	io.Copy(io.Discard, tr)
	return data, err
}

func copySingleFileFromTar(reader io.ReadCloser, writer io.Writer, name string, ty byte) error {
	tr := tar.NewReader(reader)
	hdr, err := tr.Next()
	if err == nil && hdr.Name == name && hdr.Typeflag == ty {
		_, err = io.Copy(writer, tr)
	} else {
		err = errors.New(fmt.Sprintf("file %s not found in tar", name))
	}
	io.Copy(io.Discard, tr)
	return err
}
