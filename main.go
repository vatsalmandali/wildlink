// WildLink — serverless office messenger for machines on the same network.
// Peers find each other with UDP broadcast; messages and files go peer-to-peer over HTTP.
// The UI is a local web page opened in an Edge "app" window (no browser chrome).
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed ui.html icon_192.png w_240.png wildlink.ico stickers
var uiFS embed.FS

// defaultUpdateURL points at the update manifest (an https URL or a shared-folder
// path like \\SERVER\Share\WildLink\update.json). Bake it in at build time with
// -X main.defaultUpdateURL=...
var defaultUpdateURL = ""

// defaultGiphyKey can be baked in at build time:
//
//	go build -ldflags "-X main.defaultGiphyKey=YOUR_KEY"
var defaultGiphyKey = ""

// hasTray is set when a notification-area icon is available (Windows).
var hasTray = false

const (
	appName    = "WildLink"
	appVersion = "2.1.0"
)

// Ports can be overridden with env vars (useful for testing two copies on one machine).
var (
	httpPort       = envInt("WILDLINK_HTTP_PORT", 47821)
	discoveryPort  = envInt("WILDLINK_UDP_PORT", 47822)
	announceToPort = envInt("WILDLINK_UDP_TARGET", 47822)
)

const (
	announceEvery = 3 * time.Second
	offlineAfter  = 11 * time.Second
	maxHistory    = 2000
)

type Config struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	ManualIPs  []string `json:"manualIPs"`
	StartupSet bool     `json:"startupSet"`
	GiphyKey   string   `json:"giphyKey,omitempty"`
	UpdateURL  string   `json:"updateURL,omitempty"`
}

type Peer struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	IP       string    `json:"ip"`
	Port     int       `json:"port"`
	Online   bool      `json:"online"`
	Version  string    `json:"version,omitempty"`
	LastSeen time.Time `json:"-"`
}

type Message struct {
	ID       string `json:"id"`
	From     string `json:"from"`
	FromName string `json:"fromName"`
	To       string `json:"to"` // "all" or a peer ID
	Text     string `json:"text,omitempty"`
	File     string `json:"file,omitempty"`    // original file name
	Sticker  string `json:"sticker,omitempty"` // built-in sticker ID, or "custom" for a sticker image
	Kind     string `json:"kind,omitempty"`    // "" for a normal message, "delete" for a deletion notice
	Ref      string `json:"ref,omitempty"`     // the message a deletion refers to
	Deleted  bool   `json:"deleted,omitempty"` // shown as "This message was deleted"
	FileSize int64  `json:"fileSize,omitempty"`
	SavedAs  string `json:"savedAs,omitempty"` // receiver only: name in the downloads folder
	TS       int64  `json:"ts"`
}

