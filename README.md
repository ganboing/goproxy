# Simple Caching Goproxy Server

A minimalist caching proxy for Go Modules.

Dual mode:
- Pass-through: Captures all proxy requests to upstream `proxy.golang.org` and starts caching modules in background.
- Cache-only: Serves modules locally stored without the need of internet access. (Ideal for isolated environment)

Efficient space utilization:
- All git-based modules are stored in git bare repos. module.zip files are constructed on-the-fly.

Absolutely minimal third-parth dependencies:
- golang.org/x only

## Installation
```bash
go install github.com/ganboing/goproxy/cmd/proxy@latest
```

## Usage:
```bash
proxy <listen address>[/<prefix>]
```
The cache directories will be constructed in the working directory.

## Example:

- Server side:
  ```bash
  proxy :8080/gomod
  ```

- Client side (Pass-through mode):
  ```bash
  GOPROXY=http://localhost:8080/gomod go build ...
  ```
- Client side (Cache-only mode):
  ```bash
  GOPROXY=http://localhost:8080/gomod/cached-only go build ...
  ```
