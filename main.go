//go:build windows

// Steam Auto-Updater
//
// Detects installed Steam games, checks each one against Valve's public
// UpToDateCheck API, and when updates are pending, starts Steam silently in
// the tray so the official client downloads them. Optionally closes Steam
// again once everything is up to date. No Steam credentials are stored.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	checkInterval = 30 * time.Minute
	kickPollEvery = 30 * time.Second
	kickTimeout   = 15 * time.Minute
	runKeyName    = "SteamAutoUpdater"
	apiURL        = "https://api.steampowered.com/ISteamApps/UpToDateCheck/v0001/?appid=%s&version=%s"
)

type game struct {
	AppID   string
	Name    string
	BuildID string
	Size    int64
	Status  string
}

type steamInstall struct {
	ExePath     string
	LibraryDirs []string
}

var (
	mw           *walk.MainWindow
	tv           *walk.TableView
	lblLib       *walk.Label
	chkStart     *walk.CheckBox
	chkClose     *walk.CheckBox
	sbi          *walk.StatusBarItem
	model        = &gameModel{}
	busy         atomic.Bool
	closeWhenDone atomic.Bool
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

func main() {
	runtime.LockOSThread()

	if err := (MainWindow{
		AssignTo: &mw,
		Title:    "Steam Auto-Updater",
		MinSize:  Size{Width: 560, Height: 380},
		Layout:   VBox{},
		Children: []Widget{
			Label{AssignTo: &lblLib, Text: "Locating Steam…"},
			TableView{
				AssignTo:            &tv,
				AlternatingRowBG:    true,
				LastColumnStretched: true,
				Columns: []TableViewColumn{
					{Title: "Game", Width: 300},
					{Title: "Status", Width: 130},
					{Title: "Size", Width: 90},
				},
				Model: model,
			},
			Composite{
				Layout: HBox{Margins: Margins{Left: 10, Top: 6, Right: 10, Bottom: 10}},
				Children: []Widget{
					PushButton{Text: "Check now", OnClicked: func() { go runCycle(true) }},
					CheckBox{AssignTo: &chkStart, Text: "Start with Windows", Checked: startupEnabled(),
						OnClicked: func() { setStartup(chkStart.Checked()) }},
					CheckBox{AssignTo: &chkClose, Text: "Close Steam when done", Checked: true,
						OnClicked: func() { closeWhenDone.Store(chkClose.Checked()) }},
				},
			},
		},
		StatusBarItems: []StatusBarItem{{AssignTo: &sbi, Text: "Starting…", Width: 520}},
	}.Create()); err != nil {
		log.Fatal(err)
	}

	go func() {
		runCycle(true)
		t := time.NewTicker(checkInterval)
		for range t.C {
			runCycle(false)
		}
	}()

	mw.Run()
}

// ui runs f on the UI thread.
func ui(f func()) { mw.Synchronize(f) }

// runCycle scans the libraries, checks every game, and kicks Steam if needed.
func runCycle(manual bool) {
	if !busy.CompareAndSwap(false, true) {
		if manual {
			ui(func() { sbi.SetText("A check is already running.") })
		}
		return
	}
	defer busy.Store(false)

	ui(func() { sbi.SetText("Scanning Steam libraries…") })

	inst, err := findSteam()
	if err != nil {
		ui(func() {
			lblLib.SetText("Steam not found: " + err.Error())
			sbi.SetText("Steam not found")
			model.publish(nil)
		})
		return
	}

	games := readGames(inst)
	for i := range games {
		games[i].Status = "Checking…"
	}
	ui(func() {
		lblLib.SetText(fmt.Sprintf("%s  —  %d games in %d libraries", inst.ExePath, len(games), len(inst.LibraryDirs)))
		model.publish(games)
	})

	stale := 0
	for i := range games {
		games[i].Status = checkStatus(&games[i])
		if games[i].Status == "Update available" {
			stale++
		}
		time.Sleep(120 * time.Millisecond)
	}
	sort.SliceStable(games, func(i, j int) bool { return games[i].Name < games[j].Name })
	checkedAt := time.Now().Format("3:04 PM")
	ui(func() {
		model.publish(games)
		if stale == 0 {
			sbi.SetText(fmt.Sprintf("Checked at %s — all %d games up to date.", checkedAt, len(games)))
		} else {
			sbi.SetText(fmt.Sprintf("Checked at %s — %d of %d games need updates.", checkedAt, stale, len(games)))
		}
	})

	if stale == 0 {
		return
	}

	if steamRunning() {
		ui(func() { sbi.SetText("Steam is already running — it will apply these updates itself.") })
		return
	}

	if err := startSteam(inst); err != nil {
		ui(func() { sbi.SetText("Could not start Steam: " + err.Error()) })
		return
	}
	ui(func() { sbi.SetText(fmt.Sprintf("Steam started in the tray — updating %d game(s)…", stale)) })

	deadline := time.Now().Add(kickTimeout)
	for time.Now().Before(deadline) {
		time.Sleep(kickPollEvery)
		fresh := readGames(inst)
		remaining := 0
		for i := range fresh {
			fresh[i].Status = checkStatus(&fresh[i])
			if fresh[i].Status == "Update available" {
				remaining++
			}
			time.Sleep(120 * time.Millisecond)
		}
		sort.SliceStable(fresh, func(i, j int) bool { return fresh[i].Name < fresh[j].Name })
		ui(func() { model.publish(fresh) })
		if remaining == 0 {
			if closeWhenDone.Load() {
				ui(func() { sbi.SetText("All games up to date — closing Steam…") })
				closeSteam()
			}
			ui(func() { sbi.SetText(fmt.Sprintf("Done at %s — all games up to date.", time.Now().Format("3:04 PM"))) })
			return
		}
		ui(func() { sbi.SetText(fmt.Sprintf("Steam is updating %d game(s)…", remaining)) })
	}
	ui(func() { sbi.SetText("Updates still pending after 15 min — Steam left running.") })
}

// ---------- model ----------

type gameModel struct {
	walk.TableModelBase
	games []game
}

func (m *gameModel) RowCount() int { return len(m.games) }

func (m *gameModel) Value(row, col int) interface{} {
	if row < 0 || row >= len(m.games) {
		return ""
	}
	g := m.games[row]
	switch col {
	case 0:
		return g.Name
	case 1:
		return g.Status
	case 2:
		return humanSize(g.Size)
	}
	return ""
}

func (m *gameModel) publish(gs []game) {
	m.games = gs
	m.PublishRowsReset()
}

// ---------- update check ----------

func checkStatus(g *game) string {
	if strings.TrimSpace(g.BuildID) == "" {
		return "Unknown"
	}
	resp, err := httpClient.Get(fmt.Sprintf(apiURL, g.AppID, g.BuildID))
	if err != nil {
		return "Unknown"
	}
	defer resp.Body.Close()
	var r struct {
		Response struct {
			Success  bool `json:"success"`
			UpToDate bool `json:"up_to_date"`
		} `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil || !r.Response.Success {
		return "Unknown"
	}
	if r.Response.UpToDate {
		return "Up to date"
	}
	return "Update available"
}

// ---------- Steam discovery ----------

func findSteam() (*steamInstall, error) {
	if p, err := regString(registry.CURRENT_USER, `Software\Valve\Steam`, "SteamPath"); err == nil && p != "" {
		return buildInstall(filepath.FromSlash(p))
	}
	if p, err := regString(registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\Valve\Steam`, "InstallPath"); err == nil && p != "" {
		return buildInstall(filepath.FromSlash(p))
	}
	if p, err := regString(registry.LOCAL_MACHINE, `SOFTWARE\Valve\Steam`, "InstallPath"); err == nil && p != "" {
		return buildInstall(filepath.FromSlash(p))
	}
	for _, p := range []string{
		`C:\Program Files (x86)\Steam`,
		`C:\Program Files\Steam`,
	} {
		if fileExists(filepath.Join(p, "steam.exe")) {
			return buildInstall(p)
		}
	}
	return nil, fmt.Errorf("no Steam installation in registry or default locations")
}

func buildInstall(steamPath string) (*steamInstall, error) {
	if !fileExists(filepath.Join(steamPath, "steam.exe")) {
		return nil, fmt.Errorf("steam.exe missing in %s", steamPath)
	}
	exe := filepath.Join(steamPath, "steam.exe")
	if e, err := regString(registry.CURRENT_USER, `Software\Valve\Steam`, "SteamExe"); err == nil && e != "" {
		if fileExists(filepath.FromSlash(e)) {
			exe = filepath.FromSlash(e)
		}
	}
	inst := &steamInstall{ExePath: exe, LibraryDirs: []string{steamPath}}
	seen := map[string]bool{strings.ToLower(steamPath): true}
	if data, err := os.ReadFile(filepath.Join(steamPath, "steamapps", "libraryfolders.vdf")); err == nil {
		for _, lib := range vdfPaths(string(data)) {
			lk := strings.ToLower(lib)
			if seen[lk] || !dirExists(filepath.Join(lib, "steamapps")) {
				continue
			}
			seen[lk] = true
			inst.LibraryDirs = append(inst.LibraryDirs, lib)
		}
	}
	return inst, nil
}

var pathRe = regexp.MustCompile(`"path"\s+"([^"]*)"`)

func vdfPaths(content string) []string {
	var out []string
	for _, mt := range pathRe.FindAllStringSubmatch(content, -1) {
		out = append(out, strings.ReplaceAll(mt[1], `\\`, `\`))
	}
	return out
}

// ---------- manifest parsing ----------

// acfPairs parses key/value pairs out of a Valve VDF text. Keys are
// lowercased. A quoted token immediately followed by "{" is a section
// header and is discarded so flat pairs stay aligned.
func acfPairs(content string) map[string]string {
	m := map[string]string{}
	var key string
	haveKey := false
	i := 0
	for i < len(content) {
		c := content[i]
		switch {
		case c == '"':
			j := i + 1
			var sb strings.Builder
			for j < len(content) && content[j] != '"' {
				if content[j] == '\\' && j+1 < len(content) {
					j++
				}
				sb.WriteByte(content[j])
				j++
			}
			tok := sb.String()
			i = j + 1
			if !haveKey {
				key, haveKey = tok, true
			} else {
				m[strings.ToLower(key)] = tok
				haveKey = false
			}
		case c == '{':
			haveKey = false
			i++
		default:
			i++
		}
	}
	return m
}

func readGames(inst *steamInstall) []game {
	seen := map[string]bool{}
	var games []game
	for _, lib := range inst.LibraryDirs {
		sa := filepath.Join(lib, "steamapps")
		entries, err := os.ReadDir(sa)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), "appmanifest_") || !strings.HasSuffix(e.Name(), ".acf") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(sa, e.Name()))
			if err != nil {
				continue
			}
			m := acfPairs(string(data))
			if m["stateflags"] != "4" {
				continue
			}
			id := m["appid"]
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			name := m["name"]
			if name == "" {
				name = "App " + id
			}
			size, _ := strconv.ParseInt(m["sizeondisk"], 10, 64)
			games = append(games, game{AppID: id, Name: name, BuildID: m["buildid"], Size: size})
		}
	}
	sort.Slice(games, func(i, j int) bool { return games[i].Name < games[j].Name })
	return games
}

// ---------- Steam process control ----------

func steamRunning() bool {
	out, err := runHidden("tasklist", "/FI", "IMAGENAME eq steam.exe", "/FO", "CSV", "/NH")
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(out), `"steam.exe"`)
}

func startSteam(inst *steamInstall) error {
	return exec.Command(inst.ExePath, "-silent").Start()
}

func closeSteam() {
	_, _ = runHidden("taskkill", "/IM", "steam.exe")
	for i := 0; i < 10; i++ {
		time.Sleep(2 * time.Second)
		if !steamRunning() {
			return
		}
	}
	_, _ = runHidden("taskkill", "/F", "/IM", "steam.exe")
}

// runHidden runs a console command without flashing a console window.
func runHidden(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	b, err := cmd.Output()
	return string(b), err
}

// ---------- startup registration ----------

func startupEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(runKeyName)
	return err == nil
}

func setStartup(on bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE)
	if err != nil {
		ui(func() { sbi.SetText("Could not open startup key: " + err.Error()) })
		return
	}
	defer k.Close()
	if on {
		exe, err := os.Executable()
		if err != nil {
			ui(func() { sbi.SetText("Could not resolve exe path: " + err.Error()) })
			return
		}
		if err := k.SetStringValue(runKeyName, `"`+exe+`"`); err != nil {
			ui(func() { sbi.SetText("Could not set startup entry: " + err.Error()) })
		} else {
			ui(func() { sbi.SetText("Will start with Windows.") })
		}
		return
	}
	if err := k.DeleteValue(runKeyName); err != nil {
		ui(func() { sbi.SetText("Startup entry not present.") })
	} else {
		ui(func() { sbi.SetText("Will not start with Windows.") })
	}
}

// ---------- helpers ----------

func regString(root registry.Key, path, value string) (string, error) {
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer k.Close()
	v, _, err := k.GetStringValue(value)
	return v, err
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func humanSize(b int64) string {
	const MB, GB = 1 << 20, 1 << 30
	switch {
	case b >= GB:
		return fmt.Sprintf("%.1f GB", float64(b)/GB)
	case b >= MB:
		return fmt.Sprintf("%d MB", b/MB)
	default:
		return fmt.Sprintf("%d KB", b/1024)
	}
}