type announcement struct {
	App     string `json:"app"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	Port    int    `json:"port"`
	Version string `json:"version,omitempty"`
}

// updateInfo describes a newer release found in the update manifest.
type updateInfo struct {
	Version string `json:"version"`
	Notes   string `json:"notes,omitempty"`
	File    string `json:"file,omitempty"` // resolved URL or path for this OS
}

type App struct {
	mu         sync.Mutex
	cfg        Config
	peers      map[string]*Peer
	history    []Message
	seen       map[string]bool
	subs       map[chan []byte]struct{}
	dataDir    string
	dlDir      string
	udp        *net.UDPConn
	background bool
	focusedAt  time.Time // last time the chat window reported itself focused
	uiOpen     bool
	update     *updateInfo
}

var (
	dialer    = &net.Dialer{Timeout: 4 * time.Second}
	quickHTTP = &http.Client{Timeout: 6 * time.Second}
	// Tuned for big files on a LAN: large socket buffers, keep-alives, no compression
	// (photos and videos are already compressed, so gzip would only burn CPU).
	bulkHTTP = &http.Client{Transport: &http.Transport{
		DialContext:         dialer.DialContext,
		WriteBufferSize:     256 << 10,
		ReadBufferSize:      256 << 10,
		DisableCompression:  true,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
	}}

	// copyBuf hands out 1 MB buffers, so moving a gigabyte is ~1,000 syscalls
	// instead of ~32,000 with Go's default 32 KB.
	copyBuf = sync.Pool{New: func() any { b := make([]byte, 1<<20); return &b }}
	uiURL   = fmt.Sprintf("http://127.0.0.1:%d/", httpPort)
)

func main() {
	background := len(os.Args) > 1 && os.Args[1] == "--background"

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", httpPort))
	if err != nil {
		// Something is already on our port. If it's this version (or newer) of
		// WildLink, just show its window. If it's an older build or the old
		// "LAN Chat", ask it to quit and take over.
		if v := runningVersion(); v != "" && v >= appVersion {
			openWindow()
			return
		}
		quick := &http.Client{Timeout: 2 * time.Second}
		if r, e := quick.Post(fmt.Sprintf("http://127.0.0.1:%d/api/quit", httpPort), "application/json", nil); e == nil {
			r.Body.Close()
		}
		for i := 0; i < 20 && err != nil; i++ {
			time.Sleep(250 * time.Millisecond)
			ln, err = net.Listen("tcp", fmt.Sprintf(":%d", httpPort))
		}
		if err != nil {
			showError(fmt.Sprintf("WildLink can't start because port %d is in use by another program.\n\nClose the old LAN Chat or WildLink from Task Manager and try again.", httpPort))
			return
		}
	}
	cleanupLegacy()

	a := &App{
		peers:      map[string]*Peer{},
		seen:       map[string]bool{},
		subs:       map[chan []byte]struct{}{},
		background: background,
	}
	a.setupDirs()
	a.migrateOldData()
	a.loadConfig()
	a.loadHistory()

	a.udp, err = net.ListenUDP("udp4", &net.UDPAddr{Port: discoveryPort})
	if err != nil {
		log.Printf("discovery listen failed: %v", err)
	} else {
		go a.discoveryListener()
	}
	go a.announceLoop()
	go a.expiryLoop()
	go a.updateLoop()
	go a.sweepParts()
	startTray(a)

	mux := http.NewServeMux()
	// Local UI (loopback only)
	mux.HandleFunc("/", localOnly(a.handleUI))
	mux.HandleFunc("/icon.png", localOnly(serveAsset("icon_192.png")))
	mux.HandleFunc("/w.png", localOnly(serveAsset("w_240.png")))
	mux.HandleFunc("/files/", localOnly(a.handleFiles))
	mux.HandleFunc("/api/events", localOnly(a.handleEvents))
	mux.HandleFunc("/api/version", localOnly(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"app": appName, "version": appVersion})
	}))
	mux.HandleFunc("/api/send", localOnly(a.handleSend))
	mux.HandleFunc("/api/sendsticker", localOnly(a.handleSendSticker))
	mux.HandleFunc("/api/sendurl", localOnly(a.handleSendURL))
	mux.HandleFunc("/api/collection", localOnly(a.handleCollection))
	mux.HandleFunc("/api/collection/add", localOnly(a.handleCollectionAdd))
	mux.HandleFunc("/api/collection/save", localOnly(a.handleCollectionSave))
	mux.HandleFunc("/api/collection/remove", localOnly(a.handleCollectionRemove))
	mux.HandleFunc("/api/collection/send", localOnly(a.handleCollectionSend))
	mux.HandleFunc("/api/settings", localOnly(a.handleSettings))
	mux.HandleFunc("/api/delete", localOnly(a.handleDelete))
	mux.HandleFunc("/api/focus", localOnly(a.handleFocus))
	mux.HandleFunc("/api/check", localOnly(a.handleCheck))
	mux.HandleFunc("/api/update/check", localOnly(a.handleUpdateCheck))
	mux.HandleFunc("/api/update/download", localOnly(a.handleUpdateDownload))
	mux.HandleFunc("/collection/", localOnly(a.handleCollectionFile))
	stickerFS, _ := fs.Sub(uiFS, "stickers")
	mux.Handle("/stickers/", localOnlyH(http.StripPrefix("/stickers/", http.FileServer(http.FS(stickerFS)))))
	mux.HandleFunc("/api/sendfile", localOnly(a.handleSendFile))
	mux.HandleFunc("/api/name", localOnly(a.handleName))
	mux.HandleFunc("/api/addpeer", localOnly(a.handleAddPeer))
	mux.HandleFunc("/api/open", localOnly(a.handleOpen))
	mux.HandleFunc("/api/startup", localOnly(a.handleStartup))
	mux.HandleFunc("/api/quit", localOnly(a.handleQuit))
	// Peer-to-peer (reachable from the LAN)
	mux.HandleFunc("/peer/msg", a.handlePeerMsg)
	mux.HandleFunc("/peer/file", a.handlePeerFile)
	mux.HandleFunc("/peer/file/offset", a.handlePeerFileOffset)

	if !background {
		go func() { time.Sleep(300 * time.Millisecond); openWindow() }()
	}
	log.Printf("listening on :%d", httpPort)
	log.Fatal(http.Serve(ln, mux))
}

// ---------- setup & persistence ----------

func (a *App) setupDirs() {
	base, err := os.UserConfigDir()
	if err != nil {
		base = "."
	}
	a.dataDir = filepath.Join(base, appName)
	if d := os.Getenv("WILDLINK_DATA"); d != "" {
		a.dataDir = d
	}
	os.MkdirAll(a.dataDir, 0o755)
	home, _ := os.UserHomeDir()
	a.dlDir = filepath.Join(home, "Downloads", "WildLink Files")
	if d := os.Getenv("WILDLINK_DATA"); d != "" {
		a.dlDir = filepath.Join(d, "files")
	}
	os.MkdirAll(filepath.Join(a.dlDir, "Sent"), 0o755)
	if f, err := os.OpenFile(filepath.Join(a.dataDir, "wildlink.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		log.SetOutput(f)
	}
}

// migrateOldData carries settings, history, stickers and GIFs over from WildChat/LANChat.
func (a *App) migrateOldData() {
	if _, err := os.Stat(filepath.Join(a.dataDir, "config.json")); err == nil {
		return
	}
	base := filepath.Dir(a.dataDir)
	for _, old := range []string{"WildChat", "LANChat"} {
		src := filepath.Join(base, old)
		if _, err := os.Stat(filepath.Join(src, "config.json")); err != nil {
			continue
		}
		for _, item := range []string{"config.json", "history.jsonl", "stickers", "gifs"} {
			copyTree(filepath.Join(src, item), filepath.Join(a.dataDir, item))
		}
		log.Printf("migrated settings from %s", old)
		return
	}
}

func copyTree(src, dst string) {
	info, err := os.Stat(src)
	if err != nil {
		return
	}
	if info.IsDir() {
		os.MkdirAll(dst, 0o755)
		ents, _ := os.ReadDir(src)
		for _, e := range ents {
			copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()))
		}
		return
	}
	in, err := os.Open(src)
	if err != nil {
		return
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return
	}
	defer out.Close()
	io.Copy(out, in)
}

func (a *App) loadConfig() {
	b, err := os.ReadFile(filepath.Join(a.dataDir, "config.json"))
	if err == nil {
		json.Unmarshal(b, &a.cfg)
	}
	if a.cfg.ID == "" {
		a.cfg.ID = randID()
		a.saveConfigLocked()
	}
	// First run of an installed copy: start with Windows by default (can be turned off in the app).
	if !a.cfg.StartupSet && isInstalled() {
		setStartup(true)
		a.cfg.StartupSet = true
		a.saveConfigLocked()
	}
}

func (a *App) saveConfigLocked() {
	b, _ := json.MarshalIndent(a.cfg, "", "  ")
	os.WriteFile(filepath.Join(a.dataDir, "config.json"), b, 0o644)
}

func (a *App) loadHistory() {
	f, err := os.Open(filepath.Join(a.dataDir, "history.jsonl"))
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 4*1024*1024)
	for sc.Scan() {
		var m Message
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m.Kind == "delete" {
			a.seen[m.ID] = true
			for i := range a.history {
				if a.history[i].ID != m.Ref {
					continue
				}
				if m.Text == "me" {
					a.history = append(a.history[:i], a.history[i+1:]...)
				} else {
					h := &a.history[i]
					h.Deleted, h.Text, h.File, h.SavedAs, h.Sticker, h.FileSize = true, "", "", "", "", 0
				}
				break
			}
			continue
		}
		if !a.seen[m.ID] {
			a.seen[m.ID] = true
			a.history = append(a.history, m)
		}
	}
	if len(a.history) > maxHistory {
		a.history = a.history[len(a.history)-maxHistory:]
	}
	// Rebuild known peers from history so past conversations stay listed.
	for _, m := range a.history {
		if m.From != a.cfg.ID {
			a.peers[m.From] = &Peer{ID: m.From, Name: m.FromName}
		}
	}
}

func (a *App) appendHistory(m Message) {
	f, err := os.OpenFile(filepath.Join(a.dataDir, "history.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(m)
	f.Write(append(b, '\n'))
}

// store records a message once, persists it and pushes it to the UI.
func (a *App) store(m Message) bool {
	a.mu.Lock()
	if a.seen[m.ID] {
		a.mu.Unlock()
		return false
	}
	a.seen[m.ID] = true
	a.history = append(a.history, m)
	if len(a.history) > maxHistory {
		a.history = a.history[len(a.history)-maxHistory:]
	}
	a.mu.Unlock()
	a.appendHistory(m)
	a.publish(map[string]any{"type": "msg", "msg": m})

	if m.From != a.cfg.ID && !a.chatFocused() {
		go notifyOS(notificationFor(m))
	}
	return true
}

// ---------- discovery ----------

func broadcastTargets() []net.IP {
	targets := []net.IP{net.IPv4bcast}
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || ifc.Flags&net.FlagBroadcast == 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, ad := range addrs {
			ipn, ok := ad.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || len(ipn.Mask) != 4 && len(ipn.Mask) != 16 {
				continue
			}
			mask := ipn.Mask
			if len(mask) == 16 {
				mask = mask[12:]
			}
			bc := make(net.IP, 4)
			for i := 0; i < 4; i++ {
				bc[i] = ip4[i] | ^mask[i]
			}
			targets = append(targets, bc)
		}
	}
	return targets
}

func (a *App) announce() {
	if a.udp == nil {
		return
	}
	a.mu.Lock()
	if a.cfg.Name == "" { // stay invisible until a name is set
		a.mu.Unlock()
		return
	}
	payload, _ := json.Marshal(announcement{App: appName, ID: a.cfg.ID, Name: a.cfg.Name, Port: httpPort, Version: appVersion})
	unicast := append([]string{}, a.cfg.ManualIPs...)
	for _, p := range a.peers {
		if p.Online && p.IP != "" {
			unicast = append(unicast, p.IP)
		}
	}
	a.mu.Unlock()

	for _, ip := range broadcastTargets() {
		a.udp.WriteToUDP(payload, &net.UDPAddr{IP: ip, Port: announceToPort})
	}
	// Unicast too, so peers on other subnets (added by IP) keep hearing us.
	for _, s := range unicast {
		if ip := net.ParseIP(s); ip != nil {
			a.udp.WriteToUDP(payload, &net.UDPAddr{IP: ip, Port: announceToPort})
		}
	}
}

func (a *App) announceLoop() {
	for {
		a.announce()
		time.Sleep(announceEvery)
	}
}

func (a *App) discoveryListener() {
	buf := make([]byte, 4096)
	for {
		n, src, err := a.udp.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		var an announcement
		if json.Unmarshal(buf[:n], &an) != nil || an.App != appName || an.ID == "" {
			continue
		}
		a.mu.Lock()
		if an.ID == a.cfg.ID {
			a.mu.Unlock()
			continue
		}
		a.mu.Unlock()
		a.touchPeer(an.ID, an.Name, src.IP.String(), an.Port, an.Version)
	}
}

func (a *App) touchPeer(id, name, ip string, port int, version string) {
	a.mu.Lock()
	p, ok := a.peers[id]
	changed := false
	if !ok {
		p = &Peer{ID: id}
		a.peers[id] = p
		changed = true
	}
	// An IP change alone isn't pushed to the UI: a PC on both Wi-Fi and LAN
	// is heard on two addresses and would otherwise flicker every few seconds.
	if p.Name != name || !p.Online {
		changed = true
	}
	p.Name, p.IP, p.Port, p.Online, p.LastSeen = name, ip, port, true, time.Now()
	if version != "" {
		p.Version = version
	}
	a.mu.Unlock()
	if changed {
		a.publishPeers()
	}
}

func (a *App) expiryLoop() {
	for {
		time.Sleep(2 * time.Second)
		changed := false
		a.mu.Lock()
		for _, p := range a.peers {
			if p.Online && time.Since(p.LastSeen) > offlineAfter {
				p.Online = false
				changed = true
			}
		}
		a.mu.Unlock()
		if changed {
			a.publishPeers()
		}
	}
}

func (a *App) peerList() []Peer {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Peer, 0, len(a.peers))
	for _, p := range a.peers {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// ---------- UI events (SSE) ----------

func (a *App) publish(v any) {
	b, _ := json.Marshal(v)
	a.mu.Lock()
	defer a.mu.Unlock()
	for ch := range a.subs {
		select {
		case ch <- b:
		default:
		}
	}
}

func (a *App) publishPeers() { a.publish(map[string]any{"type": "peers", "peers": a.peerList()}) }

func (a *App) meEvent() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return map[string]any{
		"type": "me", "id": a.cfg.ID, "name": a.cfg.Name, "ips": localIPs(),
		"os": runtime.GOOS, "startup": startupEnabled(), "downloads": a.dlDir, "version": appVersion, "giphyKey": a.giphyKeyLocked(), "updateURL": a.cfg.UpdateURL,
		"tray": hasTray,
	}
}

func (a *App) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch := make(chan []byte, 256)

	send := func(b []byte) { fmt.Fprintf(w, "data: %s\n\n", b); fl.Flush() }
	mb, _ := json.Marshal(a.meEvent())
	send(mb)
	pb, _ := json.Marshal(map[string]any{"type": "peers", "peers": a.peerList()})
	send(pb)
	a.mu.Lock()
	hb, _ := json.Marshal(map[string]any{"type": "history", "msgs": a.history})
	a.subs[ch] = struct{}{}
	a.mu.Unlock()
	send(hb)
	a.mu.Lock()
	u := a.update
	a.mu.Unlock()
	if u != nil {
		ub, _ := json.Marshal(map[string]any{"type": "update", "update": u})
		send(ub)
	}

	defer func() { a.mu.Lock(); delete(a.subs, ch); a.mu.Unlock() }()
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-ch:
			send(b)
		case <-tick.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

// ---------- UI actions ----------

func (a *App) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, _ := uiFS.ReadFile("ui.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

func (a *App) targets(to string) ([]Peer, error) {
	if to == "all" {
		var out []Peer
		for _, p := range a.peerList() {
			if p.Online {
				out = append(out, p)
			}
		}
		return out, nil
	}
	a.mu.Lock()
	p, ok := a.peers[to]
	a.mu.Unlock()
	if !ok || !p.Online {
		return nil, errors.New("that person is offline right now")
	}
	return []Peer{*p}, nil
}

func (a *App) newMessage(to string) Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return Message{ID: randID(), From: a.cfg.ID, FromName: a.cfg.Name, To: to, TS: time.Now().UnixMilli()}
}

func (a *App) handleSend(w http.ResponseWriter, r *http.Request) {
	var req struct{ To, Text string }
	if json.NewDecoder(r.Body).Decode(&req) != nil || strings.TrimSpace(req.Text) == "" {
		http.Error(w, "empty message", 400)
		return
	}
	m := a.newMessage(req.To)
	m.Text = req.Text
	code, msg := a.deliver(m)
	reply(w, code, msg)
}

// handleSendSticker sends one of the built-in stickers (tiny: only its ID travels).
func (a *App) handleSendSticker(w http.ResponseWriter, r *http.Request) {
	var req struct{ To, ID string }
	json.NewDecoder(r.Body).Decode(&req)
	label, ok := builtinSticker(req.ID)
	if !ok {
		http.Error(w, "unknown sticker", 400)
		return
	}
	m := a.newMessage(req.To)
	m.Sticker = req.ID
	m.Text = "[" + label + " sticker]" // what older versions display
	code, msg := a.deliver(m)
	reply(w, code, msg)
}

// deliver sends a text/sticker message to its recipients and stores it.
func (a *App) deliver(m Message) (int, string) {
	peers, err := a.targets(m.To)
	if err != nil {
		return 409, err.Error()
	}
	body, _ := json.Marshal(m)
	if m.To == "all" {
		for _, p := range peers {
			go postJSON(p, body)
		}
	} else if err := postJSON(peers[0], body); err != nil {
		return 502, "couldn't reach " + peers[0].Name + ": " + err.Error()
	}
	a.store(m)
	return 204, ""
}

func reply(w http.ResponseWriter, code int, msg string) {
	if code == 204 {
		w.WriteHeader(204)
		return
	}
	http.Error(w, msg, code)
}

func postJSON(p Peer, body []byte) error {
	resp, err := quickHTTP.Post(fmt.Sprintf("http://%s:%d/peer/msg", p.IP, p.Port), "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func (a *App) handleSendFile(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code, msg := a.sendFile(q.Get("to"), q.Get("name"), r.Body, "")
	reply(w, code, msg)
}

// sendFile keeps a copy in "Sent" (so the sender can preview it) and streams it to every recipient.
// sticker is "custom" for a user sticker image, "" for a normal file.
func (a *App) sendFile(to, name string, src io.Reader, sticker string) (int, string) {
	name = cleanName(name)
	peers, err := a.targets(to)
	if err != nil {
		return 409, err.Error()
	}
	sentDir := filepath.Join(a.dlDir, "Sent")
	os.MkdirAll(sentDir, 0o755)
	keep := uniquePath(sentDir, name)
	out, err := os.Create(keep)
	if err != nil {
		return 500, err.Error()
	}
	bw := bufio.NewWriterSize(out, 1<<20)
	size, err := bigCopy(bw, src)
	if ferr := bw.Flush(); err == nil {
		err = ferr
	}
	out.Close()
	if err != nil {
		os.Remove(keep)
		return 500, err.Error()
	}
	m := a.newMessage(to)
	m.File, m.FileSize, m.Sticker = name, size, sticker
	meta, _ := json.Marshal(m)
	m.SavedAs = "Sent/" + filepath.Base(keep)
	metaHdr := base64.StdEncoding.EncodeToString(meta)

	who := "Everyone"
	if to != "all" && len(peers) == 1 {
		who = peers[0].Name
	}

	// One counter per recipient, so a retry that rewinds doesn't inflate the total.
	progress := make([]int64, len(peers))
	read := func() int64 {
		var n int64
		for i := range progress {
			n += atomic.LoadInt64(&progress[i])
		}
		return n
	}
	finish := func(string) {}
	if sticker == "" {
		finish = a.track(transfer{ID: m.ID, Dir: "out", Who: who, Name: name,
			Size: size * int64(len(peers)), FileSize: size}, read)
	}

	var failed []string
	var wg sync.WaitGroup
	var fmu sync.Mutex
	gate := make(chan struct{}, 4) // a few at a time keeps Wi-Fi from thrashing
	for i, p := range peers {
		wg.Add(1)
		go func(i int, p Peer) {
			defer wg.Done()
			gate <- struct{}{}
			defer func() { <-gate }()
			if err := a.sendToPeer(p, keep, size, metaHdr, m.ID, &progress[i]); err != nil {
				log.Printf("send %s to %s: %v", name, p.Name, err)
				fmu.Lock()
				failed = append(failed, p.Name)
				fmu.Unlock()
			}
		}(i, p)
	}
	wg.Wait()
	if len(peers) > 0 && len(failed) == len(peers) {
		finish("failed")
		os.Remove(keep)
		return 502, "couldn't deliver to " + strings.Join(failed, ", ")
	}
	finish("done")
	a.store(m)
	if len(failed) > 0 {
		return 207, "sent, but not to: " + strings.Join(failed, ", ")
	}
	return 204, ""
}

// sendToPeer uploads the file, retrying on network trouble and picking up where it
// left off rather than starting a half-finished transfer again.
func (a *App) sendToPeer(p Peer, path string, size int64, metaHdr, msgID string, progress *int64) error {
	var lastErr error
	var offset int64
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
			if n, err := peerOffset(p, msgID); err == nil && n >= 0 && n <= size {
				offset = n
			} else {
				offset = 0
			}
		}
		atomic.StoreInt64(progress, offset)
		resume, err := a.sendChunk(p, path, offset, size, metaHdr, progress)
		if err == nil {
			return nil
		}
		lastErr = err
		if resume >= 0 && resume <= size { // receiver told us exactly where to continue
			offset = resume
		}
	}
	return lastErr
}

// sendChunk sends everything from offset onwards. The second return value is the
// offset the receiver wants next, or -1 if it didn't say.
func (a *App) sendChunk(p Peer, path string, offset, size int64, metaHdr string, progress *int64) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return -1, err
	}
	defer f.Close()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return -1, err
		}
	}
	req, err := http.NewRequest("POST", fmt.Sprintf("http://%s:%d/peer/file", p.IP, p.Port),
		&countingReader{bufio.NewReaderSize(f, 1<<20), progress})
	if err != nil {
		return -1, err
	}
	req.ContentLength = size - offset
	req.Header.Set("X-WildLink-Meta", metaHdr)
	req.Header.Set("X-WildLink-Offset", fmt.Sprint(offset))
	resp, err := bulkHTTP.Do(req)
	if err != nil {
		return -1, err
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10)) // drain so the connection is reused
		resp.Body.Close()
	}()
	if resp.StatusCode < 300 {
		return -1, nil
	}
	want := int64(-1)
	if h := resp.Header.Get("X-WildLink-Offset"); h != "" {
		fmt.Sscanf(h, "%d", &want)
	}
	return want, fmt.Errorf("status %d", resp.StatusCode)
}

// peerOffset asks a colleague how much of this file they already received.
func peerOffset(p Peer, msgID string) (int64, error) {
	resp, err := quickHTTP.Get(fmt.Sprintf("http://%s:%d/peer/file/offset?id=%s", p.IP, p.Port, url.QueryEscape(msgID)))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var out struct{ Offset int64 }
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<10)).Decode(&out) != nil {
		return 0, fmt.Errorf("bad reply")
	}
	return out.Offset, nil
}

// handleSendURL downloads a GIF picked from GIPHY and sends it as a file,
// so colleagues without internet still see it.
func (a *App) handleSendURL(w http.ResponseWriter, r *http.Request) {
	var req struct{ To, URL, Title string }
	json.NewDecoder(r.Body).Decode(&req)
	u, err := url.Parse(req.URL)
	if err != nil || u.Scheme != "https" || !(u.Hostname() == "giphy.com" || strings.HasSuffix(u.Hostname(), ".giphy.com")) {
		http.Error(w, "only GIPHY links are supported", 400)
		return
	}
	if _, err := a.targets(req.To); err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	c := &http.Client{Timeout: 45 * time.Second}
	resp, err := c.Get(req.URL)
	if err != nil {
		http.Error(w, "couldn't download the GIF. Is this computer online?", 502)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		http.Error(w, fmt.Sprintf("GIPHY returned %d", resp.StatusCode), 502)
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = "GIF"
	}
	if len(title) > 60 {
		title = title[:60]
	}
	code, msg := a.sendFile(req.To, title+".gif", io.LimitReader(resp.Body, 40<<20), "")
	reply(w, code, msg)
}

// ---------- My stickers / My GIFs ----------

func (a *App) collectionDir(kind string) (string, bool) {
	if kind != "stickers" && kind != "gifs" {
		return "", false
	}
	d := filepath.Join(a.dataDir, kind)
	os.MkdirAll(d, 0o755)
	return d, true
}

func (a *App) handleCollection(w http.ResponseWriter, r *http.Request) {
	dir, ok := a.collectionDir(r.URL.Query().Get("kind"))
	if !ok {
		http.Error(w, "bad kind", 400)
		return
	}
	ents, _ := os.ReadDir(dir)
	type item struct {
		Name string `json:"name"`
		t    time.Time
	}
	items := []item{}
	for _, e := range ents {
		if e.IsDir() || !isImageName(e.Name()) {
			continue
		}
		info, _ := e.Info()
		items = append(items, item{Name: e.Name(), t: info.ModTime()})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].t.After(items[j].t) })
	json.NewEncoder(w).Encode(items)
}

func (a *App) handleCollectionAdd(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dir, ok := a.collectionDir(q.Get("kind"))
	name := cleanName(q.Get("name"))
	if !ok || !isImageName(name) {
		http.Error(w, "please choose a PNG, JPG, WebP or GIF image", 400)
		return
	}
	if q.Get("kind") == "gifs" && !strings.HasSuffix(strings.ToLower(name), ".gif") {
		http.Error(w, "My GIFs only takes .gif files", 400)
		return
	}
	dest := uniquePath(dir, name)
	f, err := os.Create(dest)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	_, err = io.Copy(f, io.LimitReader(r.Body, 40<<20))
	f.Close()
	if err != nil {
		os.Remove(dest)
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(204)
}

// handleCollectionSave copies a photo/GIF from the chat into My stickers or My GIFs.
func (a *App) handleCollectionSave(w http.ResponseWriter, r *http.Request) {
	var req struct{ Kind, SavedAs string }
	json.NewDecoder(r.Body).Decode(&req)
	dir, ok := a.collectionDir(req.Kind)
	src, ok2 := a.safeLocal(req.SavedAs)
	if !ok || !ok2 || !isImageName(src) {
		http.Error(w, "can't save that", 400)
		return
	}
	in, err := os.Open(src)
	if err != nil {
		http.Error(w, "file not found. It may have been moved or deleted", 404)
		return
	}
	defer in.Close()
	out, err := os.Create(uniquePath(dir, filepath.Base(src)))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	io.Copy(out, in)
	out.Close()
	w.WriteHeader(204)
}

func (a *App) handleCollectionRemove(w http.ResponseWriter, r *http.Request) {
	var req struct{ Kind, Name string }
	json.NewDecoder(r.Body).Decode(&req)
	dir, ok := a.collectionDir(req.Kind)
	if !ok {
		http.Error(w, "bad kind", 400)
		return
	}
	os.Remove(filepath.Join(dir, filepath.Base(req.Name)))
	w.WriteHeader(204)
}

func (a *App) handleCollectionSend(w http.ResponseWriter, r *http.Request) {
	var req struct{ To, Kind, Name string }
	json.NewDecoder(r.Body).Decode(&req)
	dir, ok := a.collectionDir(req.Kind)
	if !ok {
		http.Error(w, "bad kind", 400)
		return
	}
	f, err := os.Open(filepath.Join(dir, filepath.Base(req.Name)))
	if err != nil {
		http.Error(w, "not found", 404)
		return
	}
	defer f.Close()
	sticker := ""
	if req.Kind == "stickers" {
		sticker = "custom"
	}
	code, msg := a.sendFile(req.To, filepath.Base(req.Name), f, sticker)
	reply(w, code, msg)
}

func (a *App) handleCollectionFile(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/collection/"), "/", 2)
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	dir, ok := a.collectionDir(parts[0])
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeFile(w, r, filepath.Join(dir, filepath.Base(parts[1])))
}

func (a *App) handleSettings(w http.ResponseWriter, r *http.Request) {
	var req struct{ GiphyKey, UpdateURL *string }
	json.NewDecoder(r.Body).Decode(&req)
	a.mu.Lock()
	if req.GiphyKey != nil {
		a.cfg.GiphyKey = strings.TrimSpace(*req.GiphyKey)
	}
	if req.UpdateURL != nil {
		a.cfg.UpdateURL = strings.TrimSpace(*req.UpdateURL)
	}
	a.saveConfigLocked()
	a.mu.Unlock()
	a.publish(a.meEvent())
	go a.checkUpdate()
	w.WriteHeader(204)
}

func (a *App) handleName(w http.ResponseWriter, r *http.Request) {
	var req struct{ Name string }
	json.NewDecoder(r.Body).Decode(&req)
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 40 {
		http.Error(w, "name must be 1–40 characters", 400)
		return
	}
	a.mu.Lock()
	a.cfg.Name = name
	a.saveConfigLocked()
	a.mu.Unlock()
	a.announce()
	a.publish(a.meEvent())
	w.WriteHeader(204)
}

func (a *App) handleAddPeer(w http.ResponseWriter, r *http.Request) {
	var req struct{ IP string }
	json.NewDecoder(r.Body).Decode(&req)
	ip := net.ParseIP(strings.TrimSpace(req.IP))
	if ip == nil || ip.To4() == nil {
		http.Error(w, "enter an IPv4 address like 192.168.1.20", 400)
		return
	}
	a.mu.Lock()
	s := ip.String()
	exists := false
	for _, x := range a.cfg.ManualIPs {
		if x == s {
			exists = true
		}
	}
	if !exists {
		a.cfg.ManualIPs = append(a.cfg.ManualIPs, s)
		a.saveConfigLocked()
	}
	a.mu.Unlock()
	a.announce()
	w.WriteHeader(204)
}

func (a *App) handleOpen(w http.ResponseWriter, r *http.Request) {
	target := a.dlDir
	if n := r.URL.Query().Get("file"); n != "" {
		p, ok := a.safeLocal(n)
		if !ok {
			http.Error(w, "bad file name", 400)
			return
		}
		target = p
		if _, err := os.Stat(target); err != nil {
			http.Error(w, "file not found — it may have been moved or deleted", 404)
			return
		}
	}
	openPath(target)
	w.WriteHeader(204)
}

func (a *App) handleStartup(w http.ResponseWriter, r *http.Request) {
	var req struct{ Enabled bool }
	json.NewDecoder(r.Body).Decode(&req)
	if err := setStartup(req.Enabled); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	a.publish(a.meEvent())
	w.WriteHeader(204)
}

func (a *App) handleQuit(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(204)
	go func() { time.Sleep(200 * time.Millisecond); os.Exit(0) }()
}

// safeLocal resolves a name like "photo.jpg" or "Sent/photo.jpg" inside the files folder.
func (a *App) safeLocal(name string) (string, bool) {
	p := filepath.Join(a.dlDir, filepath.FromSlash(name))
	rel, err := filepath.Rel(a.dlDir, p)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	return p, true
}

// handleFiles streams received/sent files to the UI (supports seeking in videos).
func (a *App) handleFiles(w http.ResponseWriter, r *http.Request) {
	p, ok := a.safeLocal(strings.TrimPrefix(r.URL.Path, "/files/"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		http.Error(w, "file not found — it may have been moved or deleted", 404)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, filepath.Base(p), st.ModTime(), f)
}

func serveAsset(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := uiFS.ReadFile(name)
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "max-age=86400")
		w.Write(b)
	}
}

// ---------- peer endpoints ----------

func (a *App) handlePeerMsg(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	var m Message
	if json.NewDecoder(io.LimitReader(r.Body, 256*1024)).Decode(&m) != nil || m.ID == "" || m.From == "" {
		http.Error(w, "bad message", 400)
		return
	}
	m.SavedAs = ""
	a.noteSender(r, m)
	if m.Kind == "delete" {
		a.mu.Lock()
		var from string
		for i := range a.history {
			if a.history[i].ID == m.Ref {
				from = a.history[i].From
			}
		}
		a.mu.Unlock()
		if from == m.From { // only the original sender may withdraw a message
			m.Text = ""
			a.applyDelete(m)
		}
		w.WriteHeader(204)
		return
	}
	a.store(m)
	w.WriteHeader(204)
}

func (a *App) partPath(destDir, msgID string) string {
	return filepath.Join(destDir, ".wildlink-part-"+cleanName(msgID))
}

// handlePeerFileOffset tells a sender how much of a part-finished file we already have.
func (a *App) handlePeerFileOffset(w http.ResponseWriter, r *http.Request) {
	var n int64
	for _, dir := range []string{a.dlDir, filepath.Join(a.dlDir, "Stickers")} {
		if st, err := os.Stat(a.partPath(dir, r.URL.Query().Get("id"))); err == nil {
			n = st.Size()
			break
		}
	}
	json.NewEncoder(w).Encode(map[string]int64{"offset": n})
}

func (a *App) handlePeerFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	raw, err := base64.StdEncoding.DecodeString(r.Header.Get("X-WildLink-Meta"))
	var m Message
	if err != nil || json.Unmarshal(raw, &m) != nil || m.ID == "" || m.File == "" {
		http.Error(w, "bad file metadata", 400)
		return
	}
	m.File = cleanName(m.File)
	destDir := a.dlDir
	if m.Sticker != "" {
		destDir = filepath.Join(a.dlDir, "Stickers")
	}
	os.MkdirAll(destDir, 0o755)

	total := m.FileSize
	if total <= 0 {
		total = r.ContentLength
	}
	if free := freeSpace(destDir); free > 0 && total > 0 && total+(100<<20) > free {
		http.Error(w, "not enough free disk space for this file", 507)
		return
	}

	part := a.partPath(destDir, m.ID)
	var offset int64
	fmt.Sscanf(r.Header.Get("X-WildLink-Offset"), "%d", &offset)
	have := int64(0)
	if st, err := os.Stat(part); err == nil {
		have = st.Size()
	}
	if offset != have { // out of step: tell the sender exactly where to continue from
		w.Header().Set("X-WildLink-Offset", fmt.Sprint(have))
		http.Error(w, "resume from a different offset", 409)
		return
	}
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	markHidden(part)
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		f.Close()
		http.Error(w, err.Error(), 500)
		return
	}

	got := offset
	finish := func(string) {}
	if m.Sticker == "" {
		finish = a.track(transfer{ID: m.ID, Dir: "in", Who: m.FromName, Name: m.File, Size: total, FileSize: total},
			func() int64 { return atomic.LoadInt64(&got) })
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	n, err := bigCopy(bw, &countingReader{r.Body, &got})
	flushErr := bw.Flush()
	f.Close()
	if err != nil || flushErr != nil {
		// Keep what arrived: the sender picks up from here on its next attempt.
		finish("failed")
		http.Error(w, "transfer interrupted", 500)
		return
	}
	if total > 0 && offset+n < total {
		finish("failed")
		http.Error(w, "transfer incomplete", 500)
		return
	}

	dest := uniquePath(destDir, m.File)
	if err := os.Rename(part, dest); err != nil {
		finish("failed")
		os.Remove(part)
		http.Error(w, err.Error(), 500)
		return
	}
	finish("done")
	rel, _ := filepath.Rel(a.dlDir, dest)
	m.FileSize, m.SavedAs = offset+n, filepath.ToSlash(rel)
	a.noteSender(r, m)
	a.store(m)
	w.WriteHeader(204)
}

// sweepParts clears part-finished transfers that nobody came back for.
func (a *App) sweepParts() {
	for _, dir := range []string{a.dlDir, filepath.Join(a.dlDir, "Stickers")} {
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if !strings.HasPrefix(e.Name(), ".wildlink-part-") {
				continue
			}
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
}

func (a *App) noteSender(r *http.Request, m Message) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	a.mu.Lock()
	p := a.peers[m.From]
	port := httpPort
	if p != nil && p.Port != 0 {
		port = p.Port
	}
	a.mu.Unlock()
	a.touchPeer(m.From, m.FromName, host, port, "")
}

// ---------- deleting messages ----------

// handleDelete removes a message for this person only, or for everyone (sender only).
func (a *App) handleDelete(w http.ResponseWriter, r *http.Request) {
	var req struct{ ID, Scope string }
	json.NewDecoder(r.Body).Decode(&req)
	a.mu.Lock()
	var target Message
	found := false
	for i := range a.history {
		if a.history[i].ID == req.ID {
			target, found = a.history[i], true // copy: the slice moves when messages arrive
		}
	}
	me, name := a.cfg.ID, a.cfg.Name
	a.mu.Unlock()
	if !found {
		http.Error(w, "message not found", 404)
		return
	}
	if req.Scope == "all" {
		if target.From != me {
			http.Error(w, "only the sender can delete a message for everyone", 403)
			return
		}
		notice := Message{ID: randID(), From: me, FromName: name, To: target.To, Kind: "delete", Ref: target.ID, TS: time.Now().UnixMilli()}
		peers, err := a.targets(target.To)
		if err == nil {
			body, _ := json.Marshal(notice)
			for _, p := range peers {
				go postJSON(p, body)
			}
		}
		a.applyDelete(notice)
		w.WriteHeader(204)
		return
	}
	a.applyDelete(Message{ID: randID(), From: me, Kind: "delete", Ref: target.ID, Text: "me", TS: time.Now().UnixMilli()})
	w.WriteHeader(204)
}

// applyDelete records a deletion, persists it and tells the UI.
// Text "me" means hide it here only; anything else is "deleted for everyone".
func (a *App) applyDelete(notice Message) {
	forMe := notice.Text == "me"
	a.mu.Lock()
	found := false
	for i := range a.history {
		m := &a.history[i]
		if m.ID != notice.Ref {
			continue
		}
		found = true
		if forMe {
			a.history = append(a.history[:i], a.history[i+1:]...)
		} else {
			m.Deleted, m.Text, m.File, m.SavedAs, m.Sticker, m.FileSize = true, "", "", "", "", 0
		}
		break
	}
	a.mu.Unlock()
	if !found {
		return
	}
	a.appendHistory(notice)
	a.publish(map[string]any{"type": "deleted", "ref": notice.Ref, "scope": notice.Text})
}

// ---------- window focus & notifications ----------

func (a *App) handleFocus(w http.ResponseWriter, r *http.Request) {
	var req struct{ Focused bool }
	json.NewDecoder(r.Body).Decode(&req)
	a.mu.Lock()
	a.uiOpen = true
	if req.Focused {
		a.focusedAt = time.Now()
	} else {
		a.focusedAt = time.Time{}
	}
	a.mu.Unlock()
	w.WriteHeader(204)
}

// chatFocused reports whether the chat window is open and in front right now.
func (a *App) chatFocused() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.subs) > 0 && !a.focusedAt.IsZero() && time.Since(a.focusedAt) < 90*time.Second
}

func notificationFor(m Message) (string, string) {
	who := m.FromName
	if m.To == "all" {
		who += " (Everyone)"
	}
	switch {
	case m.Kind != "":
		return "", ""
	case m.Sticker != "":
		return who, "Sent a sticker"
	case m.File != "":
		switch kindOfFile(m.File) {
		case "image":
			return who, "Sent a photo"
		case "video":
			return who, "Sent a video"
		}
		return who, "Sent a file: " + m.File
	}
	body := m.Text
	if len(body) > 160 {
		body = body[:160] + "…"
	}
	return who, body
}

func kindOfFile(n string) string {
	switch strings.ToLower(filepath.Ext(n)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp", ".avif":
		return "image"
	case ".mp4", ".webm", ".mov", ".m4v", ".ogv":
		return "video"
	}
	return "file"
}

// ---------- updates ----------

// updateSource is the manifest location: an https URL, or a path to a file on a shared folder.
func (a *App) updateSource() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfg.UpdateURL != "" {
		return a.cfg.UpdateURL
	}
	if defaultUpdateURL != "" {
		return defaultUpdateURL
	}
	// Fallback: a one-line update-source.txt placed next to the program,
	// so IT can point a whole office at one location without rebuilding.
	if exe, err := os.Executable(); err == nil {
		if b, err := os.ReadFile(filepath.Join(filepath.Dir(exe), "update-source.txt")); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

func (a *App) updateLoop() {
	for {
		a.checkUpdate()
		time.Sleep(6 * time.Hour)
	}
}

// githubAPI turns what someone pastes into Settings into a usable URL:
// "github:owner/repo", "owner/repo" or any github.com link to the repo or its
// releases all become the API call for the latest release.
func githubAPI(src string) string {
	t := strings.TrimSpace(src)
	// Anything that is plainly a file path or another site is not a repository.
	if strings.HasPrefix(t, "/") || strings.HasPrefix(t, `\\`) || strings.Contains(t, ":\\") || strings.HasSuffix(strings.ToLower(t), ".json") {
		return ""
	}
	if strings.Contains(t, "://") && !strings.Contains(t, "github.com") {
		return ""
	}
	t = strings.TrimPrefix(strings.TrimPrefix(t, "github:"), "https://")
	t = strings.TrimPrefix(strings.TrimPrefix(t, "http://"), "www.")
	t = strings.TrimPrefix(t, "github.com/")
	if strings.HasPrefix(t, "api.github.com/") {
		return "https://" + t
	}
	parts := strings.Split(strings.Trim(t, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" ||
		strings.Contains(parts[0], ".") || strings.Contains(parts[1], ".") {
		return ""
	}
	return fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", parts[0], parts[1])
}

// parseGitHubRelease reads GitHub's "latest release" reply: the tag is the version
// and the attached files are the installers.
func parseGitHubRelease(raw []byte, goos string) *updateInfo {
	var rel struct {
		TagName string `json:"tag_name"`
		Name    string `json:"name"`
		Body    string `json:"body"`
		Draft   bool   `json:"draft"`
		Assets  []struct {
			File string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if json.Unmarshal(raw, &rel) != nil || rel.TagName == "" || rel.Draft {
		return nil
	}
	u := &updateInfo{Version: strings.TrimPrefix(rel.TagName, "v")}
	notes := strings.TrimSpace(rel.Body)
	if notes == "" {
		notes = strings.TrimSpace(rel.Name)
	}
	if i := strings.IndexAny(notes, "\r\n"); i > 0 {
		notes = strings.TrimSpace(notes[:i])
	}
	if len(notes) > 120 {
		notes = notes[:120] + "…"
	}
	u.Notes = notes
	for _, a := range rel.Assets {
		name := strings.ToLower(a.File)
		switch goos {
		case "darwin":
			if strings.Contains(name, "mac") && strings.HasSuffix(name, ".zip") {
				u.File = a.URL
			}
		case "windows":
			if strings.HasSuffix(name, ".exe") {
				u.File = a.URL
			}
		}
	}
	return u
}

// checkUpdate reads the manifest and, if it names a newer version, tells the UI.
func (a *App) checkUpdate() (*updateInfo, error) {
	src := a.updateSource()
	if src == "" {
		return nil, nil
	}
	if api := githubAPI(src); api != "" {
		src = api
	}
	raw, err := readSource(src)
	if err != nil {
		return nil, err
	}
	var u *updateInfo
	if bytes.Contains(raw, []byte(`"tag_name"`)) { // a GitHub release
		u = parseGitHubRelease(raw, runtime.GOOS)
		if u == nil {
			return nil, fmt.Errorf("no published release found in that repository")
		}
	} else if strings.Contains(src, "api.github.com") {
		return nil, fmt.Errorf("no published release found in that repository")
	} else {
		var man struct {
			Version, Notes, Windows, Mac string
		}
		if err := json.Unmarshal(raw, &man); err != nil {
			return nil, fmt.Errorf("the update file isn't valid JSON")
		}
		file := man.Windows
		if runtime.GOOS == "darwin" {
			file = man.Mac
		}
		u = &updateInfo{Version: man.Version, Notes: man.Notes, File: resolveSibling(src, file)}
	}
	if !newerVersion(u.Version, appVersion) {
		a.mu.Lock()
		a.update = nil
		a.mu.Unlock()
		a.publish(map[string]any{"type": "update", "update": nil})
		return nil, nil
	}
	a.mu.Lock()
	a.update = u
	a.mu.Unlock()
	a.publish(map[string]any{"type": "update", "update": u})
	return u, nil
}

func (a *App) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	u, err := a.checkUpdate()
	if err != nil {
		http.Error(w, "couldn't read the update file: "+err.Error(), 502)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"update": u, "version": appVersion})
}

// handleUpdateDownload fetches the installer and opens it, so the person just clicks through it.
func (a *App) handleUpdateDownload(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	u := a.update
	a.mu.Unlock()
	if u == nil || u.File == "" {
		http.Error(w, "no update file is available for this computer", 404)
		return
	}
	raw, err := readSource(u.File)
	if err != nil {
		http.Error(w, "couldn't download the update: "+err.Error(), 502)
		return
	}
	name := cleanName(filepath.Base(strings.Split(u.File, "?")[0]))
	dest := uniquePath(a.dlDir, name)
	if err := os.WriteFile(dest, raw, 0o755); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	openPath(dest)
	w.WriteHeader(204)
}

// readSource reads an https URL or a file path (including \\server\share paths).
func readSource(src string) ([]byte, error) {
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		c := &http.Client{Timeout: 2 * time.Minute}
		req, err := http.NewRequest("GET", src, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", appName+"/"+appVersion)
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := c.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("server returned %d", resp.StatusCode)
		}
		return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
	}
	return os.ReadFile(src)
}

// resolveSibling turns a bare file name in the manifest into a full URL or path next to it.
func resolveSibling(manifest, file string) string {
	if file == "" {
		return ""
	}
	if strings.HasPrefix(file, "http://") || strings.HasPrefix(file, "https://") || filepath.IsAbs(file) {
		return file
	}
	if i := strings.LastIndexAny(manifest, "/\\"); i >= 0 {
		return manifest[:i+1] + file
	}
	return file
}

// newerVersion compares dotted versions like 2.1.0.
func newerVersion(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 4; i++ {
		var x, y int
		if i < len(pa) {
			fmt.Sscanf(pa[i], "%d", &x)
		}
		if i < len(pb) {
			fmt.Sscanf(pb[i], "%d", &y)
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// ---------- network check ----------

// handleCheck tells the person whether another computer is reachable on the WildLink port.
func (a *App) handleCheck(w http.ResponseWriter, r *http.Request) {
	ip := strings.TrimSpace(r.URL.Query().Get("ip"))
	if net.ParseIP(ip) == nil {
		http.Error(w, "enter an IPv4 address like 192.168.1.20", 400)
		return
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", ip, httpPort), 4*time.Second)
	if err == nil {
		conn.Close()
		json.NewEncoder(w).Encode(map[string]string{"result": "ok",
			"text": ip + " answered on port " + fmt.Sprint(httpPort) + ". Messages and files can reach that computer."})
		return
	}
	msg := err.Error()
	hint := "That computer didn't answer. Check that WildLink is running there."
	switch {
	case strings.Contains(msg, "refused"):
		hint = "That computer is reachable, but nothing is listening. WildLink is probably closed there."
	case strings.Contains(msg, "no route to host"), strings.Contains(msg, "unreachable"):
		hint = "The network blocked the connection. Usually the other computer's firewall, or Wi-Fi client isolation on the router."
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline"):
		hint = "No answer before the timeout. Usually a firewall on that computer, or the two are on different networks."
	}
	json.NewEncoder(w).Encode(map[string]string{"result": "fail", "text": hint, "detail": msg})
}

// ---------- transfer progress ----------

type transfer struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	Dir      string `json:"dir"` // "in" or "out"
	Who      string `json:"who"`
	Name     string `json:"name"`
	Size     int64  `json:"size"` // total bytes to move (file size × recipients)
	FileSize int64  `json:"fileSize"`
	Done     int64  `json:"done"`
	Rate     int64  `json:"rate"`  // bytes per second, smoothed
	ETA      int64  `json:"eta"`   // seconds remaining, -1 when unknown
	State    string `json:"state"` // active, done, failed
}

// bigCopy is io.Copy with a 1 MB buffer.
func bigCopy(dst io.Writer, src io.Reader) (int64, error) {
	b := copyBuf.Get().(*[]byte)
	defer copyBuf.Put(b)
	return io.CopyBuffer(struct{ io.Writer }{dst}, struct{ io.Reader }{src}, *b)
}

type countingReader struct {
	r io.Reader
	n *int64
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	atomic.AddInt64(c.n, int64(n))
	return n, err
}

// track pushes progress for a file transfer to the UI until the returned func is called.
func (a *App) track(t transfer, read func() int64) func(state string) {
	t.Type, t.State, t.ETA = "transfer", "active", -1
	quit := make(chan struct{})
	a.publish(t)
	go func() {
		tk := time.NewTicker(500 * time.Millisecond)
		defer tk.Stop()
		last, lastAt, rate := read(), time.Now(), 0.0
		for {
			select {
			case <-quit:
				return
			case now := <-tk.C:
				n := read()
				if secs := now.Sub(lastAt).Seconds(); secs > 0 {
					sample := float64(n-last) / secs
					if rate == 0 {
						rate = sample
					} else {
						rate = rate*0.6 + sample*0.4 // smooth out bursts
					}
				}
				last, lastAt = n, now
				t.Done, t.Rate, t.ETA = n, int64(rate), -1
				if rate > 1 && t.Size > n {
					t.ETA = int64(float64(t.Size-n) / rate)
				}
				a.publish(t)
			}
		}
	}()
	return func(state string) {
		close(quit)
		t.Done, t.State, t.ETA = read(), state, 0
		a.publish(t)
	}
}

func runningVersion() string {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/version", httpPort))
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var v struct{ App, Version string }
	if json.NewDecoder(resp.Body).Decode(&v) != nil || v.App != appName {
		return ""
	}
	return v.Version
}

func localOnlyH(h http.Handler) http.HandlerFunc { return localOnly(h.ServeHTTP) }

func (a *App) giphyKeyLocked() string {
	if a.cfg.GiphyKey != "" {
		return a.cfg.GiphyKey
	}
	return defaultGiphyKey
}

var stickerIndex = func() map[string]string {
	out := map[string]string{}
	b, _ := uiFS.ReadFile("stickers/index.json")
	var list []struct{ ID, Label string }
	json.Unmarshal(b, &list)
	for _, s := range list {
		out[s.ID] = s.Label
	}
	return out
}()

func builtinSticker(id string) (string, bool) {
	l, ok := stickerIndex[id]
	return l, ok
}

func isImageName(n string) bool {
	switch strings.ToLower(filepath.Ext(n)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return true
	}
	return false
}

// cleanName makes a file name from any OS safe to save on this one
// (e.g. "Screenshot 10:30.png" from a Mac is invalid on Windows).
func cleanName(n string) string {
	n = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, n)
	n = strings.TrimRight(strings.TrimSpace(n), ". ")
	n = strings.TrimLeft(n, ".")
	if n == "" {
		n = "file"
	}
	if len(n) > 150 {
		ext := filepath.Ext(n)
		if len(ext) > 10 {
			ext = ""
		}
		n = n[:150-len(ext)] + ext
	}
	switch strings.ToUpper(strings.TrimSuffix(n, filepath.Ext(n))) {
	case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "LPT1", "LPT2":
		n = "_" + n
	}
	return n
}

func localOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			http.Error(w, "forbidden", 403)
			return
		}
		h(w, r)
	}
}

func envInt(k string, def int) int {
	var n int
	if _, err := fmt.Sscanf(os.Getenv(k), "%d", &n); err == nil && n > 0 {
		return n
	}
	return def
}

func randID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func uniquePath(dir, name string) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	p := filepath.Join(dir, name)
	for i := 1; ; i++ {
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			return p
		}
		p = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
	}
}

func localIPs() []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, ad := range addrs {
		if ipn, ok := ad.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil && !ipn.IP.IsLinkLocalUnicast() {
			out = append(out, ipn.IP.String())
		}
	}
	return out
}
