package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

func main() {
	address := flag.String("listen", "127.0.0.1:9000", "address for the static browser application")
	root := flag.String("root", "ui/dist", "directory produced by make wasm")
	basePath := flag.String("base-path", "/", "URL path containing the browser application")
	flag.Parse()

	listener, err := net.Listen("tcp", *address)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Handler: newHandlerAt(*root, *basePath), ReadHeaderTimeout: 5 * time.Second}
	fmt.Printf("Starting browser-native Shelley at http://%s%s?runtime=wasm\n", listener.Addr(), normalizeBasePath(*basePath))
	if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func newHandler(root string) http.Handler {
	return newHandlerAt(root, "/")
}

func newHandlerAt(root, basePath string) http.Handler {
	assets := os.DirFS(root)
	files := http.FileServer(http.Dir(root))
	base := normalizeBasePath(basePath)
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requestPath := request.URL.Path
		if base != "/" {
			withoutSlash := strings.TrimSuffix(base, "/")
			if requestPath == withoutSlash {
				http.Redirect(response, request, base, http.StatusMovedPermanently)
				return
			}
			if !strings.HasPrefix(requestPath, base) {
				http.NotFound(response, request)
				return
			}
			requestPath = "/" + strings.TrimPrefix(requestPath, base)
		}
		name := strings.TrimPrefix(path.Clean(requestPath), "/")
		if name == "." || name == "" {
			name = "index.html"
		}
		if _, err := fs.Stat(assets, name); err == nil {
			assetRequest := request.Clone(request.Context())
			assetRequest.URL.Path = requestPath
			files.ServeHTTP(response, assetRequest)
			return
		} else if !errors.Is(err, fs.ErrNotExist) {
			http.Error(response, err.Error(), http.StatusInternalServerError)
			return
		}
		if path.Ext(name) != "" {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeFile(response, request, path.Join(root, "index.html"))
	})
}

func normalizeBasePath(value string) string {
	value = strings.Trim(value, "/")
	if value == "" {
		return "/"
	}
	return "/" + value + "/"
}
