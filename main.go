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

type config struct {
	ScheduleOn  bool   `json:"schedule_on"`
	WindowStart string `json:"window_start"` // "01:00", 24h
	WindowEnd   string `json:"window_end"`   // "07:00", 24h
	CloseSteam  bool   `json:"close_steam_when_done"`
}

var (
	mw            *walk.MainWindow
	tv            *walk.TableView
	lblLib        *walk.Label
	chkStart      *walk.CheckBox
	chkClose      *walk.CheckBox
	chkSched      *walk.CheckBox
	edStart       *walk.LineEdit
	edEnd         *walk.LineEdit
	sbi           *walk.StatusBarItem
	model         = &gameModel{}
	cfg           config
	busy          atomic.Bool
	closeWhenDone atomic.Bool
	lastCheck     atomic.Int64
	pendingCount  atomic.Int64
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

func main() {
	runtime.LockOSThread()

	cfg = loadConfig()
	closeWhenDone.Store(cfg.CloseSteam)

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
					PushButton{Text: "Check now", OnClicked: func() { go runCycle(true, true) }},
					CheckBox{AssignTo: &chkStart, Text: "Start with Windows", Checked: startupEnabled(),
						OnClicked: func() { setStartup(chkStart.Checked()) }},
					CheckBox{AssignTo: &chkClose, Text: "Close Steam when done", Checked: cfg.CloseSteam,
						OnClicked: func() {
							closeWhenDone.Store(chkClose.Checked())
							cfg.CloseSteam = chkClose.Checked()
							saveConfig()
						}},
				},
			},
			Composite{
				Layout: HBox{Margins: Margins{Left: 10, Top: 0, Right: 10, Bottom: 10}},
				Children: []Widget{
					CheckBox{AssignTo: &chkSched, Text: "Only update on a schedule", Checked: cfg.ScheduleOn,
						OnClicked: func() {
							cfg.ScheduleOn = chkSched.Checked()
							edStart.SetEnabled(cfg.ScheduleOn)
							edEnd.SetEnabled(cfg.ScheduleOn)
							saveConfig()
						}},
					Label{Text: "Window:"},
					LineEdit{AssignTo: &edStart, Text: cfg.WindowStart, MinSize: Size{Width: 60, Height: 22},
						OnEditingFinished: applyScheduleEdits},
					Label{Text: "to"},
					LineEdit{AssignTo: &edEnd, Text: cfg.WindowEnd, MinSize: Size{Width: 60, Height: 22},
						OnEditingFinished: applyScheduleEdits},
				},
			},
		},
		StatusBarItems: []StatusBarItem{{AssignTo: &sbi, Text: "Starting…", Width: 520}},
	}.Create()); err != nil {
		log.Fatal(err)
	}

	edStart.SetEnabled(cfg.ScheduleOn)
	edEnd.SetEnabled(cfg.ScheduleOn)

	go func() {
		runCycle(false, inWindow(time.Now()))
		prevIn := inWindow(time.Now())
		t := time.NewTicker(time.Minute)
		for range t.C {
			cur := inWindow(time.Now())
			if cur && (!prevIn || time.Since(time.Unix(lastCheck.Load(), 0)) >= checkInterval) {
				go runCycle(false, true)
			}
			if cfg.ScheduleOn && !cur {
				p := pendingCount.Load()
				ui(func() {
					if p == 0 {
						sbi.SetText(fmt.Sprintf("Paused — outside schedule window (%s–%s).", cfg.WindowStart, cfg.WindowEnd))
					} else {
						sbi.SetText(fmt.Sprintf("Paused — %d update(s) waiting for window (%s–%s).", p, cfg.WindowStart, cfg.WindowEnd))
					}
				})
			}
			prevIn = cur
		}
	}()

	mw.Run()
}

// ui runs f on the UI thread.
func ui(f func()) { mw.Synchronize(f) }

// runCycle scans the libraries, checks every game, and kicks Steam if needed.
func runCycle(manual, kickAllowed bool) {
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
	lastCheck.Store(time.Now().Unix())
	pendingCount.Store(int64(stale))

	if stale == 0 {
		return
	}

	if !inWindow(time.Now()) && !kickAllowed {
		ui(func() {
			sbi.SetText(fmt.Sprintf("%d game(s) need updates — waiting for schedule window (%s–%s).", stale, cfg.WindowStart, cfg.WindowEnd))
		})
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

// ---------- schedule ----------

func configPath() string {
	if d, err := os.UserConfigDir(); err == nil && d != "" {
		return filepath.Join(d, "SteamAutoUpdater", "config.json")
	}
	exe, err := os.Executable()
	if err == nil {
		return filepath.Join(filepath.Dir(exe), "config.json")
	}
	return "config.json"
}

func loadConfig() config {
	c := config{WindowStart: "01:00", WindowEnd: "07:00", CloseSteam: true}
	data, err := os.ReadFile(configPath())
	if err != nil {
		return c
	}
	_ = json.Unmarshal(data, &c)
	return c
}

func saveConfig() {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(configPath()), 0o755)
	_ = os.WriteFile(configPath(), data, 0o644)
}

// parseHHMM parses "H:MM" or "HH:MM" (24h) into minutes past midnight.
func parseHHMM(s string) (int, bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, false
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return 0, false
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// inWindow reports whether now falls inside the configured schedule window.
// Windows may wrap midnight (22:00–06:00). With the schedule off it is always
// true; if the config is invalid it is also true (behave as unscheduled).
func inWindow(now time.Time) bool {
	if !cfg.ScheduleOn {
		return true
	}
	start, ok1 := parseHHMM(cfg.WindowStart)
	end, ok2 := parseHHMM(cfg.WindowEnd)
	if !ok1 || !ok2 {
		return true
	}
	if start == end {
		return true
	}
	m := now.Hour()*60 + now.Minute()
	if start < end {
		return m >= start && m < end
	}
	return m >= start || m < end
}

// applyScheduleEdits validates and saves the window fields. UI thread only.
func applyScheduleEdits() {
	start, ok1 := parseHHMM(edStart.Text())
	end, ok2 := parseHHMM(edEnd.Text())
	if !ok1 || !ok2 {
		sbi.SetText("Schedule times must be HH:MM (24h), e.g. 01:00.")
		return
	}
	cfg.WindowStart = fmt.Sprintf("%02d:%02d", start/60, start%60)
	cfg.WindowEnd = fmt.Sprintf("%02d:%02d", end/60, end%60)
	edStart.SetText(cfg.WindowStart)
	edEnd.SetText(cfg.WindowEnd)
	saveConfig()
	sbi.SetText(fmt.Sprintf("Schedule window saved: %s–%s.", cfg.WindowStart, cfg.WindowEnd))
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
