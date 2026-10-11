package main

import (
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

type app struct {
	lastSeen int64
	token    string
	host     string
	scan     *Scanner
}

func main() {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "Could not start the local server:", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	a := &app{
		token: newToken(),
		host:  fmt.Sprintf("127.0.0.1:%d", port),
		scan:  NewScanner(),
	}
	a.touch()

	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleIndex)
	mux.HandleFunc("/api/places", a.api("GET", a.handlePlaces))
	mux.HandleFunc("/api/pick", a.api("POST", a.handlePick))
	mux.HandleFunc("/api/scan", a.api("POST", a.handleScan))
	mux.HandleFunc("/api/cancel", a.api("POST", a.handleCancel))
	mux.HandleFunc("/api/status", a.api("GET", a.handleStatus))
	mux.HandleFunc("/api/results", a.api("GET", a.handleResults))
	mux.HandleFunc("/api/delete", a.api("POST", a.handleDelete))
	mux.HandleFunc("/api/reveal", a.api("POST", a.handleReveal))
	mux.HandleFunc("/api/quit", a.api("POST", a.handleQuit))

	url := fmt.Sprintf("http://%s/?t=%s", a.host, a.token)
	fmt.Println("Disk Space Visualizer is running. If your browser did not open, visit:")
	fmt.Println(url)
	go func() {
		time.Sleep(300 * time.Millisecond)
		openBrowser(url)
	}()
	go a.watchdog()

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (a *app) touch() { atomic.StoreInt64(&a.lastSeen, time.Now().UnixNano()) }

func (a *app) watchdog() {
	for range time.Tick(15 * time.Second) {
		idle := time.Since(time.Unix(0, atomic.LoadInt64(&a.lastSeen)))
		if idle > 10*time.Minute && !a.scan.Running() {
			os.Exit(0)
		}
	}
}

func (a *app) tokenOK(t string) bool {
	return subtle.ConstantTimeCompare([]byte(t), []byte(a.token)) == 1
}

func (a *app) api(method string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Host != a.host || !a.tokenOK(r.Header.Get("X-Token")) {
			writeError(w, http.StatusForbidden, "Forbidden.")
			return
		}
		if r.Method != method {
			writeError(w, http.StatusMethodNotAllowed, "Method not allowed.")
			return
		}
		a.touch()
		w.Header().Set("Cache-Control", "no-store")
		h(w, r)
	}
}

func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Host != a.host || !a.tokenOK(r.URL.Query().Get("t")) {
		http.Error(w, "This link is not valid. Start Disk Space Visualizer again to get a new one.", http.StatusForbidden)
		return
	}
	a.touch()
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Write(indexHTML)
}

func (a *app) handlePlaces(w http.ResponseWriter, r *http.Request) {
	type place struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	places := []place{}
	if home, err := os.UserHomeDir(); err == nil {
		for _, n := range []string{"Downloads", "Documents", "Desktop", "Pictures", "Videos", "Music"} {
			p := filepath.Join(home, n)
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				places = append(places, place{n, p})
			}
		}
	}
	for _, d := range listDrives() {
		places = append(places, place{d, d})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"places":    places,
		"canPick":   canPickFolder,
		"trashName": trashName,
		"excludes":  loadSettings().Excludes,
	})
}

func (a *app) handlePick(w http.ResponseWriter, r *http.Request) {
	path, err := pickFolder()
	a.touch()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": path})
}

func (a *app) handleScan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path     string   `json:"path"`
		Excludes []string `json:"excludes"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	root, err := cleanRoot(req.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	excludes := cleanExcludes(req.Excludes)
	saveSettings(settings{Excludes: excludes})
	if err := a.scan.Start(root, excludes); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"started": true})
}

func (a *app) handleCancel(w http.ResponseWriter, r *http.Request) {
	a.scan.Cancel()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.scan.Status())
}

func (a *app) handleResults(w http.ResponseWriter, r *http.Request) {
	tree, files := a.scan.Results()
	writeJSON(w, http.StatusOK, map[string]interface{}{"tree": tree, "files": files})
}

func (a *app) handleDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths []string `json:"paths"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	results := a.scan.Delete(req.Paths)
	writeJSON(w, http.StatusOK, map[string]interface{}{"results": results})
}

func (a *app) handleReveal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := revealPath(req.Path); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *app) handleQuit(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	go func() {
		time.Sleep(200 * time.Millisecond)
		os.Exit(0)
	}()
}

func cleanRoot(p string) (string, error) {
	p = strings.Trim(strings.TrimSpace(p), `"'`)
	if p == "" {
		return "", errors.New("Enter a folder path first.")
	}
	if len(p) == 2 && p[1] == ':' {
		p += string(os.PathSeparator)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("That path doesn't look right: %v", err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("Can't open %q. Check the path and try again.", p)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%q is a file. Choose a folder.", p)
	}
	return abs, nil
}

type settings struct {
	Excludes []string `json:"excludes"`
}

func settingsPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "diskspace-visualizer", "settings.json"), nil
}

func loadSettings() settings {
	s := settings{Excludes: []string{}}
	p, err := settingsPath()
	if err != nil {
		return s
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return s
	}
	var loaded settings
	if json.Unmarshal(data, &loaded) != nil {
		return s
	}
	s.Excludes = cleanExcludes(loaded.Excludes)
	return s
}

func saveSettings(s settings) {
	p, err := settingsPath()
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(p), 0o700) != nil {
		return
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(p, data, 0o600)
}

func cleanExcludes(lines []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, l := range lines {
		l = strings.Trim(strings.TrimSpace(l), `"'`)
		if l == "" {
			continue
		}
		abs, err := filepath.Abs(l)
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
		key := strings.ToLower(abs)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, abs)
	}
	return out
}

func readJSON(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "The request could not be read.")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
